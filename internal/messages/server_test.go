package messages_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"codelink/internal/config"
	"codelink/internal/jsonx"
	"codelink/internal/testkit"
	"codelink/internal/testkit/harness"
	"codelink/internal/upstream"
)

func body(extra string, stream bool) string { return testkit.MessagesBody(extra, "", stream, "") }

func okCompletion(testkit.Seen, int) testkit.Reply { return testkit.JSONR(200, testkit.Text("ok")) }

func eq[T any](t *testing.T, got, want T, what string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: got %#v, want %#v", what, got, want)
	}
}

func str(v any, path ...any) string { return jsonx.Str(jsonx.Get(v, path...)) }

func num(v any, path ...any) int {
	n, ok := jsonx.Int(jsonx.Get(v, path...))
	if !ok {
		return -1
	}
	return n
}

func request(t *testing.T, h *harness.H, method, path, b string, header map[string]string, auth bool) testkit.HTTPReply {
	t.Helper()
	r, err := h.Request(method, path, b, header, auth)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func messages(t *testing.T, h *harness.H, b string) testkit.HTTPReply {
	t.Helper()
	return request(t, h, "POST", "/v1/messages", b, nil, true)
}

func first(ev []testkit.SSEEvent, name string) map[string]any {
	for _, e := range ev {
		if e.Event == name {
			return e.Data
		}
	}
	return nil
}

func last(ev []testkit.SSEEvent) testkit.SSEEvent {
	if len(ev) == 0 {
		return testkit.SSEEvent{}
	}
	return ev[len(ev)-1]
}

func joined(ev []testkit.SSEEvent, field string) string {
	var b strings.Builder
	for _, e := range ev {
		b.WriteString(str(e.Data, "delta", field))
	}
	return b.String()
}

func contains(names []string, n string) bool {
	for _, x := range names {
		if x == n {
			return true
		}
	}
	return false
}

func requestEvents(h *harness.H) []int {
	var out []int
	for _, e := range h.Log.All() {
		if e.Kind == "request" {
			out = append(out, e.Status)
		}
	}
	return out
}

// MARK: auth, health, models

func TestHealthNeedsNoAuthButTheAPIDoes(t *testing.T) {
	h := harness.Run(t, harness.Options{}, okCompletion)
	health := request(t, h, "GET", "/healthz", "", nil, false)
	eq(t, health.Status, 200, "health")
	eq(t, str(health.JSON(), "status"), "ok", "health body")

	noAuth := request(t, h, "POST", "/v1/messages", body("", false), nil, false)
	eq(t, noAuth.Status, 401, "no auth")
	eq(t, str(noAuth.JSON(), "type")+"/"+str(noAuth.JSON(), "error", "type"), "error/authentication_error", "no auth body")
	wrong := request(t, h, "POST", "/v1/messages", body("", false), map[string]string{"Authorization": "Bearer nope"}, false)
	eq(t, wrong.Status, 401, "wrong token")
	eq(t, request(t, h, "GET", "/v1/models", "", nil, false).Status, 401, "models no auth")

	eq(t, messages(t, h, body("", false)).Status, 200, "bearer")
	apiKey := request(t, h, "POST", "/v1/messages", body("", false), map[string]string{"x-api-key": testkit.Token}, false)
	eq(t, apiKey.Status, 200, "x-api-key")
	eq(t, len(h.Stub.ChatRequests()), 2, "chat requests")
}

func TestModelsListsConfiguredIDsAndRoleAliases(t *testing.T) {
	h := harness.Run(t, harness.Options{}, okCompletion)
	r := request(t, h, "GET", "/v1/models", "", map[string]string{"anthropic-version": "2023-06-01"}, true)
	eq(t, r.Status, 200, "status")
	var ids []string
	for _, d := range jsonx.Arr(r.JSON()["data"]) {
		ids = append(ids, str(d, "id"))
	}
	eq(t, ids, []string{"GLM", "Small", "Big", "sonnet", "opus", "haiku"}, "ids")
	eq(t, str(r.JSON(), "data", 0, "type"), "model", "type")
	eq(t, r.JSON()["has_more"], any(false), "has_more")
	eq(t, str(r.JSON(), "first_id"), "GLM", "first_id")
	eq(t, str(r.JSON(), "data", 4, "display_name"), "Big (opus)", "display_name")
}

// MARK: non-streaming

func TestNonStreamingTranslatesBothWays(t *testing.T) {
	content := "<block>no"
	h := harness.Run(t, harness.Options{}, func(testkit.Seen, int) testkit.Reply {
		return testkit.JSONR(200, testkit.Completion{Content: &content, Reasoning: "think", Finish: "stop", StopReason: "</block>",
			Prompt: 100, CompletionN: 4, Cached: 60}.String())
	})
	r := messages(t, h, body(`"stop_sequences":["</block>"],"system":"sys"`, false))
	eq(t, r.Status, 200, "status")
	up := h.Stub.LastChat()
	eq(t, str(up, "model"), "glm-upstream", "upstream model")
	eq(t, up["stream"], any(false), "stream")
	eq(t, jsonx.String(up["stop"]), `["</block>"]`, "stop")
	eq(t, str(up, "messages", 0, "role"), "system", "system")
	reqs := h.Stub.Requests()
	eq(t, reqs[len(reqs)-1].Header.Get("Authorization"), "Bearer sk-stub-secret", "auth")
	msg := r.JSON()
	eq(t, str(msg, "type")+"/"+str(msg, "role")+"/"+str(msg, "model"), "message/assistant/GLM", "message")
	var types []string
	for _, b := range jsonx.Arr(msg["content"]) {
		types = append(types, jsonx.Type(b))
	}
	eq(t, types, []string{"thinking", "text"}, "types")
	eq(t, str(msg, "stop_reason")+" "+str(msg, "stop_sequence"), "stop_sequence </block>", "stop")
	eq(t, num(msg, "usage", "input_tokens"), 40, "input")
	eq(t, num(msg, "usage", "cache_read_input_tokens"), 60, "cached")
	st := requestEvents(h)
	eq(t, st[len(st)-1], 200, "logged")
}

func TestClaudeModelNamesRouteToRoles(t *testing.T) {
	h := harness.Run(t, harness.Options{}, okCompletion)
	for _, c := range [][2]string{{"claude-haiku-4-5-20251001", "small-upstream"}, {"claude-opus-5[1m]", "big-upstream"},
		{"claude-fable-5-1", "big-upstream"}, {"claude-sonnet-5", "glm-upstream"}, {"Small", "small-upstream"}, {"whatever", "glm-upstream"}} {
		r := messages(t, h, testkit.MessagesBody("", c[0], false, ""))
		eq(t, r.Status, 200, c[0])
		eq(t, str(h.Stub.LastChat(), "model"), c[1], c[0])
		eq(t, str(r.JSON(), "model"), c[0], c[0]+" echoed")
	}
}

func TestUnknownModelWithoutMainIs404(t *testing.T) {
	h := harness.Run(t, harness.Options{Configure: func(c *config.Config) { c.Roles = config.Roles{} }}, okCompletion)
	r := messages(t, h, testkit.MessagesBody("", "claude-sonnet-5", false, ""))
	eq(t, r.Status, 404, "status")
	eq(t, str(r.JSON(), "error", "type"), "not_found_error", "type")
	bad := messages(t, h, "{nope")
	eq(t, bad.Status, 400, "bad JSON")
	eq(t, str(bad.JSON(), "error", "type"), "invalid_request_error", "bad type")
	eq(t, messages(t, h, `{"messages":[]}`).Status, 400, "missing model")
}

// MARK: streaming

func TestStreamingEventOrderAndBlockIndices(t *testing.T) {
	chunks := []string{
		testkit.Content("hello"),
		testkit.Reasoning("hmm"),
		testkit.ToolCall(0, "call_a", "A", ""),
		testkit.ToolCall(0, "call_a", "", `{"x":`),
		testkit.ToolCall(0, "call_a", "", "1}"),
		testkit.ToolCall(1, "call_b", "B", "{}"),
		testkit.Finish("stop"),
		testkit.Usage(50, 7, 20),
	}
	h := harness.Run(t, harness.Options{}, func(testkit.Seen, int) testkit.Reply { return testkit.SSE(chunks...) })
	r := messages(t, h, body("", true))
	eq(t, r.Status, 200, "status")
	eq(t, r.Header.Get("Content-Type"), "text/event-stream", "content type")
	ev := r.Events()
	eq(t, ev[0].Event, "message_start", "first")
	eq(t, str(ev[0].Data, "message", "model"), "GLM", "model")
	eq(t, ev[1].Event, "ping", "second")
	eq(t, testkit.CheckBlocksBalanced(t, ev), map[int]string{0: "text", 1: "thinking", 2: "tool_use", 3: "tool_use"}, "blocks")
	var args strings.Builder
	sig := false
	for _, e := range ev {
		if e.Event == "content_block_delta" && num(e.Data, "index") == 2 {
			args.WriteString(str(e.Data, "delta", "partial_json"))
		}
		if str(e.Data, "delta", "type") == "signature_delta" {
			sig = true
		}
	}
	eq(t, args.String(), `{"x":1}`, "args")
	eq(t, sig, true, "signature")
	d := first(ev, "message_delta")
	eq(t, str(d, "delta", "stop_reason"), "tool_use", "stop")
	eq(t, []int{num(d, "usage", "input_tokens"), num(d, "usage", "cache_read_input_tokens"), num(d, "usage", "output_tokens")}, []int{30, 20, 7}, "usage")
	eq(t, last(ev).Event, "message_stop", "last")
	up := h.Stub.LastChat()
	eq(t, up["stream"], any(true), "stream")
	eq(t, jsonx.Get(up, "stream_options", "include_usage"), any(true), "include_usage")
	reqs := h.Stub.Requests()
	eq(t, reqs[len(reqs)-1].Header.Get("Accept"), "text/event-stream", "accept")
}

func TestThinkTagsInContentBecomeThinking(t *testing.T) {
	chunks := []string{testkit.Content("<thi"), testkit.Content("nk>let me see</th"), testkit.Content("ink>The answer"), testkit.Finish("")}
	h := harness.Run(t, harness.Options{}, func(testkit.Seen, int) testkit.Reply { return testkit.SSE(chunks...) })
	ev := messages(t, h, body("", true)).Events()
	eq(t, testkit.CheckBlocksBalanced(t, ev), map[int]string{0: "thinking", 1: "text"}, "blocks")
	eq(t, joined(ev, "thinking"), "let me see", "thinking")
	eq(t, joined(ev, "text"), "The answer", "text")
	eq(t, str(first(ev, "message_delta"), "delta", "stop_reason"), "end_turn", "stop")
}

func TestStreamingStopSequenceAndMaxTokens(t *testing.T) {
	h := harness.Run(t, harness.Options{}, func(_ testkit.Seen, n int) testkit.Reply {
		if n == 0 {
			return testkit.SSE(testkit.Content("<severity>12"), testkit.Chunk("{}", "stop", `,"stop_reason":"</severity>"`))
		}
		return testkit.SSE(testkit.Content("long"), testkit.Finish("length"))
	})
	a := messages(t, h, body(`"stop_sequences":["</severity>"]`, true)).Events()
	d := first(a, "message_delta")
	eq(t, str(d, "delta", "stop_reason")+" "+str(d, "delta", "stop_sequence"), "stop_sequence </severity>", "stop sequence")
	b := messages(t, h, body("", true)).Events()
	eq(t, str(first(b, "message_delta"), "delta", "stop_reason"), "max_tokens", "max_tokens")
}

func TestTruncatedStreamsEndWithAnOverloadedError(t *testing.T) {
	cases := []testkit.Reply{
		testkit.SSEReply{Chunks: []string{testkit.Content("par")}},
		testkit.SSEReply{Chunks: []string{testkit.Content("par")}, Done: true},
		testkit.SSEReply{},
		testkit.RawStream{Text: "data: " + testkit.Content("par") + "\n\ndata: {\"choi"},
	}
	for i, reply := range cases {
		h := harness.Run(t, harness.Options{}, func(testkit.Seen, int) testkit.Reply { return reply })
		r := messages(t, h, body("", true))
		ev := r.Events()
		names := testkit.Names(ev)
		eq(t, r.Status, 200, fmt.Sprint("case ", i))
		eq(t, last(ev).Event, "error", fmt.Sprint("case ", i, names))
		eq(t, str(last(ev).Data, "error", "type"), "overloaded_error", fmt.Sprint("case ", i))
		if contains(names, "message_stop") || contains(names, "message_delta") {
			t.Errorf("case %d: %v", i, names)
		}
	}
}

func TestCompleteStreamWithoutDoneIsAccepted(t *testing.T) {
	h := harness.Run(t, harness.Options{}, func(testkit.Seen, int) testkit.Reply {
		return testkit.SSEReply{Chunks: []string{testkit.Content("hi"), testkit.Finish("")}}
	})
	eq(t, last(messages(t, h, body("", true)).Events()).Event, "message_stop", "last")
}

func TestLongLinesPassIntact(t *testing.T) {
	big := strings.Repeat("x", 2<<20)
	h := harness.Run(t, harness.Options{}, func(testkit.Seen, int) testkit.Reply {
		return testkit.SSE(testkit.Content(big), testkit.Finish(""))
	})
	ev := messages(t, h, body("", true)).Events()
	eq(t, len(joined(ev, "text")), len(big), "length")
	eq(t, last(ev).Event, "message_stop", "last")
}

func TestLineReaderSplitsAndLimits(t *testing.T) {
	lines := func(s string) ([]string, error) {
		r := upstream.NewLineReader(strings.NewReader(s))
		var out []string
		for {
			l, ok, err := r.Next()
			if err != nil || !ok {
				return out, err
			}
			out = append(out, l)
		}
	}
	got, _ := lines("a\r\nb\n\nc")
	eq(t, got, []string{"a", "b", "", "c"}, "split")
	got, _ = lines("data: héllo\n")
	eq(t, got, []string{"data: héllo"}, "utf8")
	if _, err := lines(strings.Repeat("z", 17<<20)); !errors.Is(err, upstream.ErrLineTooLong) {
		t.Errorf("long line: %v", err)
	}
}

func TestInBandErrorFrame(t *testing.T) {
	frame := `{"error":{"message":"This model's maximum context length is 1000 tokens. However, you requested 1500 tokens (1400 in the messages, 100 in the completion).","code":400}}`
	h := harness.Run(t, harness.Options{}, func(_ testkit.Seen, n int) testkit.Reply {
		if n == 0 {
			return testkit.SSEReply{Chunks: []string{testkit.Content("a"), frame}}
		}
		return testkit.SSEReply{Chunks: []string{`{"error":{"message":"engine dead"}}`}}
	})
	ev := messages(t, h, body("", true)).Events()
	eq(t, last(ev).Event, "error", "last")
	eq(t, str(last(ev).Data, "error", "message"), "prompt is too long: 1400 tokens > 1000 maximum", "message")
	eq(t, testkit.CheckBlocksBalanced(t, ev), map[int]string{0: "text"}, "blocks")
	ev2 := messages(t, h, body("", true)).Events()
	eq(t, str(last(ev2).Data, "error", "type"), "overloaded_error", "dead")
}

func TestPingsWhileTheBackendIsQuiet(t *testing.T) {
	tuning := testkit.FastTuning()
	tuning.PingInterval = 25 * time.Millisecond
	h := harness.Run(t, harness.Options{Tuning: &tuning}, func(testkit.Seen, int) testkit.Reply {
		return testkit.SSEReply{Chunks: []string{testkit.Content("late"), testkit.Finish("")}, Done: true, After: 400 * time.Millisecond}
	})
	ev := messages(t, h, body("", true)).Events()
	eq(t, ev[0].Event, "message_start", "first")
	pings := 0
	for _, e := range ev {
		if e.Event == "content_block_start" {
			break
		}
		if e.Event == "ping" {
			pings++
		}
	}
	if pings < 5 {
		t.Errorf("%d pings before output: %v", pings, testkit.Names(ev))
	}
	eq(t, last(ev).Event, "message_stop", "last")
}

func streamRequest(t *testing.T, ctx context.Context, h *harness.H) *http.Response {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("http://127.0.0.1:%d/v1/messages", h.Port), strings.NewReader(body("", true)))
	req.Header.Set("Authorization", "Bearer "+testkit.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestMessageStartArrivesBeforeTheFirstToken(t *testing.T) {
	h := harness.Run(t, harness.Options{}, func(testkit.Seen, int) testkit.Reply {
		return testkit.SSEReply{Chunks: []string{testkit.Content("x"), testkit.Finish("")}, Done: true, After: time.Second}
	})
	started := time.Now()
	resp := streamRequest(t, context.Background(), h)
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	var firstLine string
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "event:") {
			firstLine = sc.Text()
			break
		}
	}
	eq(t, firstLine, "event: message_start", "first event")
	if d := time.Since(started); d >= 900*time.Millisecond {
		t.Errorf("message_start after %v", d)
	}
}

