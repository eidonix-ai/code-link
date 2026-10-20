// Package websearch is the web search providers behind Claude Code's
// WebSearch tool (docs/DESIGN.md §4.5): Tavily and Brave, about five
// results per search. Errors are hooks.ErrSearchUnavailable (no key, a bad
// key, a failure) or hooks.ErrSearchTooManyRequests (HTTP 429).
//
// Port of the Swift Features/WebSearch/WebSearchers.swift.
package websearch

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"codelink/internal/hooks"
	"codelink/internal/jsonx"
)

// ResultCount is the number of results asked for.
const ResultCount = 5

const (
	TavilyEndpoint = "https://api.tavily.com/search"
	BraveEndpoint  = "https://api.search.brave.com/res/v1/web/search"
	timeout        = 30 * time.Second
	maxResponse    = 8 << 20
)

// Unavailable answers every search with ErrSearchUnavailable (search off,
// or no key).
type Unavailable struct{}

func (Unavailable) Search(context.Context, string, []string, []string) ([]hooks.SearchHit, error) {
	return nil, hooks.ErrSearchUnavailable
}

// Tavily: POST https://api.tavily.com/search.
type Tavily struct {
	Key      string
	Client   *http.Client // nil = http.DefaultClient
	Endpoint string       // "" = TavilyEndpoint
}

// Brave: GET https://api.search.brave.com/res/v1/web/search?q=…&count=5.
type Brave struct {
	Key      string
	Client   *http.Client
	Endpoint string // "" = BraveEndpoint
}

var (
	_ hooks.WebSearcher = Unavailable{}
	_ hooks.WebSearcher = (*Tavily)(nil)
	_ hooks.WebSearcher = (*Brave)(nil)
)

func send(ctx context.Context, client *http.Client, req *http.Request) (any, error) {
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return nil, hooks.ErrSearchUnavailable
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, hooks.ErrSearchTooManyRequests
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, hooks.ErrSearchUnavailable // 401/403 (bad key) and everything else
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return nil, hooks.ErrSearchUnavailable
	}
	j, err := jsonx.Parse(data)
	if err != nil {
		return nil, hooks.ErrSearchUnavailable
	}
	return j, nil
}

func cleanDomains(d []string) []string {
	var out []string
	for _, s := range d {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func strings2any(s []string) []any {
	out := make([]any, len(s))
	for i, x := range s {
		out[i] = x
	}
	return out
}

func (t *Tavily) Search(ctx context.Context, query string, allowed, blocked []string) ([]hooks.SearchHit, error) {
	if t.Key == "" {
		return nil, hooks.ErrSearchUnavailable
	}
	body := map[string]any{"api_key": t.Key, "query": query, "max_results": ResultCount}
	if inc := cleanDomains(allowed); len(inc) > 0 {
		body["include_domains"] = strings2any(inc)
	}
	if exc := cleanDomains(blocked); len(exc) > 0 {
		body["exclude_domains"] = strings2any(exc)
	}
	endpoint := t.Endpoint
	if endpoint == "" {
		endpoint = TavilyEndpoint
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(jsonx.Marshal(body)))
	if err != nil {
		return nil, hooks.ErrSearchUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.Key)
	j, err := send(ctx, t.Client, req)
	if err != nil {
		return nil, err
	}
	results, ok := jsonx.Get(j, "results").([]any)
	if !ok {
		return nil, hooks.ErrSearchUnavailable
	}
	hits := []hooks.SearchHit{}
	for _, r := range results {
		u := jsonx.Str(jsonx.Get(r, "url"))
		if u == "" {
			continue
		}
		title, ok := jsonx.StrOK(jsonx.Get(r, "title"))
		if !ok {
			title = u
		}
		age, ok := jsonx.StrOK(jsonx.Get(r, "published_date"))
		if !ok {
			age = jsonx.Str(jsonx.Get(r, "age"))
		}
		hits = append(hits, hooks.SearchHit{Title: title, URL: u, Snippet: jsonx.Str(jsonx.Get(r, "content")), PageAge: age})
	}
	return hits, nil
}

// composeQuery: Brave has no domain parameters, so they become site: /
// -site: terms.
func composeQuery(query string, allowed, blocked []string) string {
	q := query
	inc, exc := cleanDomains(allowed), cleanDomains(blocked)
	switch {
	case len(inc) == 1:
		q += " site:" + inc[0]
	case len(inc) > 1:
		terms := make([]string, len(inc))
		for i, d := range inc {
			terms[i] = "site:" + d
		}
		q += " (" + strings.Join(terms, " OR ") + ")"
	}
	for _, d := range exc {
		q += " -site:" + d
	}
	return q
}

var markup = regexp.MustCompile(`<[^>]+>`)

var entities = strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#x27;", "'", "&#39;", "'")

func stripMarkup(s string) string { return entities.Replace(markup.ReplaceAllString(s, "")) }

func (b *Brave) Search(ctx context.Context, query string, allowed, blocked []string) ([]hooks.SearchHit, error) {
	if b.Key == "" {
		return nil, hooks.ErrSearchUnavailable
	}
	endpoint := b.Endpoint
	if endpoint == "" {
		endpoint = BraveEndpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, hooks.ErrSearchUnavailable
	}
	u.RawQuery = url.Values{"q": {composeQuery(query, allowed, blocked)}, "count": {strconv.Itoa(ResultCount)}}.Encode()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, hooks.ErrSearchUnavailable
	}
	req.Header.Set("X-Subscription-Token", b.Key)
	req.Header.Set("Accept", "application/json")
	j, err := send(ctx, b.Client, req)
	if err != nil {
		return nil, err
	}
	// A response without web.results is a valid "no hits" answer.
	hits := []hooks.SearchHit{}
	for _, r := range jsonx.Arr(jsonx.Get(j, "web", "results")) {
		link := jsonx.Str(jsonx.Get(r, "url"))
		if link == "" {
			continue
		}
		title, ok := jsonx.StrOK(jsonx.Get(r, "title"))
		if !ok {
			title = link
		}
		hits = append(hits, hooks.SearchHit{Title: stripMarkup(title), URL: link,
			Snippet: stripMarkup(jsonx.Str(jsonx.Get(r, "description"))), PageAge: jsonx.Str(jsonx.Get(r, "age"))})
	}
	return hits, nil
}
