package responses_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"codelink/internal/hooks"
	"codelink/internal/jsonx"
	"codelink/internal/testkit"
	"codelink/internal/testkit/harness"
)

// barrierSearcher fails unless want searches are in flight together.
type barrierSearcher struct {
	want    int
	mu      sync.Mutex
	arrived int
	all     chan struct{}
}

func (b *barrierSearcher) Search(_ context.Context, q string, _, _ []string) ([]hooks.SearchHit, error) {
	b.mu.Lock()
	b.arrived++
	if b.arrived == b.want {
		close(b.all)
	}
	b.mu.Unlock()
	select {
	case <-b.all:
		return []hooks.SearchHit{{Title: "Result for " + q, URL: "https://example.com/" + q}}, nil
	case <-time.After(3 * time.Second):
		return nil, errors.New("searches ran one at a time")
	}
}

func TestARoundsSearchesRunConcurrently(t *testing.T) {
	searcher := &barrierSearcher{want: 3, all: make(chan struct{})}
	calls := `[` +
		`{"id":"call_a","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"a\"}"}},` +
		`{"id":"call_b","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"b\"}"}},` +
		`{"id":"call_c","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"c\"}"}}]`
	h := harness.Run(t, harness.Options{Hooks: hooks.Hooks{WebSearch: searcher}}, func(_ testkit.Seen, n int) testkit.Reply {
		if n == 0 {
			return testkit.JSONR(200, testkit.Completion{ToolCalls: calls, Finish: "tool_calls", Prompt: 10, CompletionN: 2}.String())
		}
		return testkit.JSONR(200, testkit.Completion{Content: str("Answer."), Prompt: 30, CompletionN: 4}.String())
	})
	post(t, h, body("", "", false, `"tools":[{"type":"web_search"}]`))
	reqs := h.Stub.ChatRequests()
	if len(reqs) != 2 {
		t.Fatalf("rounds = %d", len(reqs))
	}
	m := jsonx.Arr(reqs[1]["messages"])
	tools := 0
	for _, msg := range m {
		if jsonx.Str(jsonx.Get(msg, "role")) == "tool" {
			tools++
			if c := jsonx.Str(jsonx.Get(msg, "content")); c == "" || strings.Contains(c, "unavailable") {
				t.Errorf("search result: %q", c)
			}
		}
	}
	if tools != 3 {
		t.Errorf("tool results = %d", tools)
	}
}
