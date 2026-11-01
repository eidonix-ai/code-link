package messages

import (
	"context"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"sync"

	"codelink/internal/apierr"
	"codelink/internal/hooks"
	"codelink/internal/httpx"
	"codelink/internal/ids"
	"codelink/internal/jsonx"
	"codelink/internal/upstream"
)

// Anthropic's web_search_tool_result_error codes.
const (
	searchUnavailable     = "unavailable"
	searchTooManyRequests = "too_many_requests"
	searchMaxUsesExceeded = "max_uses_exceeded"
	searchInvalidInput    = "invalid_tool_input"
)

const (
	snippetMax    = 1500
	hitsPerSearch = 5
)

// searchFailedText is what the model is told when a search didn't run.
func searchFailedText(code string) string {
	switch code {
	case searchMaxUsesExceeded:
		return "Web search was not run: the search limit for this request was reached. Answer from what you already have."
	case searchTooManyRequests:
		return "Web search failed: too many searches right now. Answer without it and say that you could not search."
	case searchInvalidInput:
		return `Web search was not run: the call needs a non-empty "query" string.`
	}
	return "Web search is unavailable. Answer without it and say that you could not search."
}

// searchHitsText is the model-facing text of a search's hits.
func searchHitsText(query string, hits []hooks.SearchHit) string {
	var s strings.Builder
	if query != "" {
		s.WriteString(`Web search results for "` + query + "\":\n")
	}
	if len(hits) == 0 {
		return s.String() + "No results found."
	}
	for i, h := range hits {
		s.WriteString("\n" + strconv.Itoa(i+1) + ". " + h.Title + "\n   " + h.URL + "\n")
		if h.PageAge != "" {
			s.WriteString("   Published: " + h.PageAge + "\n")
		}
		if h.Snippet != "" {
			s.WriteString("   " + clipSnippet(h.Snippet) + "\n")
		}
	}
	return strings.TrimSpace(s.String())
}

func clipSnippet(s string) string {
	if c, cut := apierr.Clip(s, snippetMax); cut {
		return c + "…"
	}
	return s
}

// historicSearchText is the tool result for a web_search_tool_result block
// from an earlier turn: the hits as they were shown (the snippet rides in
// encrypted_content, base64) or the error.
func historicSearchText(content any) string {
	if jsonx.Type(content) == "web_search_tool_result_error" {
		code := jsonx.Str(jsonx.Get(content, "error_code"))
		if code == "" {
			code = searchUnavailable
		}
		return searchFailedText(code)
	}
	var hits []hooks.SearchHit
	for _, r := range jsonx.Arr(content) {
		title := jsonx.Str(jsonx.Get(r, "title"))
		snippet := ""
		if enc, ok := jsonx.StrOK(jsonx.Get(r, "encrypted_content")); ok {
			if d, err := base64.StdEncoding.DecodeString(enc); err == nil && string(d) != title {
				snippet = strings.ToValidUTF8(string(d), "�")
			}
		}
		hits = append(hits, hooks.SearchHit{Title: title, URL: jsonx.Str(jsonx.Get(r, "url")), Snippet: snippet,
			PageAge: jsonx.Str(jsonx.Get(r, "page_age"))})
	}
	return searchHitsText("", hits)
}

// searchOutcome is one search the model asked for, answered.
type searchOutcome struct {
	id        string // srvtoolu_…: the id the client sees
	callID    string // the model's own id for the call
	query     string
	hits      []hooks.SearchHit
	errorCode string // "" = it ran
}

// resultContent is the web_search_tool_result content.
func (o searchOutcome) resultContent() any {
	if o.errorCode != "" {
		return obj{"type": "web_search_tool_result_error", "error_code": o.errorCode}
	}
	out := []any{}
	for _, h := range o.hits {
		snippet := h.Snippet
		if snippet == "" {
			snippet = h.Title
		}
		r := obj{"type": "web_search_result", "url": h.URL, "title": h.Title,
			"encrypted_content": base64.StdEncoding.EncodeToString([]byte(clipSnippet(snippet)))}
		if h.PageAge != "" {
			r["page_age"] = h.PageAge
		}
		out = append(out, r)
	}
	return out
}

// blocks are the two content blocks of a non-streaming message.
func (o searchOutcome) blocks() []any {
	return []any{
		obj{"type": "server_tool_use", "id": o.id, "name": webSearchFunction, "input": obj{"query": o.query}},
		obj{"type": "web_search_tool_result", "tool_use_id": o.id, "content": o.resultContent()},
	}
}

func (o searchOutcome) modelText() string {
	if o.errorCode != "" {
		return searchFailedText(o.errorCode)
	}
	return searchHitsText(o.query, o.hits)
}

// searchCall is a web_search call the model made.
type searchCall struct{ id, args string }