// MARK: errors before the stream

func TestUpstreamErrorsKeepTheirRealStatus(t *testing.T) {
	for _, c := range []struct {
		up, status int
		typ        string
	}{{400, 400, "invalid_request_error"}, {401, 502, "api_error"}, {404, 404, "not_found_error"},
		{429, 429, "rate_limit_error"}, {500, 500, "api_error"}, {503, 503, "overloaded_error"}, {529, 529, "overloaded_error"}} {
		h := harness.Run(t, harness.Options{}, func(testkit.Seen, int) testkit.Reply {
			return testkit.JSONReply{Status: c.up, Body: `{"error":{"message":"upstream said no at http://10.0.0.1:8000"}}`,
				Header: map[string]string{"Retry-After": "7", "X-Should-Retry": "true"}}
		})
		for _, stream := range []bool{false, true} {
			what := fmt.Sprintf("upstream %d stream %v", c.up, stream)
			r := messages(t, h, body("", stream))
			eq(t, r.Status, c.status, what)
			eq(t, str(r.JSON(), "error", "type"), c.typ, what)
			if strings.Contains(str(r.JSON(), "error", "message"), "10.0.0.1") {
				t.Errorf("%s: address leaked: %s", what, r.Text())
			}
			eq(t, r.Header.Get("Retry-After")+" "+r.Header.Get("X-Should-Retry"), "7 true", what+" headers")
		}
	}
}

