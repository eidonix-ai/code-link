package messages_test

import (
	"context"
	"encoding/base64"
	"strings"
	"sync"
	"testing"

	"codelink/internal/config"
	"codelink/internal/hooks"
	"codelink/internal/jsonx"
	"codelink/internal/testkit"
	"codelink/internal/testkit/harness"
)

// MARK: auto mode

const classifierBody = `{"model":"claude-sonnet-5","max_tokens":64,"stop_sequences":["</severity>"],"system":[{"type":"text","text":"You are a security monitor for autonomous AI coding agents."}],"messages":[{"role":"user","content":"<transcript>rm -rf build</transcript>"}]}`

func classifierAnswer() map[string]any {
	return jsonx.ParseObject([]byte(`{"id":"msg_x","type":"message","role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"<severity>12"}],"stop_reason":"stop_sequence","stop_sequence":"</severity>","usage":{"input_tokens":900,"output_tokens":1}}`))
}

// fakeAutoMode answers classifier requests with answer (nil = a miss).
type fakeAutoMode struct {
	answer   map[string]any
	decision *config.Model
	mu       sync.Mutex
	n        int
}

func (f *fakeAutoMode) IsClassifierRequest(req map[string]any) bool {
	return strings.Contains(jsonx.String(req["system"]), "You are a security monitor for autonomous AI coding agents")
}

func (f *fakeAutoMode) Respond(context.Context, map[string]any) map[string]any {
	f.mu.Lock()
	f.n++
	f.mu.Unlock()
	if f.answer == nil {
		return nil
	}
	return jsonx.Clone(f.answer).(map[string]any)
}

func (f *fakeAutoMode) DecisionModel() (config.Model, bool) {
	if f.decision == nil {
		return config.Model{}, false
	}
	return *f.decision, true
}

func (f *fakeAutoMode) responded() int { f.mu.Lock(); defer f.mu.Unlock(); return f.n }

func TestClassifierRequestsAreAnsweredByTheResponder(t *testing.T) {
	am := &fakeAutoMode{answer: classifierAnswer()}
	h := harness.Run(t, harness.Options{Hooks: hooks.Hooks{AutoMode: am}}, func(testkit.Seen, int) testkit.Reply { return testkit.JSONR(500, "{}") })
	r := messages(t, h, classifierBody)
	eq(t, r.Status, 200, "status")
	eq(t, str(r.JSON(), "content", 0, "text"), "<severity>12", "text")
	eq(t, str(r.JSON(), "stop_reason")+" "+str(r.JSON(), "stop_sequence"), "stop_sequence </severity>", "stop")

	// stream: true gets the same answer as an event sequence.
	s := messages(t, h, strings.Replace(classifierBody, `"max_tokens":64`, `"max_tokens":64,"stream":true`, 1))
	ev := testkit.WithoutPings(s.Events())
	eq(t, testkit.Names(ev), []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}, "events")
	eq(t, str(ev[2].Data, "delta", "text"), "<severity>12", "delta")
	eq(t, str(ev[4].Data, "delta", "stop_reason"), "stop_sequence", "stop")
	eq(t, str(ev[4].Data, "delta", "stop_sequence"), "</severity>", "stop sequence")
	eq(t, jsonx.String(jsonx.Get(ev[0].Data, "message", "content")), "[]", "start content")
	eq(t, num(ev[0].Data, "message", "usage", "output_tokens"), 0, "start output tokens")
	eq(t, num(ev[4].Data, "usage", "output_tokens"), 1, "final output tokens")
	eq(t, am.responded(), 2, "responded")
	eq(t, len(h.Stub.ChatRequests()), 0, "no backend call")
}