// searchRunner runs the searches of one request, counting them against
// max_uses.
type searchRunner struct {
	tool     *webSearchTool
	searcher hooks.WebSearcher
	log      hooks.EventLog
	searched int
	// succeeded counts searches that returned results (usage.server_tool_use).
	succeeded int
}

// answer answers the model's web_search calls. Which calls run is decided
// in order (the first max_uses run); the searches that run go out
// concurrently, and the outcomes keep the calls' order.
func (r *searchRunner) answer(ctx context.Context, calls []searchCall) []searchOutcome {
	out := make([]searchOutcome, len(calls))
	var run []int
	for i, c := range calls {
		query := strings.TrimSpace(jsonx.Str(jsonx.Get(jsonx.ParseObject([]byte(c.args)), "query")))
		out[i] = searchOutcome{id: ids.New("srvtoolu_"), callID: c.id, query: query}
		switch {
		case query == "":
			out[i].errorCode = searchInvalidInput
		case r.searched >= r.tool.maxUses:
			out[i].errorCode = searchMaxUsesExceeded
		case r.searcher == nil:
			r.searched++
			out[i].errorCode = searchUnavailable
		default:
			r.searched++
			run = append(run, i)
		}
	}
	var wg sync.WaitGroup
	for _, i := range run {
		wg.Add(1)
		go func(o *searchOutcome) {
			defer wg.Done()
			hits, err := r.searcher.Search(ctx, o.query, r.tool.allowedDomains, r.tool.blockedDomains)
			switch {
			case errors.Is(err, hooks.ErrSearchTooManyRequests):
				o.errorCode = searchTooManyRequests
			case err != nil:
				o.errorCode = searchUnavailable
			default:
				if len(hits) > hitsPerSearch {
					hits = hits[:hitsPerSearch]
				}
				o.hits = hits
			}
		}(&out[i])
	}
	wg.Wait()
	for _, i := range run {
		if out[i].errorCode == "" {
			r.succeeded++
			r.log.Record(hooks.Event{Kind: "websearch", Query: out[i].query, Results: len(out[i].hits)})
		}
	}
	return out
}

// searchLoop is Claude Code's WebSearch on a chat model (docs/DESIGN.md
// §4.5). Claude Code runs WebSearch as a separate request whose only tool
// is the server tool web_search_20250305; on the real API the server
// searches and the answer carries server_tool_use + web_search_tool_result
// blocks. Here the model gets a web_search(query) function, and when it
// calls it the engine searches, shows the client the pair, hands the
// results back to the model and generates again, until the model stops
// searching or max_uses is reached. A search that can't run is a
// web_search_tool_result_error the model sees too; the request never fails
// because of it.
type searchLoop struct {
	c    *call
	tool *webSearchTool
}

// maxRounds: every searched round uses a search; the extra rounds let the
// model be told the limit was reached, and the last round has no tools so
// it must answer.
func (l searchLoop) maxRounds() int { return l.tool.maxUses + 2 }

// roundRequest is the Chat Completions request of a round.
func roundRequest(base obj, round, maxRounds int, messages []any) obj {
	r := make(obj, len(base))
	for k, v := range base {
		r[k] = v
	}
	r["messages"] = messages
	if round >= maxRounds-1 {
		delete(r, "tools")
		delete(r, "tool_choice")
		delete(r, "parallel_tool_calls")
	} else if tc, ok := r["tool_choice"]; round > 0 && ok && jsonx.Str(tc) != "none" {
		r["tool_choice"] = "auto" // a forced call would be forced every round
	}
	return r
}

func (l searchLoop) request(round int, messages []any) obj {
	return roundRequest(l.c.chatReq, round, l.maxRounds(), messages)
}

func (l searchLoop) run() {
	c := l.c
	baseMessages := jsonx.Arr(c.chatReq["messages"])
	first, e := c.send(l.request(0, baseMessages), c.stream)
	if e != nil {
		c.fail(*e)
		return
	}
	runner := &searchRunner{tool: l.tool, searcher: c.env.Hooks.WebSearch, log: c.env.Log}
	if !c.stream || !first.IsEventStream() {
		l.blocking(first, runner)
		return
	}
	s := httpx.StartStream(c.w, 200, nil, c.env.Tuning.PingInterval, false)
	defer s.Close()
	l.streaming(first, runner, s)
}