func TestContextOverflowRewrittenOverHTTP(t *testing.T) {
	h := harness.Run(t, harness.Options{}, func(testkit.Seen, int) testkit.Reply {
		return testkit.JSONR(400, `{"error":{"message":"This model's maximum context length is 32768 tokens. However, you requested 40000 tokens (35000 in the messages, 5000 in the completion).","type":"BadRequestError","code":400}}`)
	})
	r := messages(t, h, body("", true))
	eq(t, r.Status, 400, "status")
	eq(t, str(r.JSON(), "error", "message"), "prompt is too long: 35000 tokens > 32768 maximum", "message")
}

func TestHeaderWaitIsBounded(t *testing.T) {
	h := harness.Run(t, harness.Options{Configure: func(c *config.Config) { c.HeaderTimeoutSeconds = 0.3 }}, func(testkit.Seen, int) testkit.Reply {
		return testkit.Delayed{After: 3 * time.Second, Reply: testkit.JSONR(200, testkit.Text("late"))}
	})
	started := time.Now()
	r := messages(t, h, body("", true))
	eq(t, r.Status, 504, "status")
	if d := time.Since(started); d >= 2*time.Second {
		t.Errorf("took %v", d)
	}
}

func TestAStartedStreamOutlivesTheHeaderWait(t *testing.T) {
	h := harness.Run(t, harness.Options{Configure: func(c *config.Config) { c.HeaderTimeoutSeconds = 0.3 }}, func(testkit.Seen, int) testkit.Reply {
		return testkit.SSEReply{Chunks: []string{testkit.Content("still here"), testkit.Finish("")}, Done: true, After: 800 * time.Millisecond}
	})
	ev := messages(t, h, body("", true)).Events()
	eq(t, joined(ev, "text"), "still here", "text")
	eq(t, last(ev).Event, "message_stop", "last")
}