func TestAMissRunsTheRequestOnTheDecisionModel(t *testing.T) {
	w := 32_000
	decision := config.Model{ID: "Small", BackendID: "stub", Upstream: "small-upstream", ContextWindow: &w}
	am := &fakeAutoMode{decision: &decision}
	content := "\n</think>\n<severity>80"
	h := harness.Run(t, harness.Options{Hooks: hooks.Hooks{AutoMode: am}}, func(testkit.Seen, int) testkit.Reply {
		return testkit.JSONR(200, testkit.Completion{Content: &content, Finish: "stop", StopReason: "</severity>"}.String())
	})
	r := messages(t, h, classifierBody)
	eq(t, r.Status, 200, "status")
	eq(t, str(h.Stub.LastChat(), "model"), "small-upstream", "decision model")
	eq(t, jsonx.String(h.Stub.LastChat()["stop"]), `["</severity>"]`, "stop")
	eq(t, str(r.JSON(), "content", 0, "text"), "<severity>80", "text")
	eq(t, str(r.JSON(), "stop_reason"), "stop_sequence", "stop reason")
	// Ordinary requests are untouched.
	messages(t, h, testkit.MessagesBody("", "claude-sonnet-5", false, ""))
	eq(t, str(h.Stub.LastChat(), "model"), "glm-upstream", "ordinary")
	eq(t, am.responded(), 1, "responded")
}

// MARK: images (the Messages side: vision detection and image_url parts;
// describing images is internal/media's)

const image = `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgoAAAANSUhEUg=="}}`

func TestImagesPassedToAVisionModel(t *testing.T) {
	yes := true
	h := harness.Run(t, harness.Options{Configure: func(c *config.Config) { c.Models[0].Vision = &yes }}, okCompletion)
	messages(t, h, testkit.MessagesBody("", "", false, `[{"role":"user","content":[`+image+`]},{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"Shot","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[`+image+`]}]}]`))
	m := jsonx.Arr(h.Stub.LastChat()["messages"])
	eq(t, jsonx.Type(jsonx.Get(m[0], "content", 0)), "image_url", "top-level image")
	var roles []string
	for _, x := range m {
		roles = append(roles, str(x, "role"))
	}
	eq(t, roles, []string{"user", "assistant", "tool", "user"}, "roles")
	parts := jsonx.Arr(jsonx.Get(m[3], "content"))
	eq(t, jsonx.Type(parts[len(parts)-1]), "image_url", "tool image")
}

// A model configured without vision is asked once, on the first request
// with an image; one that reads it gets images directly. (The refusing
// case, where images are described, is tested with internal/media.)
func TestVisionIsDetectedOnTheFirstImage(t *testing.T) {
	h := harness.Run(t, harness.Options{
		Hooks:     hooks.Hooks{Images: &testkit.Describer{}},
		Configure: func(c *config.Config) { c.Roles.Vision = "Big" },
	}, func(seen testkit.Seen, _ int) testkit.Reply {
		if n, _ := jsonx.Int(seen.JSON()["max_tokens"]); n == 1 {
			return testkit.JSONR(200, testkit.Text("H"))
		}
		return testkit.JSONR(200, testkit.Text("ok"))
	})
	messages(t, h, testkit.MessagesBody("", "", false, `[{"role":"user","content":[{"type":"text","text":"hi"}]}]`))
	eq(t, len(h.Stub.ChatRequests()), 1, "no image, no probe")
	for range 2 {
		messages(t, h, testkit.MessagesBody("", "", false, `[{"role":"user","content":[`+image+`]}]`))
	}
	eq(t, len(h.Stub.ChatRequests()), 4, "probed once")
	eq(t, jsonx.Type(jsonx.Get(h.Stub.LastChat(), "messages", 0, "content", 0)), "image_url", "image passed")
}

// MARK: web search

func searchBody(maxUses int, stream bool) string {
	s := `{"model":"claude-haiku-4-5","max_tokens":1000,"stream":STREAM,"system":[{"type":"text","text":"You run a web search."}],"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":MAX}],"messages":[{"role":"user","content":"Perform a web search for the query: swift 6.4 release"}]}`
	s = strings.Replace(s, "MAX", strings.TrimSpace(jsonx.String(maxUses)), 1)
	return strings.Replace(s, "STREAM", jsonx.String(stream), 1)
}

func searchCall(q string, index int) string {
	return testkit.ToolCall(index, "call_s"+jsonx.String(index), "web_search", `{"query":"`+q+`"}`)
}

