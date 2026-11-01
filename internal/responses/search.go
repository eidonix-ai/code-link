package responses

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"

	"codelink/internal/apierr"
	"codelink/internal/hooks"
	"codelink/internal/jsonx"
)

// Codex's web_search tool on a chat model (docs/DESIGN.md §4.5): the model
// gets a web_search(query) function; on a call the engine searches, shows
// the client a web_search_call item, hands the results back and generates
// again, until the model stops searching or max_uses is reached.

const (
	webSearchFunction = "web_search"
	defaultMaxUses    = 5
	snippetMax        = 1500
	hitsPerSearch     = 5
)

// Search error codes (Anthropic's web_search_tool_result_error codes).
const (
	searchUnavailable     = "unavailable"
	searchTooManyRequests = "too_many_requests"
	searchMaxUsesExceeded = "max_uses_exceeded"
	searchInvalidInput    = "invalid_tool_input"
)

type webSearchTool struct {
	maxUses        int
	allowedDomains []string
	blockedDomains []string
}

// function is the chat tool the model is offered.
func (webSearchTool) function() map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{
		"name":        webSearchFunction,
		"description": "Search the web for current information. Returns the top results with title, URL and a snippet. Use it for anything that may have changed or that you do not know; you can search again with a better query.",
		"parameters": map[string]any{"type": "object",
			"properties": map[string]any{"query": map[string]any{"type": "string", "description": "The search query"}},
			"required":   []any{"query"}},
	}}
}

// findSearchTool is Codex's web_search tool as the engine's search loop. A
// function tool of the same name the client defined wins.
func findSearchTool(stripped []any, reg *registry) *webSearchTool {
	if _, ok := reg.resolve(webSearchFunction); ok {
		return nil
	}
	for _, t := range stripped {
		if strings.HasPrefix(jsonx.Type(t), "web_search") {
			var allowed []string
			for _, d := range jsonx.Arr(jsonx.Get(t, "filters", "allowed_domains")) {
				if s, ok := d.(string); ok {
					allowed = append(allowed, s)
				}
			}
			return &webSearchTool{maxUses: defaultMaxUses, allowedDomains: allowed}
		}
	}
	return nil
}

// searchOutcome is one search the model asked for, answered.
type searchOutcome struct {
	callID    string // the model's own id for the call
	query     string
	hits      []hooks.SearchHit
	errorCode string
}

// modelText is what the model is told.
func (o searchOutcome) modelText() string {
	if o.errorCode != "" {
		return searchFailedText(o.errorCode)
	}
	return searchHitsText(o.query, o.hits)
}

func searchFailedText(code string) string {
	switch code {
	case searchMaxUsesExceeded:
		return "Web search was not run: the search limit for this request was reached. Answer from what you already have."
	case searchTooManyRequests:
		return "Web search failed: too many searches right now. Answer without it and say that you could not search."
	case searchInvalidInput:
		return "Web search was not run: the call needs a non-empty \"query\" string."
	}
	return "Web search is unavailable. Answer without it and say that you could not search."
}

func searchHitsText(query string, hits []hooks.SearchHit) string {
	s := ""
	if query != "" {
		s = "Web search results for \"" + query + "\":\n"
	}
	if len(hits) == 0 {
		return s + "No results found."
	}
	for i, h := range hits {
		s += "\n" + strconv.Itoa(i+1) + ". " + h.Title + "\n   " + h.URL + "\n"
		if h.PageAge != "" {
			s += "   Published: " + h.PageAge + "\n"
		}
		if h.Snippet != "" {
			s += "   " + clipSnippet(h.Snippet) + "\n"
		}
	}
	return strings.TrimSpace(s)
}

func clipSnippet(s string) string {
	if c, cut := apierr.Clip(s, snippetMax); cut {
		return c + "…"
	}
	return s
}

// searchRunner runs the searches of one request, counting them against
// max_uses.
type searchRunner struct {
	tool     webSearchTool
	searcher hooks.WebSearcher
	log      hooks.EventLog
	searched int
}

// answer answers the model's web_search calls. Which calls run is decided
// in order (the first maxUses run); the searches that run go out
// concurrently, and the outcomes keep the calls' order.
func (r *searchRunner) answer(ctx context.Context, calls []searchCall) []searchOutcome {
	out := make([]searchOutcome, len(calls))
	var run []int
	for i, c := range calls {
		var query string
		if j, err := jsonx.Parse([]byte(c.args)); err == nil {
			query = strings.TrimSpace(jsonx.Str(jsonx.Get(j, "query")))
		}
		out[i] = searchOutcome{callID: c.id, query: query}
		switch {
		case query == "":
			out[i].errorCode = searchInvalidInput
		case r.searched >= r.tool.maxUses:
			out[i].errorCode = searchMaxUsesExceeded
		case r.searcher != nil:
			r.searched++
			run = append(run, i)
		default:
			r.searched++
			out[i].errorCode = searchUnavailable
		}
	}
	var wg sync.WaitGroup
	for _, i := range run {
		wg.Add(1)
		go func(o *searchOutcome) {
			defer wg.Done()
			hits, err := r.searcher.Search(ctx, o.query, r.tool.allowedDomains, r.tool.blockedDomains)
			switch {
			case err == nil:
				if len(hits) > hitsPerSearch {
					hits = hits[:hitsPerSearch]
				}
				o.hits = hits
			case errors.Is(err, hooks.ErrSearchTooManyRequests):
				o.errorCode = searchTooManyRequests
			default:
				o.errorCode = searchUnavailable
			}
		}(&out[i])
	}
	wg.Wait()
	for _, i := range run {
		if out[i].errorCode == "" {
			r.log.Record(hooks.Event{Kind: "websearch", Query: out[i].query, Results: len(out[i].hits)})
		}
	}
	return out
}

// searchRoundRequest is the Chat Completions request of a search round:
// the last round has no tools so the model must answer, and later rounds
// don't force a tool call every round.
func searchRoundRequest(base map[string]any, round, maxRounds int, messages []any) map[string]any {
	r := make(map[string]any, len(base))
	for k, v := range base {
		r[k] = v
	}
	r["messages"] = messages
	if round >= maxRounds-1 {
		delete(r, "tools")
		delete(r, "tool_choice")
		delete(r, "parallel_tool_calls")
	} else if tc, ok := r["tool_choice"]; round > 0 && ok && tc != "none" {
		r["tool_choice"] = "auto"
	}
	return r
}

// functionCall is a chat tool call with input as its JSON arguments.
func functionCall(id, name string, input any) map[string]any {
	args := "{}"
	if input != nil {
		args = jsonx.String(input)
	}
	return map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args}}
}

// searchArgs parses a search call's arguments ({} when they aren't JSON).
func searchArgs(args string) any {
	if args == "" {
		args = "{}"
	}
	if j, err := jsonx.Parse([]byte(args)); err == nil {
		return j
	}
	return map[string]any{}
}