func TestUnreachableBackendIs502(t *testing.T) {
	h := harness.Run(t, harness.Options{Configure: func(c *config.Config) { c.Backends[0].BaseURL = "http://127.0.0.1:1/v1" }}, okCompletion)
	r := messages(t, h, body("", false))
	eq(t, r.Status, 502, "status")
	eq(t, str(r.JSON(), "error", "type"), "api_error", "type")
}

// MARK: disconnects

func TestClientDisconnectCancelsTheUpstream(t *testing.T) {
	h := harness.Run(t, harness.Options{}, func(testkit.Seen, int) testkit.Reply { return testkit.Endless{Every: 20 * time.Millisecond} })
	ctx, cancel := context.WithCancel(context.Background())
	resp := streamRequest(t, ctx, h)
	sc := bufio.NewScanner(resp.Body)
	n := 0
	for sc.Scan() {
		if strings.Contains(sc.Text(), "text_delta") {
			if n++; n == 3 {
				break
			}
		}
	}
	eq(t, n, 3, "deltas read")
	cancel()
	resp.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for h.Stub.Disconnected() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	eq(t, h.Stub.Disconnected(), 1, "upstream disconnected")
}

// MARK: count_tokens

func TestCountTokensEstimates(t *testing.T) {
	h := harness.Run(t, harness.Options{}, okCompletion)
	text := strings.Repeat("word ", 400)
	r := request(t, h, "POST", "/v1/messages/count_tokens", `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"`+text+`"}]}`, nil, true)
	eq(t, r.Status, 200, "status")
	if n := num(r.JSON(), "input_tokens"); n <= 500 || n >= 600 {
		t.Errorf("estimate %d", n)
	}
	eq(t, len(h.Stub.ChatRequests()), 0, "no backend call")
	bad := request(t, h, "POST", "/v1/messages/count_tokens", `{"messages":[]}`, nil, true)
	eq(t, bad.Status, 400, "missing model")
}