func blocksOf(ev []testkit.SSEEvent, typ string) []any {
	var out []any
	for _, e := range ev {
		if cb := jsonx.Get(e.Data, "content_block"); jsonx.Type(cb) == typ {
			out = append(out, cb)
		}
	}
	return out
}

func TestSearchesStreamAcrossGenerations(t *testing.T) {
	searcher := &testkit.Searcher{}
	h := harness.Run(t, harness.Options{Hooks: hooks.Hooks{WebSearch: searcher}}, func(_ testkit.Seen, n int) testkit.Reply {
		if n == 0 {
			return testkit.SSE(testkit.Reasoning("I should search."), searchCall("swift 6.4 release", 0), testkit.Finish("tool_calls"), testkit.Usage(100, 10, 0))
		}
		return testkit.SSE(testkit.Content("Swift 6.4 is out."), testkit.Finish("stop"), testkit.Usage(300, 20, 90))
	})
	ev := messages(t, h, searchBody(8, true)).Events()
	eq(t, testkit.CheckBlocksBalanced(t, ev), map[int]string{0: "thinking", 1: "server_tool_use", 2: "web_search_tool_result", 3: "text"}, "blocks")
	stu := blocksOf(ev, "server_tool_use")[0]
	eq(t, str(stu, "name"), "web_search", "name")
	if !strings.HasPrefix(str(stu, "id"), "srvtoolu_") {
		t.Errorf("id %v", stu)
	}
	found := false
	for _, e := range ev {
		if str(e.Data, "delta", "partial_json") == `{"query":"swift 6.4 release"}` {
			found = true
		}
	}
	eq(t, found, true, "query delta")
	result := blocksOf(ev, "web_search_tool_result")[0]
	eq(t, str(result, "tool_use_id"), str(stu, "id"), "tool_use_id")
	hit := jsonx.Get(result, "content", 0)
	eq(t, str(hit, "type"), "web_search_result", "hit type")
	if !strings.HasPrefix(str(hit, "url"), "https://example.com/") {
		t.Errorf("url %v", hit)
	}
	eq(t, str(hit, "title"), "Result for swift 6.4 release", "title")
	eq(t, str(hit, "page_age"), "2026-01-01", "page_age")
	snippet, _ := base64.StdEncoding.DecodeString(str(hit, "encrypted_content"))
	eq(t, string(snippet), "Snippet about swift 6.4 release", "snippet")
	eq(t, len(blocksOf(ev, "tool_use")), 0, "no tool_use")
	d := first(ev, "message_delta")
	eq(t, str(d, "delta", "stop_reason"), "end_turn", "stop")
	eq(t, num(d, "usage", "server_tool_use", "web_search_requests"), 1, "searches")
	eq(t, []int{num(d, "usage", "output_tokens"), num(d, "usage", "input_tokens"), num(d, "usage", "cache_read_input_tokens")}, []int{30, 310, 90}, "usage")
	eq(t, last(ev).Event, "message_stop", "last")
	eq(t, searcher.Queries(), []string{"swift 6.4 release"}, "queries")

	reqs := h.Stub.ChatRequests()
	eq(t, len(reqs), 2, "rounds")
	var names []string
	for _, x := range jsonx.Arr(reqs[0]["tools"]) {
		names = append(names, str(x, "function", "name"))
	}
	eq(t, names, []string{"web_search"}, "tools")
	eq(t, str(reqs[0], "model"), "small-upstream", "model")
	m := jsonx.Arr(reqs[1]["messages"])
	assistant, tool := m[len(m)-2], m[len(m)-1]
	eq(t, str(assistant, "tool_calls", 0, "function", "name"), "web_search", "replayed call")
	eq(t, str(assistant, "reasoning_content"), "I should search.", "replayed reasoning")
	eq(t, str(tool, "role")+" "+str(tool, "tool_call_id"), "tool call_s0", "tool message")
	if !strings.Contains(str(tool, "content"), "Snippet about swift 6.4 release") {
		t.Errorf("tool content: %v", tool)
	}
	logged := false
	for _, e := range h.Log.All() {
		if e.Kind == "websearch" && e.Query == "swift 6.4 release" && e.Results == 2 {
			logged = true
		}
	}
	eq(t, logged, true, "websearch logged")
}

