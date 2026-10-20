package websearch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sync"
	"testing"

	"codelink/internal/hooks"
	"codelink/internal/jsonx"
)

type seen struct {
	method string
	url    *url.URL
	header http.Header
	body   []byte
}

type stub struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []seen
}

func serve(t *testing.T, status int, body string) *stub {
	s := &stub{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.reqs = append(s.reqs, seen{r.Method, r.URL, r.Header.Clone(), b})
		s.mu.Unlock()
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *stub) requests() []seen {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seen(nil), s.reqs...)
}

func canon(s string) string {
	v, err := jsonx.Parse([]byte(s))
	if err != nil {
		return "invalid: " + s
	}
	return jsonx.String(v)
}

var ctx = context.Background()

const tavilyBody = `{"results":[{"title":"Swift","url":"https://swift.org","content":"The Swift language","published_date":"2026-10-01"},{"title":"No date","url":"https://a.example/x","content":"c"},{"title":"bad","content":"no url"}]}`

func TestTavilySearchMapsResultsAndSendsRequest(t *testing.T) {
	s := serve(t, 200, tavilyBody)
	hits, err := (&Tavily{Key: "tvly-key", Endpoint: s.URL + "/search"}).Search(ctx, "swift 6.4", []string{"swift.org"}, []string{"spam.com"})
	if err != nil {
		t.Fatal(err)
	}
	want := []hooks.SearchHit{{Title: "Swift", URL: "https://swift.org", Snippet: "The Swift language", PageAge: "2026-10-01"},
		{Title: "No date", URL: "https://a.example/x", Snippet: "c"}}
	if !reflect.DeepEqual(hits, want) {
		t.Fatalf("%+v", hits)
	}
	r := s.requests()[0]
	if r.method != "POST" || r.url.Path != "/search" || r.header.Get("Authorization") != "Bearer tvly-key" || r.header.Get("Content-Type") != "application/json" {
		t.Fatalf("%+v", r)
	}
	if got := canon(string(r.body)); got != canon(`{"api_key":"tvly-key","query":"swift 6.4","max_results":5,"include_domains":["swift.org"],"exclude_domains":["spam.com"]}`) {
		t.Fatal(got)
	}
}

func TestTavilyDefaultEndpoint(t *testing.T) {
	if TavilyEndpoint != "https://api.tavily.com/search" || BraveEndpoint != "https://api.search.brave.com/res/v1/web/search" {
		t.Fatal("endpoints")
	}
}

func TestTavilyOmitsEmptyDomainLists(t *testing.T) {
	s := serve(t, 200, `{"results":[]}`)
	hits, err := (&Tavily{Key: "k", Endpoint: s.URL}).Search(ctx, "q", []string{}, nil)
	if err != nil || len(hits) != 0 {
		t.Fatal(hits, err)
	}
	b := jsonx.ParseObject(s.requests()[0].body)
	if _, ok := b["include_domains"]; ok {
		t.Fatal("include_domains")
	}
	if _, ok := b["exclude_domains"]; ok {
		t.Fatal("exclude_domains")
	}
}

func TestTavilyStatusMapping(t *testing.T) {
	for status, want := range map[int]error{401: hooks.ErrSearchUnavailable, 403: hooks.ErrSearchUnavailable, 429: hooks.ErrSearchTooManyRequests,
		500: hooks.ErrSearchUnavailable, 400: hooks.ErrSearchUnavailable} {
		s := serve(t, status, "{}")
		if _, err := (&Tavily{Key: "k", Endpoint: s.URL}).Search(ctx, "q", nil, nil); !errors.Is(err, want) {
			t.Errorf("%d: %v", status, err)
		}
	}
}