func (l searchLoop) streaming(resp *upstream.Response, runner *searchRunner, s *httpx.Stream) {
	c := l.c
	t := newStreamTranslator(s, c.requested, c.stopSequences, true)
	t.start()
	messages := append([]any(nil), jsonx.Arr(c.chatReq["messages"])...)
	defer func() { c.log(t.status()) }()
	for round := range l.maxRounds() {
		if round > 0 {
			t.nextRound()
			r, e := c.send(l.request(round, messages), true)
			if e != nil {
				t.fail(*e)
				return
			}
			resp = r
		}
		complete := pump(resp, t)
		if t.done {
			return
		}
		if !complete {
			t.fail(apierr.Truncated)
			return
		}
		t.finishRound()
		if len(t.held) == 0 {
			t.finalize()
			return
		}

		s.Ping() // the searches take a moment
		calls := make([]searchCall, len(t.held))
		for i, h := range t.held {
			calls[i] = searchCall{h.callID, h.args}
		}
		outcomes := runner.answer(c.ctx, calls)
		if c.ctx.Err() != nil {
			return
		}
		for _, o := range outcomes {
			t.serverSearch(o)
		}
		if len(t.toolOrder) > 0 || round >= l.maxRounds()-1 {
			// The client answers its own tool calls; its next request
			// replays the searches from history.
			t.finalize()
			return
		}
		assistant := obj{"role": "assistant", "content": orNull(t.roundText.String())}
		if r := t.roundReason.String(); r != "" {
			assistant["reasoning_content"] = r
			assistant["reasoning"] = r
		}
		var tcs []any
		for _, h := range t.held {
			args := h.args
			if args == "" {
				args = "{}"
			}
			input, err := jsonx.Parse([]byte(args))
			if err != nil {
				input = obj{}
			}
			tcs = append(tcs, functionCall(h.callID, webSearchFunction, input))
		}
		assistant["tool_calls"] = tcs
		messages = append(messages, assistant)
		for _, o := range outcomes {
			messages = append(messages, obj{"role": "tool", "tool_call_id": o.callID, "content": o.modelText()})
		}
	}
	t.finalize()
}

func (l searchLoop) blocking(resp *upstream.Response, runner *searchRunner) {
	c := l.c
	messages := append([]any(nil), jsonx.Arr(c.chatReq["messages"])...)
	blocks := []any{}
	var usage chatUsage
	var last obj
	for round := range l.maxRounds() {
		if round > 0 {
			r, e := c.send(l.request(round, messages), false)
			if e != nil {
				c.fail(*e)
				return
			}
			resp = r
		}
		data, err := resp.Collect(0)
		var chat any
		if err == nil {
			chat, err = jsonx.Parse(data)
		}
		msg := translateMessage(chat, c.requested, c.stopSequences)
		if err != nil || msg == nil {
			c.fail(apierr.API(502, "the backend returned an invalid response"))
			return
		}
		u, _ := parseUsage(jsonx.Get(chat, "usage"))
		usage.add(u)
		last = msg
		var calls []searchCall
		regular := 0
		for _, b := range jsonx.Arr(msg["content"]) {
			if jsonx.Type(b) == "tool_use" {
				if jsonx.Str(jsonx.Get(b, "name")) == webSearchFunction {
					input := jsonx.Get(b, "input")
					if input == nil {
						input = obj{}
					}
					calls = append(calls, searchCall{jsonx.Str(jsonx.Get(b, "id")), jsonx.String(input)})
					continue
				}
				regular++
			}
			blocks = append(blocks, b)
		}
		var outcomes []searchOutcome
		if len(calls) > 0 {
			outcomes = runner.answer(c.ctx, calls)
			for _, o := range outcomes {
				blocks = append(blocks, o.blocks()...)
			}
		}
		if len(calls) == 0 || regular > 0 || round >= l.maxRounds()-1 || c.ctx.Err() != nil {
			break
		}
		assistant := obj{"role": "assistant"}
		for k, v := range jsonx.Obj(jsonx.Get(chat, "choices", 0, "message")) {
			assistant[k] = v
		}
		assistant["role"] = "assistant"
		if _, ok := assistant["content"]; !ok {
			assistant["content"] = nil
		}
		messages = append(messages, assistant)
		for _, o := range outcomes {
			messages = append(messages, obj{"role": "tool", "tool_call_id": o.callID, "content": o.modelText()})
		}
	}
	hasClientCalls := false
	for _, b := range blocks {
		if jsonx.Type(b) == "tool_use" {
			hasClientCalls = true
		}
	}
	stop := jsonx.Str(last["stop_reason"])
	if stop == "" {
		stop = "end_turn"
	}
	if hasClientCalls {
		stop = "tool_use"
	} else if stop == "tool_use" {
		stop = "end_turn"
	}
	msg := obj{}
	for k, v := range last {
		msg[k] = v
	}
	msg["content"] = blocks
	msg["stop_reason"] = stop
	if stop != "stop_sequence" {
		msg["stop_sequence"] = nil
	}
	msg["usage"] = usage.anthropic(runner.succeeded)
	c.log(200)
	c.reply(msg)
}