func TestMaxUsesAndUnavailableBecomeErrorResults(t *testing.T) {
	searcher := &testkit.Searcher{}
	h := harness.Run(t, harness.Options{Hooks: hooks.Hooks{WebSearch: searcher}}, func(_ testkit.Seen, n int) testkit.Reply {
		if n == 0 {
			return testkit.SSE(searchCall("one", 0), searchCall("two", 1), testkit.Finish("tool_calls"))
		}
		return testkit.SSE(testkit.Content("done"), testkit.Finish(""))
	})
	ev := messages(t, h, searchBody(1, true)).Events()
	results := blocksOf(ev, "web_search_tool_result")
	eq(t, len(results), 2, "results")
	eq(t, len(jsonx.Arr(jsonx.Get(results[0], "content"))), 2, "hits") // the fake searcher gives two
	eq(t, str(results[1], "content", "type"), "web_search_tool_result_error", "error type")
	eq(t, str(results[1], "content", "error_code"), "max_uses_exceeded", "error code")
	eq(t, searcher.Queries(), []string{"one"}, "queries")
	m := jsonx.Arr(h.Stub.ChatRequests()[1]["messages"])
	if !strings.Contains(str(m[len(m)-1], "content"), "search limit") {
		t.Errorf("tool text: %v", m[len(m)-1])
	}

	// No search provider: unavailable.
	h = harness.Run(t, harness.Options{}, func(_ testkit.Seen, n int) testkit.Reply {
		if n == 0 {
			return testkit.SSE(searchCall("x", 0), testkit.Finish("tool_calls"))
		}
		return testkit.SSE(testkit.Content("Could not search."), testkit.Finish(""))
	})
	ev = messages(t, h, searchBody(8, true)).Events()
	eq(t, str(blocksOf(ev, "web_search_tool_result")[0], "content", "error_code"), "unavailable", "unavailable")
	eq(t, joined(ev, "text"), "Could not search.", "text")
	eq(t, num(first(ev, "message_delta"), "usage", "server_tool_use", "web_search_requests"), 0, "no searches")

	// A rate-limited provider: too_many_requests.
	h = harness.Run(t, harness.Options{Hooks: hooks.Hooks{WebSearch: &testkit.Searcher{Err: hooks.ErrSearchTooManyRequests}}}, func(_ testkit.Seen, n int) testkit.Reply {
		if n == 0 {
			return testkit.SSE(searchCall("x", 0), testkit.Finish("tool_calls"))
		}
		return testkit.SSE(testkit.Content("later"), testkit.Finish(""))
	})
	ev = messages(t, h, searchBody(8, true)).Events()
	eq(t, str(blocksOf(ev, "web_search_tool_result")[0], "content", "error_code"), "too_many_requests", "rate limited")
}

func TestTheLastRoundHasNoTools(t *testing.T) {
	searcher := &testkit.Searcher{}
	h := harness.Run(t, harness.Options{Hooks: hooks.Hooks{WebSearch: searcher}}, func(seen testkit.Seen, _ int) testkit.Reply {
		if _, ok := seen.JSON()["tools"]; !ok {
			return testkit.SSE(testkit.Content("final"), testkit.Finish(""))
		}
		return testkit.SSE(searchCall("again", 0), testkit.Finish("tool_calls"))
	})
	ev := messages(t, h, searchBody(1, true)).Events()
	eq(t, len(h.Stub.ChatRequests()), 3, "search, told the limit, answer without tools")
	eq(t, searcher.Queries(), []string{"again"}, "queries")
	eq(t, joined(ev, "text"), "final", "text")
	eq(t, last(ev).Event, "message_stop", "last")
	// Later rounds don't force a call.
	eq(t, h.Stub.ChatRequests()[1]["tool_choice"] == nil || h.Stub.ChatRequests()[1]["tool_choice"] == "auto", true, "tool_choice")
}

