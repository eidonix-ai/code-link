package messages_test

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"codelink/internal/hooks"
	"codelink/internal/jsonx"
	"codelink/internal/testkit"
	"codelink/internal/testkit/harness"
)

// barrierSearcher fails unless want searches are in flight together, so a
// test using it passes only if a round's searches run concurrently.
type barrierSearcher struct {
	want    int
	mu      sync.Mutex
	arrived int
	all     chan struct{}
	queries []string
}

func newBarrierSearcher(want int) *barrierSearcher {
	return &barrierSearcher{want: want, all: make(chan struct{})}
}

func (b *barrierSearcher) Search(_ context.Context, q string, _, _ []string) ([]hooks.SearchHit, error) {
	b.mu.Lock()
	b.queries = append(b.queries, q)
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

func TestClaudeCodesMaxUsesIsCappedAtFive(t *testing.T) {
	searcher := &testkit.Searcher{}
	h := harness.Run(t, harness.Options{Hooks: hooks.Hooks{WebSearch: searcher}}, func(_ testkit.Seen, n int) testkit.Reply {
		if n == 0 {
			var parts []string
			for i := range 7 {
				parts = append(parts, searchCall("q"+strconv.Itoa(i), i))
			}
			return testkit.SSE(append(parts, testkit.Finish("tool_calls"))...)
		}
		return testkit.SSE(testkit.Content("done"), testkit.Finish(""))
	})
	// Claude Code's WebSearch asks for max_uses 8.
	ev := messages(t, h, searchBody(8, true)).Events()
	results := blocksOf(ev, "web_search_tool_result")
	eq(t, len(results), 7, "results")
	exceeded := 0
	for _, r := range results {
		if str(r, "content", "error_code") == "max_uses_exceeded" {
			exceeded++
		}
	}
	eq(t, exceeded, 2, "calls past the cap")
	q := searcher.Queries()
	sort.Strings(q)
	eq(t, q, []string{"q0", "q1", "q2", "q3", "q4"}, "the first five run")
	eq(t, num(first(ev, "message_delta"), "usage", "server_tool_use", "web_search_requests"), 5, "searches")

	// A lower max_uses still applies.
	searcher = &testkit.Searcher{}
	h = harness.Run(t, harness.Options{Hooks: hooks.Hooks{WebSearch: searcher}}, func(_ testkit.Seen, n int) testkit.Reply {
		if n == 0 {
			return testkit.SSE(searchCall("a", 0), searchCall("b", 1), searchCall("c", 2), testkit.Finish("tool_calls"))
		}
		return testkit.SSE(testkit.Content("done"), testkit.Finish(""))
	})
	messages(t, h, searchBody(2, true))
	eq(t, len(searcher.Queries()), 2, "max_uses 2")
}

func TestARoundsSearchesRunConcurrently(t *testing.T) {
	for _, stream := range []bool{true, false} {
		searcher := newBarrierSearcher(3)
		h := harness.Run(t, harness.Options{Hooks: hooks.Hooks{WebSearch: searcher}}, func(_ testkit.Seen, n int) testkit.Reply {
			if n == 0 {
				if stream {
					return testkit.SSE(searchCall("a", 0), searchCall("b", 1), searchCall("c", 2), testkit.Finish("tool_calls"))
				}
				calls := `[` +
					`{"id":"call_a","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"a\"}"}},` +
					`{"id":"call_b","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"b\"}"}},` +
					`{"id":"call_c","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"c\"}"}}]`
				return testkit.JSONR(200, testkit.Completion{ToolCalls: calls, Finish: "tool_calls", Prompt: 10, CompletionN: 2}.String())
			}
			if stream {
				return testkit.SSE(testkit.Content("done"), testkit.Finish(""))
			}
			return testkit.JSONR(200, testkit.Completion{Content: strp("done"), Prompt: 10, CompletionN: 2}.String())
		})
		r := messages(t, h, searchBody(8, stream))
		var results []any
		if stream {
			results = blocksOf(r.Events(), "web_search_tool_result")
		} else {
			for _, b := range jsonx.Arr(r.JSON()["content"]) {
				if jsonx.Type(b) == "web_search_tool_result" {
					results = append(results, b)
				}
			}
		}
		eq(t, len(results), 3, "results")
		// Outcomes keep the model's call order whatever order searches finish in.
		for i, want := range []string{"a", "b", "c"} {
			if got := str(results[i], "content", "type"); got == "web_search_tool_result_error" {
				t.Fatalf("stream=%v: search %s failed: %v", stream, want, results[i])
			}
			eq(t, str(jsonx.Arr(jsonx.Get(results[i], "content"))[0], "title"), "Result for "+want, "order")
		}
	}
}

func strp(s string) *string { return &s }