func TestTavilyUnavailableWithoutKeyOrOnTransportFailureOrGarbage(t *testing.T) {
	noKey := serve(t, 200, tavilyBody)
	if _, err := (&Tavily{Endpoint: noKey.URL}).Search(ctx, "q", nil, nil); !errors.Is(err, hooks.ErrSearchUnavailable) {
		t.Error(err)
	}
	if len(noKey.requests()) != 0 {
		t.Error("sent without a key")
	}
	down := serve(t, 200, "")
	down.Close()
	if _, err := (&Tavily{Key: "k", Endpoint: down.URL}).Search(ctx, "q", nil, nil); !errors.Is(err, hooks.ErrSearchUnavailable) {
		t.Error(err)
	}
	junk := serve(t, 200, "<html>")
	if _, err := (&Tavily{Key: "k", Endpoint: junk.URL}).Search(ctx, "q", nil, nil); !errors.Is(err, hooks.ErrSearchUnavailable) {
		t.Error(err)
	}
	noResults := serve(t, 200, "{}")
	if _, err := (&Tavily{Key: "k", Endpoint: noResults.URL}).Search(ctx, "q", nil, nil); !errors.Is(err, hooks.ErrSearchUnavailable) {
		t.Error(err)
	}
}

const braveBody = `{"web":{"results":[{"title":"Swift <strong>6</strong>","url":"https://swift.org","description":"Fast &amp; safe","age":"2 days ago"},{"title":"T","url":"https://b.example","description":"d"}]}}`

func TestBraveSearchMapsResultsAndSendsRequest(t *testing.T) {
	s := serve(t, 200, braveBody)
	hits, err := (&Brave{Key: "brave-key", Endpoint: s.URL + "/res/v1/web/search"}).Search(ctx, "swift", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []hooks.SearchHit{{Title: "Swift 6", URL: "https://swift.org", Snippet: "Fast & safe", PageAge: "2 days ago"},
		{Title: "T", URL: "https://b.example", Snippet: "d"}}
	if !reflect.DeepEqual(hits, want) {
		t.Fatalf("%+v", hits)
	}
	r := s.requests()[0]
	if r.method != "GET" || r.header.Get("X-Subscription-Token") != "brave-key" || r.header.Get("Accept") != "application/json" {
		t.Fatalf("%+v", r)
	}
	q := r.url.Query()
	if r.url.Path != "/res/v1/web/search" || q.Get("q") != "swift" || q.Get("count") != "5" {
		t.Fatal(r.url)
	}
}

func TestBraveDomainFiltersBecomeSiteTerms(t *testing.T) {
	if got := composeQuery("q", []string{"a.com"}, nil); got != "q site:a.com" {
		t.Error(got)
	}
	if got := composeQuery("q", []string{"a.com", "b.org"}, []string{"x.com", "y.com"}); got != "q (site:a.com OR site:b.org) -site:x.com -site:y.com" {
		t.Error(got)
	}
	if got := composeQuery("q", []string{}, []string{" "}); got != "q" {
		t.Error(got)
	}
	s := serve(t, 200, braveBody)
	(&Brave{Key: "k", Endpoint: s.URL}).Search(ctx, "swift", []string{"swift.org"}, []string{"x.com"})
	if got := s.requests()[0].url.Query().Get("q"); got != "swift site:swift.org -site:x.com" {
		t.Error(got)
	}
}

func TestBraveStatusMapping(t *testing.T) {
	for status, want := range map[int]error{401: hooks.ErrSearchUnavailable, 403: hooks.ErrSearchUnavailable,
		429: hooks.ErrSearchTooManyRequests, 503: hooks.ErrSearchUnavailable} {
		s := serve(t, status, "{}")
		if _, err := (&Brave{Key: "k", Endpoint: s.URL}).Search(ctx, "q", nil, nil); !errors.Is(err, want) {
			t.Errorf("%d: %v", status, err)
		}
	}
}

func TestBraveNoResultsIsEmptyNotError(t *testing.T) {
	s := serve(t, 200, "{}")
	hits, err := (&Brave{Key: "k", Endpoint: s.URL}).Search(ctx, "q", nil, nil)
	if err != nil || len(hits) != 0 {
		t.Fatal(hits, err)
	}
	if _, err := (&Brave{Endpoint: s.URL}).Search(ctx, "q", nil, nil); !errors.Is(err, hooks.ErrSearchUnavailable) {
		t.Fatal("no key", err)
	}
}

func TestUnavailableSearcherFails(t *testing.T) {
	if _, err := (Unavailable{}).Search(ctx, "q", nil, nil); !errors.Is(err, hooks.ErrSearchUnavailable) {
		t.Fatal(err)
	}
}