func TestClientToolCallsEndTheLoop(t *testing.T) {
	h := harness.Run(t, harness.Options{Hooks: hooks.Hooks{WebSearch: &testkit.Searcher{}}}, func(testkit.Seen, int) testkit.Reply {
		return testkit.SSE(searchCall("q", 0), testkit.ToolCall(1, "call_r", "Read", `{"p":1}`), testkit.Finish("tool_calls"))
	})
	b := strings.Replace(searchBody(8, true), `"tools":[`, `"tools":[{"name":"Read","input_schema":{"type":"object"}},`, 1)
	ev := messages(t, h, b).Events()
	types := map[string]bool{}
	for _, v := range testkit.CheckBlocksBalanced(t, ev) {
		types[v] = true
	}
	eq(t, types, map[string]bool{"tool_use": true, "server_tool_use": true, "web_search_tool_result": true}, "types")
	eq(t, str(first(ev, "message_delta"), "delta", "stop_reason"), "tool_use", "stop")
	eq(t, len(h.Stub.ChatRequests()), 1, "one round")
}

func TestNonStreamingSearch(t *testing.T) {
	searcher := &testkit.Searcher{}
	found := "Found it."
	h := harness.Run(t, harness.Options{Hooks: hooks.Hooks{WebSearch: searcher}}, func(_ testkit.Seen, n int) testkit.Reply {
		if n == 0 {
			return testkit.JSONR(200, testkit.Completion{ToolCalls: `[{"id":"c1","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"q1\"}"}}]`,
				Finish: "tool_calls", Prompt: 10, CompletionN: 2}.String())
		}
		return testkit.JSONR(200, testkit.Completion{Content: &found, Prompt: 30, CompletionN: 3}.String())
	})
	r := messages(t, h, searchBody(8, false))
	eq(t, r.Status, 200, "status")
	var types []string
	for _, b := range jsonx.Arr(r.JSON()["content"]) {
		types = append(types, jsonx.Type(b))
	}
	eq(t, types, []string{"server_tool_use", "web_search_tool_result", "text"}, "types")
	eq(t, str(r.JSON(), "stop_reason"), "end_turn", "stop")
	eq(t, num(r.JSON(), "usage", "server_tool_use", "web_search_requests"), 1, "searches")
	eq(t, []int{num(r.JSON(), "usage", "input_tokens"), num(r.JSON(), "usage", "output_tokens")}, []int{40, 5}, "usage")
	eq(t, searcher.Queries(), []string{"q1"}, "queries")
	m := jsonx.Arr(h.Stub.ChatRequests()[1]["messages"])
	eq(t, str(m[len(m)-1], "role")+" "+str(m[len(m)-1], "tool_call_id"), "tool c1", "tool message")
}

func TestSearchUpstreamErrorBeforeTheStreamKeepsItsStatus(t *testing.T) {
	h := harness.Run(t, harness.Options{Hooks: hooks.Hooks{WebSearch: &testkit.Searcher{}}}, func(testkit.Seen, int) testkit.Reply {
		return testkit.JSONR(503, `{"error":{"message":"busy"}}`)
	})
	r := messages(t, h, searchBody(8, true))
	eq(t, r.Status, 503, "status")
	eq(t, str(r.JSON(), "error", "type"), "overloaded_error", "type")
}

func TestAFailedLaterRoundIsAnInBandError(t *testing.T) {
	h := harness.Run(t, harness.Options{Hooks: hooks.Hooks{WebSearch: &testkit.Searcher{}}}, func(_ testkit.Seen, n int) testkit.Reply {
		if n == 0 {
			return testkit.SSE(searchCall("q", 0), testkit.Finish("tool_calls"))
		}
		return testkit.JSONR(500, `{"error":{"message":"oops"}}`)
	})
	r := messages(t, h, searchBody(8, true))
	eq(t, r.Status, 200, "status")
	eq(t, last(r.Events()).Event, "error", "last")
	eq(t, str(last(r.Events()).Data, "error", "type"), "api_error", "type")
}