// MARK: Anthropic backends

func TestAnthropicBackendsPassThrough(t *testing.T) {
	h := harness.Run(t, harness.Options{Configure: func(c *config.Config) {
		c.Backends = append(c.Backends, config.Backend{ID: "anth", Kind: config.Anthropic, BaseURL: c.Backends[0].BaseURL, APIKey: "sk-stub-secret"})
		c.Models = append(c.Models, config.Model{ID: "Claude", BackendID: "anth", Upstream: "claude-real-1"})
	}}, func(seen testkit.Seen, _ int) testkit.Reply {
		if strings.HasSuffix(seen.Path, "/messages/count_tokens") {
			return testkit.JSONR(404, `{"error":"nope"}`)
		}
		if seen.JSON()["stream"] == true {
			return testkit.RawStream{Text: "event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"héllo \U0001F600\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"}
		}
		if n, _ := jsonx.Int(seen.JSON()["max_tokens"]); n == 1 {
			return testkit.JSONReply{Status: 429, Body: `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, Header: map[string]string{"Retry-After": "30"}}
		}
		return testkit.JSONR(200, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"native"}],"model":"claude-real-1","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	b := `{"model":"Claude","max_tokens":10,"tools":[{"name":"T","input_schema":{"type":"object","properties":{"b":{},"a":{}}}}],"messages":[{"role":"user","content":"hi"}]}`
	r := request(t, h, "POST", "/v1/messages", b, map[string]string{"anthropic-beta": "x-1", "anthropic-version": "2023-06-01"}, true)
	eq(t, r.Status, 200, "status")
	eq(t, str(r.JSON(), "content", 0, "text"), "native", "text")
	reqs := h.Stub.Requests()
	seen := reqs[len(reqs)-1]
	eq(t, seen.Path, "/v1/messages", "path")
	eq(t, seen.Header.Get("x-api-key")+" "+seen.Header.Get("anthropic-beta"), "sk-stub-secret x-1", "headers")
	eq(t, str(seen.JSON(), "model"), "claude-real-1", "model")
	if !strings.Contains(string(seen.Body), `"properties":{"b":{},"a":{}}`) {
		t.Errorf("body reordered: %s", seen.Body)
	}

	s := messages(t, h, `{"model":"Claude","max_tokens":10,"stream":true,"messages":[]}`)
	eq(t, testkit.Names(s.Events()), []string{"message_start", "content_block_delta", "message_stop"}, "stream")
	eq(t, str(s.Events()[1].Data, "delta", "text"), "héllo \U0001F600", "utf8")

	e := messages(t, h, `{"model":"Claude","max_tokens":1,"messages":[]}`)
	eq(t, e.Status, 429, "429")
	eq(t, str(e.JSON(), "error", "type"), "rate_limit_error", "native type")
	eq(t, e.Header.Get("Retry-After"), "30", "retry-after")

	c := request(t, h, "POST", "/v1/messages/count_tokens", `{"model":"Claude","messages":[{"role":"user","content":"hello hello"}]}`, nil, true)
	eq(t, c.Status, 200, "count status")
	if _, ok := c.JSON()["input_tokens"]; !ok {
		t.Errorf("count: %s", c.Text())
	}
}
