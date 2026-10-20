package chat_test

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"codelink/internal/config"
	"codelink/internal/hooks"
	"codelink/internal/jsonx"
	"codelink/internal/testkit"
	"codelink/internal/testkit/harness"
)

// Port of Tests/CodeLinkEngineTests/Responses/ChatCompletionsTests.swift.

func chatBody(extra, model string, stream bool, messages string) string {
	if model == "" {
		model = "GLM"
	}
	if messages == "" {
		messages = `[{"role":"user","content":"hi"}]`
	}
	s := `{"model":"` + model + `","stream":` + strconv.FormatBool(stream) + `,"messages":` + messages
	if extra != "" {
		s += "," + extra
	}
	return s + "}"
}

func post(t *testing.T, h *harness.H, body string) testkit.HTTPReply {
	t.Helper()
	r, err := h.Request("POST", "/v1/chat/completions", body, nil, true)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	return r
}

func okChat(testkit.Seen, int) testkit.Reply { return testkit.JSONR(200, testkit.Text("ok")) }

func requests(l *testkit.Log) []hooks.Event {
	var out []hooks.Event
	for _, e := range l.All() {
		if e.Kind == "request" {
			out = append(out, e)
		}
	}
	return out
}

func lastRequest(t *testing.T, l *testkit.Log) hooks.Event {
	t.Helper()
	r := requests(l)
	if len(r) == 0 {
		t.Fatal("no request logged")
	}
	return r[len(r)-1]
}

func float(v any) float64 { f, _ := jsonx.Float(v); return f }
func num(v any) int       { n, _ := jsonx.Int(v); return n }

func types(v any) []string {
	var out []string
	for _, x := range jsonx.Arr(v) {
		out = append(out, jsonx.Type(x))
	}
	return out
}

func eq(a, b any) bool { return reflect.DeepEqual(jsonx.Marshal(a), jsonx.Marshal(b)) }

// Anthropic Messages stream events.
func aStart(input, cacheRead int) string {
	return fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-x","usage":{"input_tokens":%d,"output_tokens":1,"cache_read_input_tokens":%d}}}`, input, cacheRead)
}
func aBlock(i int, b string) string {
	return fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":%s}`, i, b)
}
func aDelta(i int, d string) string {
	return fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":%s}`, i, d)
}
func aStop(i int) string { return fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, i) }
func aText(i int, t string) string {
	return aDelta(i, `{"type":"text_delta","text":`+jsonx.String(t)+`}`)
}
func aEnd(reason string, output int) string {
	return fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":"%s"},"usage":{"output_tokens":%d}}`, reason, output)
}

const (
	aMessageStop = `{"type":"message_stop"}`
	aPing        = `{"type":"ping"}`
)

func anthropicConfig(c *config.Config) { c.Backends[0].Kind = config.Anthropic }

func boolp(b bool) *bool { return &b }

// openStream posts a streaming request and returns the open response.
func openStream(t *testing.T, h *harness.H, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", h.Port), strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testkit.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	return resp
}

// OpenAI-compatible backends.

func TestStreamPassesThroughVerbatim(t *testing.T) {
	payload := strings.Join([]string{
		"data: " + testkit.Chunk(`{"role":"assistant"}`, "", ""),
		": a comment the backend sent",
		"data: " + testkit.Content("Hel"),
		"data: " + testkit.Reasoning("hmm"),
		"data: " + testkit.Content("lo"),
		"data: " + testkit.Finish("stop"),
		"data: " + testkit.Usage(9, 3, 0),
		"data: [DONE]",
	}, "\n\n") + "\n\n"
	h := harness.Run(t, harness.Options{}, func(testkit.Seen, int) testkit.Reply { return testkit.RawStream{Text: payload} })
	r := post(t, h, chatBody(`"temperature":0.2,"stream_options":{"include_usage":true}`, "", true, ""))
	if r.Status != 200 || !strings.Contains(r.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d content-type %q", r.Status, r.Header.Get("Content-Type"))
	}
	if r.Text() != payload {
		t.Fatalf("not verbatim:\n%q\nwant\n%q", r.Text(), payload)
	}
	up := h.Stub.LastChat()
	if up["model"] != "glm-upstream" || up["stream"] != true || float(up["temperature"]) != 0.2 {
		t.Fatalf("upstream body: %v", up)
	}
	if jsonx.Get(up, "stream_options", "include_usage") != true {
		t.Fatalf("stream_options: %v", up)
	}
	seen := h.Stub.Requests()
	last := seen[len(seen)-1]
	if last.Header.Get("Authorization") != "Bearer sk-stub-secret" || last.Header.Get("Accept") != "text/event-stream" {
		t.Fatalf("upstream headers: %v", last.Header)
	}
	if e := lastRequest(t, h.Log); e.Route != "/v1/chat/completions" || e.Status != 200 {
		t.Fatalf("log: %+v", e)
	}
}

func TestChunksArriveAsTheBackendSendsThem(t *testing.T) {
	h := harness.Run(t, harness.Options{}, func(testkit.Seen, int) testkit.Reply {
		return testkit.SSEReply{Chunks: []string{testkit.Content("one"), testkit.Content("two"), testkit.Finish("")}, Done: true, Every: 200 * time.Millisecond}
	})
	resp := openStream(t, h, chatBody("", "", true, ""))
	defer resp.Body.Close()
	var stamps []time.Time
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: {") {
			stamps = append(stamps, time.Now())
		}
	}
	if len(stamps) != 3 {
		t.Fatalf("chunks: %d", len(stamps))
	}
	// Not buffered into one write: the first chunk is well ahead of the last.
	if d := stamps[2].Sub(stamps[0]); d <= 250*time.Millisecond {
		t.Fatalf("chunks arrived together (%v)", d)
	}
}

func TestNonStreamingAnswerIsForwardedAsIs(t *testing.T) {
	content := "plain"
	body := testkit.Completion{Content: &content, Reasoning: "why", Prompt: 5, CompletionN: 2}.String()
	h := harness.Run(t, harness.Options{}, func(testkit.Seen, int) testkit.Reply { return testkit.JSONR(200, body) })
	r := post(t, h, chatBody(`"max_tokens":50,"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]`, "", false, ""))
	if r.Status != 200 || r.Text() != body {
		t.Fatalf("%d %s", r.Status, r.Text())
	}
	up := h.Stub.LastChat()
	if up["stream"] != false || num(up["max_tokens"]) != 50 || jsonx.Get(up, "tools", 0, "function", "name") != "f" {
		t.Fatalf("upstream: %v", up)
	}
	if !eq(up["messages"], []any{map[string]any{"role": "user", "content": "hi"}}) {
		t.Fatalf("messages: %v", up["messages"])
	}
}

func TestModelNamesMapLikeTheMessagesRoute(t *testing.T) {
	h := harness.Run(t, harness.Options{}, okChat)
	for _, c := range [][2]string{{"GLM", "glm-upstream"}, {"Small", "small-upstream"}, {"opus", "big-upstream"},
		{"sonnet", "glm-upstream"}, {"haiku", "small-upstream"}, {"claude-haiku-4-5-20251001", "small-upstream"},
		{"claude-opus-5[1m]", "big-upstream"}, {"claude-sonnet-5", "glm-upstream"}, {"gpt-4o", "glm-upstream"}} {
		r := post(t, h, chatBody("", c[0], false, ""))
		if r.Status != 200 {
			t.Errorf("%s: status %d", c[0], r.Status)
		}
		if got := h.Stub.LastChat()["model"]; got != c[1] {
			t.Errorf("%s: upstream %v, want %s", c[0], got, c[1])
		}
	}
	h = harness.Run(t, harness.Options{Configure: func(c *config.Config) { c.Roles = config.Roles{Main: ""} }}, okChat)
	r := post(t, h, chatBody("", "gpt-4o", false, ""))
	if r.Status != 404 || jsonx.Get(r.JSON(), "error", "code") != "model_not_found" {
		t.Fatalf("no main: %d %s", r.Status, r.Text())
	}
}

func TestBadRequestsAreOpenAIShapedErrors(t *testing.T) {
	h := harness.Run(t, harness.Options{}, okChat)
	for _, body := range []string{"{nope", `{"messages":[{"role":"user","content":"x"}]}`, `{"model":"GLM"}`, `{"model":"GLM","messages":[]}`} {
		r := post(t, h, body)
		if r.Status != 400 || jsonx.Get(r.JSON(), "error", "type") != "invalid_request_error" {
			t.Errorf("%s: %d %s", body, r.Status, r.Text())
		}
		if jsonx.Str(jsonx.Get(r.JSON(), "error", "message")) == "" {
			t.Errorf("%s: no message", body)
		}
	}
	r, _ := h.Request("POST", "/v1/chat/completions", chatBody("", "", false, ""), nil, false)
	if r.Status != 401 {
		t.Fatalf("no auth: %d", r.Status)
	}
	if n := len(h.Stub.ChatRequests()); n != 0 {
		t.Fatalf("backend got %d requests", n)
	}
}

func TestBackendErrorsKeepTheirStatus(t *testing.T) {
	cases := []struct {
		status int
		body   string
		header map[string]string
		want   int
	}{
		{400, `{"error":{"message":"bad schema","type":"BadRequestError","param":"tools","code":400}}`, nil, 400},
		{404, "{\"error\":{\"message\":\"The model `x` does not exist.\",\"type\":\"NotFoundError\",\"code\":404}}", nil, 404},
		{429, `{"error":{"message":"slow down","type":"rate_limit"}}`, map[string]string{"retry-after": "11"}, 429},
		{503, `{"error":{"message":"overloaded"}}`, nil, 503},
		{500, `{"error":{"message":"boom"}}`, nil, 500},
	}
	for _, c := range cases {
		h := harness.Run(t, harness.Options{}, func(testkit.Seen, int) testkit.Reply {
			return testkit.JSONReply{Status: c.status, Body: c.body, Header: c.header}
		})
		for _, stream := range []bool{false, true} {
			r := post(t, h, chatBody("", "", stream, ""))
			if r.Status != c.want || r.Text() != c.body {
				t.Errorf("%d stream=%v: %d %s", c.status, stream, r.Status, r.Text())
			}
			if retry := c.header["retry-after"]; retry != "" && r.Header.Get("Retry-After") != retry {
				t.Errorf("%d: retry-after %q", c.status, r.Header.Get("Retry-After"))
			}
			if e := lastRequest(t, h.Log); e.Status != c.want {
				t.Errorf("%d: logged %d", c.status, e.Status)
			}
		}
	}
	// The engine's own credentials failing is a gateway problem, not the client's.
	h := harness.Run(t, harness.Options{}, func(testkit.Seen, int) testkit.Reply {
		return testkit.JSONR(401, `{"error":{"message":"Incorrect API key sk-live-abcdef123456"}}`)
	})
	r := post(t, h, chatBody("", "", false, ""))
	if r.Status != 502 || jsonx.Get(r.JSON(), "error", "code") != "upstream_auth_error" || strings.Contains(r.Text(), "abcdef") {
		t.Fatalf("401: %d %s", r.Status, r.Text())
	}
	// A body that is not an OpenAI error is wrapped, and scrubbed.
	h = harness.Run(t, harness.Options{}, func(testkit.Seen, int) testkit.Reply { return testkit.JSONR(500, "engine died at 10.1.2.3") })
	r = post(t, h, chatBody("", "", false, ""))
	if r.Status != 500 || jsonx.Get(r.JSON(), "error", "type") != "server_error" || strings.Contains(r.Text(), "10.1.2.3") {
		t.Fatalf("plain 500: %d %s", r.Status, r.Text())
	}
	h = harness.Run(t, harness.Options{Configure: func(c *config.Config) { c.Backends[0].BaseURL = "http://127.0.0.1:1/v1" }}, okChat)
	r = post(t, h, chatBody("", "", false, ""))
	if r.Status != 502 || jsonx.Get(r.JSON(), "error", "code") != "upstream_unreachable" {
		t.Fatalf("unreachable: %d %s", r.Status, r.Text())
	}
}

func TestClientDisconnectCancelsTheUpstream(t *testing.T) {
	h := harness.Run(t, harness.Options{}, func(testkit.Seen, int) testkit.Reply { return testkit.Endless{Every: 20 * time.Millisecond} })
	resp := openStream(t, h, chatBody("", "", true, ""))
	sc := bufio.NewScanner(resp.Body)
	n := 0
	for n < 3 && sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: {") {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("read %d chunks", n)
	}
	resp.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for h.Stub.Disconnected() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if d := h.Stub.Disconnected(); d != 1 {
		t.Fatalf("backend disconnects: %d", d)
	}
}

// Documents and images.

func TestImagesForAModelWithoutVisionAreDescribedOrNoted(t *testing.T) {
	// media port landed
	png := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("\x03", 64)))
	messages := `[{"role":"user","content":[{"type":"text","text":"what is this"},
	   {"type":"image_url","image_url":{"url":"data:image/png;base64,` + png + `","detail":"low"}}]}]`
	describer := &testkit.Describer{}
	h := harness.Run(t, harness.Options{Hooks: hooks.Hooks{Images: describer}, Configure: func(c *config.Config) {
		c.Roles.Vision = "Big"
		c.Models[0].Vision = boolp(false)
	}}, okChat)
	post(t, h, chatBody("", "", false, messages))
	parts := jsonx.Get(h.Stub.LastChat(), "messages", 0, "content")
	if !reflect.DeepEqual(types(parts), []string{"text", "text"}) || jsonx.Get(parts, 1, "text") != "[image: a cat on a mat]" {
		t.Fatalf("described: %v", parts)
	}
	if describer.Calls() != 1 {
		t.Fatalf("describer calls: %d", describer.Calls())
	}

	h = harness.Run(t, harness.Options{Configure: func(c *config.Config) { c.Models[0].Vision = boolp(false) }}, okChat)
	post(t, h, chatBody("", "", false, messages))
	if got := jsonx.Get(h.Stub.LastChat(), "messages", 0, "content", 1, "text"); got != "[image omitted: GLM has no vision and no vision model is configured]" {
		t.Fatalf("noted: %v", got)
	}

	// A vision model keeps its images untouched.
	h = harness.Run(t, harness.Options{Configure: func(c *config.Config) { c.Models[0].Vision = boolp(true) }}, okChat)
	post(t, h, chatBody("", "", false, messages))
	part := jsonx.Get(h.Stub.LastChat(), "messages", 0, "content", 1)
	if jsonx.Type(part) != "image_url" || jsonx.Get(part, "image_url", "url") != "data:image/png;base64,"+png || jsonx.Get(part, "image_url", "detail") != "low" {
		t.Fatalf("kept: %v", part)
	}
}

func TestPDFFilePartsAreReadAsDocuments(t *testing.T) {
	// media port landed
	reader := &testkit.DocReader{}
	messages := `[{"role":"user","content":[{"type":"text","text":"summarise"},
	   {"type":"file","file":{"filename":"report.pdf","file_data":"data:application/pdf;base64,JVBERi0xLjQK"}},
	   {"type":"file","file":{"file_id":"file-9"}},
	   {"type":"image_url","image_url":{"url":"data:application/pdf;base64,JVBERi0xLjQK"}}]}]`
	h := harness.Run(t, harness.Options{Hooks: hooks.Hooks{Documents: reader}, Configure: func(c *config.Config) { c.Models[0].Vision = boolp(true) }}, okChat)
	r := post(t, h, chatBody("", "", false, messages))
	if r.Status != 200 {
		t.Fatalf("status %d", r.Status)
	}
	parts := jsonx.Arr(jsonx.Get(h.Stub.LastChat(), "messages", 0, "content"))
	for _, p := range parts {
		if jsonx.Type(p) != "text" {
			t.Fatalf("non-text part: %v", p)
		}
	}
	if len(parts) != 4 {
		t.Fatalf("parts: %v", parts)
	}
	if got := jsonx.Get(parts[1], "text"); got != "[document: report.pdf]\nEXTRACTED 9 bytes of application/pdf" {
		t.Fatalf("document: %v", got)
	}
	if !strings.Contains(jsonx.Str(jsonx.Get(parts[2], "text")), "could not be read") {
		t.Fatalf("file_id: %v", parts[2])
	}
	if !strings.HasPrefix(jsonx.Str(jsonx.Get(parts[3], "text")), "[document: document]") {
		t.Fatalf("pdf as image_url: %v", parts[3])
	}
	calls := reader.Calls()
	found := false
	for _, c := range calls {
		found = found || c.Title == "report.pdf"
	}
	if len(calls) != 2 || !found {
		t.Fatalf("reader calls: %+v", calls)
	}
}

// Models.

func TestModelsAnswerInBothShapes(t *testing.T) {
	h := harness.Run(t, harness.Options{}, okChat)
	openai, _ := h.Request("GET", "/v1/models", "", nil, true)
	j := openai.JSON()
	if openai.Status != 200 || j["object"] != "list" {
		t.Fatalf("openai: %d %s", openai.Status, openai.Text())
	}
	var ids []string
	for _, d := range jsonx.Arr(j["data"]) {
		ids = append(ids, jsonx.Str(jsonx.Get(d, "id")))
	}
	if !reflect.DeepEqual(ids, []string{"GLM", "Small", "Big", "sonnet", "opus", "haiku"}) {
		t.Fatalf("ids: %v", ids)
	}
	first := jsonx.Get(j, "data", 0)
	if jsonx.Get(first, "object") != "model" || jsonx.Get(first, "owned_by") != "code-link" {
		t.Fatalf("entry: %v", first)
	}
	if _, ok := jsonx.Int(jsonx.Get(first, "created")); !ok {
		t.Fatalf("created: %v", first)
	}
	if _, ok := j["has_more"]; ok {
		t.Fatal("openai list has has_more")
	}

	anthropic, _ := h.Request("GET", "/v1/models", "", map[string]string{"anthropic-version": "2023-06-01"}, true)
	a := anthropic.JSON()
	if jsonx.Get(a, "data", 0, "type") != "model" || a["has_more"] != false {
		t.Fatalf("anthropic: %s", anthropic.Text())
	}
	if _, ok := a["object"]; ok {
		t.Fatal("anthropic list has object")
	}
	var aids []string
	for _, d := range jsonx.Arr(a["data"]) {
		aids = append(aids, jsonx.Str(jsonx.Get(d, "id")))
	}
	if !reflect.DeepEqual(aids, ids) {
		t.Fatalf("anthropic ids: %v", aids)
	}
}

// Anthropic backends.

func TestAnthropicBackendGetsATranslatedRequest(t *testing.T) {
	messages := `[{"role":"system","content":"be brief"},{"role":"developer","content":[{"type":"text","text":"and kind"}]},
	 {"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]},
	 {"role":"assistant","content":"checking","tool_calls":[{"id":"call_1","type":"function","function":{"name":"ls","arguments":"{\"dir\":\"/\"}"}},
	                                                       {"id":"call_2","type":"function","function":{"name":"pwd","arguments":""}}]},
	 {"role":"tool","tool_call_id":"call_1","content":"a b c"},
	 {"role":"tool","tool_call_id":"call_2","content":[{"type":"text","text":"/home"}]},
	 {"role":"user","content":"thanks"}]`
	extra := `"max_tokens":77,"temperature":0.5,"top_p":0.8,"stop":["END"],"user":"u7","parallel_tool_calls":false,"tool_choice":"required",
	"tools":[{"type":"function","function":{"name":"ls","description":"list","parameters":{"type":"object","properties":{"dir":{"type":"string"}}}}}]`
	h := harness.Run(t, harness.Options{Configure: anthropicConfig}, func(testkit.Seen, int) testkit.Reply {
		return testkit.JSONR(200, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`)
	})
	r := post(t, h, chatBody(extra, "", false, messages))
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Text())
	}
	seen := h.Stub.Requests()
	last := seen[len(seen)-1]
	if last.Path != "/v1/messages" {
		t.Fatalf("path %s", last.Path)
	}
	if last.Header.Get("x-api-key") != "sk-stub-secret" || last.Header.Get("anthropic-version") != "2023-06-01" {
		t.Fatalf("headers: %v", last.Header)
	}
	up := last.JSON()
	if up["model"] != "glm-upstream" || num(up["max_tokens"]) != 77 || up["stream"] != false {
		t.Fatalf("upstream: %v", up)
	}
	if up["system"] != "be brief\nand kind" {
		t.Fatalf("system: %v", up["system"])
	}
	if float(up["temperature"]) != 0.5 || float(up["top_p"]) != 0.8 || !eq(up["stop_sequences"], []any{"END"}) || jsonx.Get(up, "metadata", "user_id") != "u7" {
		t.Fatalf("sampling: %v", up)
	}
	if !eq(up["tool_choice"], map[string]any{"type": "any", "disable_parallel_tool_use": true}) {
		t.Fatalf("tool_choice: %v", up["tool_choice"])
	}
	if jsonx.Get(up, "tools", 0, "name") != "ls" || jsonx.Get(up, "tools", 0, "input_schema", "properties", "dir") == nil {
		t.Fatalf("tools: %v", up["tools"])
	}
	msgs := jsonx.Arr(up["messages"])
	var roles []string
	for _, m := range msgs {
		roles = append(roles, jsonx.Str(jsonx.Get(m, "role")))
	}
	if !reflect.DeepEqual(roles, []string{"user", "assistant", "user", "user"}) {
		t.Fatalf("roles: %v", roles)
	}
	if jsonx.Get(msgs[0], "content", 1, "source", "type") != "base64" || jsonx.Get(msgs[0], "content", 1, "source", "media_type") != "image/png" {
		t.Fatalf("image: %v", msgs[0])
	}
	if !reflect.DeepEqual(types(jsonx.Get(msgs[1], "content")), []string{"text", "tool_use", "tool_use"}) {
		t.Fatalf("assistant: %v", msgs[1])
	}
	if jsonx.Get(msgs[1], "content", 1, "input", "dir") != "/" || !eq(jsonx.Get(msgs[1], "content", 2, "input"), map[string]any{}) {
		t.Fatalf("tool inputs: %v", msgs[1])
	}
	// Both tool results ride in one user turn.
	if !reflect.DeepEqual(types(jsonx.Get(msgs[2], "content")), []string{"tool_result", "tool_result"}) {
		t.Fatalf("tool results: %v", msgs[2])
	}
	if jsonx.Get(msgs[2], "content", 0, "tool_use_id") != "call_1" || jsonx.Get(msgs[2], "content", 1, "content") != "/home" {
		t.Fatalf("tool results: %v", msgs[2])
	}
	if jsonx.Get(msgs[3], "content", 0, "text") != "thanks" {
		t.Fatalf("last: %v", msgs[3])
	}
}

func TestAnthropicDefaultsAndRefusals(t *testing.T) {
	h := harness.Run(t, harness.Options{Configure: anthropicConfig}, func(testkit.Seen, int) testkit.Reply {
		return testkit.JSONR(200, `{"type":"message","content":[],"stop_reason":"end_turn","usage":{}}`)
	})
	post(t, h, chatBody("", "", false, ""))
	seen := h.Stub.Requests()
	up := seen[len(seen)-1].JSON()
	if num(up["max_tokens"]) != 4096 {
		t.Fatalf("max_tokens: %v", up["max_tokens"])
	}
	if _, ok := up["system"]; ok {
		t.Fatalf("system: %v", up["system"])
	}
	none := post(t, h, `{"model":"GLM","messages":[{"role":"system","content":"only"}]}`)
	if none.Status != 400 {
		t.Fatalf("system only: %d", none.Status)
	}
}

func TestAnthropicMessageBecomesAChatCompletion(t *testing.T) {
	reply := `{"id":"msg_9","type":"message","role":"assistant","model":"claude-x","stop_reason":"tool_use",
	 "content":[{"type":"thinking","thinking":"hmm","signature":"s"},{"type":"text","text":"Let me "},{"type":"text","text":"check."},
	            {"type":"tool_use","id":"toolu_1","name":"ls","input":{"dir":"/"}}],
	 "usage":{"input_tokens":20,"cache_creation_input_tokens":5,"cache_read_input_tokens":75,"output_tokens":11}}`
	h := harness.Run(t, harness.Options{Configure: anthropicConfig}, func(testkit.Seen, int) testkit.Reply { return testkit.JSONR(200, reply) })
	r := post(t, h, chatBody("", "", false, ""))
	j := r.JSON()
	if j["object"] != "chat.completion" || j["model"] != "GLM" || !strings.HasPrefix(jsonx.Str(j["id"]), "chatcmpl-") {
		t.Fatalf("completion: %s", r.Text())
	}
	msg := jsonx.Get(j, "choices", 0, "message")
	if jsonx.Get(msg, "role") != "assistant" || jsonx.Get(msg, "content") != "Let me check." || jsonx.Get(msg, "reasoning_content") != "hmm" {
		t.Fatalf("message: %v", msg)
	}
	if jsonx.Get(msg, "tool_calls", 0, "id") != "toolu_1" || jsonx.Get(msg, "tool_calls", 0, "function", "name") != "ls" {
		t.Fatalf("tool call: %v", msg)
	}
	if got := jsonx.Get(msg, "tool_calls", 0, "function", "arguments"); got != `{"dir":"/"}` {
		t.Fatalf("arguments: %v", got)
	}
	if jsonx.Get(j, "choices", 0, "finish_reason") != "tool_calls" {
		t.Fatalf("finish: %v", j["choices"])
	}
	u := j["usage"]
	if num(jsonx.Get(u, "prompt_tokens")) != 100 || num(jsonx.Get(u, "completion_tokens")) != 11 || num(jsonx.Get(u, "total_tokens")) != 111 {
		t.Fatalf("usage: %v", u)
	}
	if num(jsonx.Get(u, "prompt_tokens_details", "cached_tokens")) != 75 {
		t.Fatalf("cached: %v", u)
	}
}

func TestAnthropicStreamBecomesChatChunks(t *testing.T) {
	events := []string{
		aStart(25, 10), aPing,
		aBlock(0, `{"type":"thinking","thinking":""}`), aDelta(0, `{"type":"thinking_delta","thinking":"hmm"}`),
		aDelta(0, `{"type":"signature_delta","signature":"s"}`), aStop(0),
		aBlock(1, `{"type":"text","text":""}`), aText(1, "Hel"), aText(1, "lo"), aStop(1),
		aBlock(2, `{"type":"tool_use","id":"toolu_1","name":"ls","input":{}}`),
		aDelta(2, `{"type":"input_json_delta","partial_json":"{\"dir\""}`), aDelta(2, `{"type":"input_json_delta","partial_json":":\"/\"}"}`), aStop(2),
		aEnd("tool_use", 12), aMessageStop,
	}
	h := harness.Run(t, harness.Options{Configure: anthropicConfig}, func(testkit.Seen, int) testkit.Reply {
		return testkit.SSEReply{Chunks: events}
	})
	r := post(t, h, chatBody(`"stream_options":{"include_usage":true}`, "", true, ""))
	if r.Status != 200 || !strings.Contains(r.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d %q", r.Status, r.Header.Get("Content-Type"))
	}
	seen := h.Stub.Requests()
	last := seen[len(seen)-1]
	if last.JSON()["stream"] != true || last.Header.Get("Accept") != "text/event-stream" {
		t.Fatalf("upstream: %v %v", last.JSON()["stream"], last.Header)
	}
	lines := testkit.DataLines(r.Text())
	if len(lines) == 0 || lines[len(lines)-1] != "[DONE]" {
		t.Fatalf("no [DONE]: %s", r.Text())
	}
	var chunks []map[string]any
	ids := map[string]bool{}
	for _, l := range lines[:len(lines)-1] {
		c := jsonx.ParseObject([]byte(l))
		if c["object"] != "chat.completion.chunk" || c["model"] != "GLM" {
			t.Fatalf("chunk: %s", l)
		}
		ids[jsonx.Str(c["id"])] = true
		chunks = append(chunks, c)
	}
	if len(ids) != 1 {
		t.Fatalf("ids: %v", ids)
	}
	if jsonx.Get(chunks[0], "choices", 0, "delta", "role") != "assistant" {
		t.Fatalf("first: %v", chunks[0])
	}
	joined := func(key string) string {
		var b strings.Builder
		for _, c := range chunks {
			b.WriteString(jsonx.Str(jsonx.Get(c, "choices", 0, "delta", key)))
		}
		return b.String()
	}
	if joined("content") != "Hello" || joined("reasoning_content") != "hmm" {
		t.Fatalf("content %q reasoning %q", joined("content"), joined("reasoning_content"))
	}
	var tools []any
	for _, c := range chunks {
		if tc := jsonx.Get(c, "choices", 0, "delta", "tool_calls", 0); tc != nil {
			tools = append(tools, tc)
		}
	}
	if len(tools) < 2 || jsonx.Get(tools[0], "id") != "toolu_1" || jsonx.Get(tools[0], "function", "name") != "ls" || num(jsonx.Get(tools[0], "index")) != 0 {
		t.Fatalf("tools: %v", tools)
	}
	var args strings.Builder
	for _, tc := range tools[1:] {
		args.WriteString(jsonx.Str(jsonx.Get(tc, "function", "arguments")))
	}
	if args.String() != `{"dir":"/"}` {
		t.Fatalf("arguments: %q", args.String())
	}
	var finish any
	for _, c := range chunks {
		if f := jsonx.Get(c, "choices", 0, "finish_reason"); f != nil {
			finish = f
			break
		}
	}
	if finish != "tool_calls" {
		t.Fatalf("finish: %v", finish)
	}
	usage := chunks[len(chunks)-1]
	if choices, ok := usage["choices"].([]any); !ok || len(choices) != 0 {
		t.Fatalf("usage chunk choices: %v", usage)
	}
	u := usage["usage"]
	if num(jsonx.Get(u, "prompt_tokens")) != 35 || num(jsonx.Get(u, "completion_tokens")) != 12 || num(jsonx.Get(u, "total_tokens")) != 47 {
		t.Fatalf("usage: %v", u)
	}
	if num(jsonx.Get(u, "prompt_tokens_details", "cached_tokens")) != 10 {
		t.Fatalf("cached: %v", u)
	}

	// No usage chunk unless asked for.
	h = harness.Run(t, harness.Options{Configure: anthropicConfig}, func(testkit.Seen, int) testkit.Reply {
		return testkit.SSE(aStart(25, 0), aBlock(0, `{"type":"text","text":""}`), aText(0, "x"), aStop(0), aEnd("end_turn", 9), aMessageStop)
	})
	r = post(t, h, chatBody("", "", true, ""))
	if strings.Contains(r.Text(), `"usage"`) || !strings.HasSuffix(r.Text(), "data: [DONE]\n\n") {
		t.Fatalf("no usage: %s", r.Text())
	}
	if !strings.Contains(r.Text(), `"finish_reason":"stop"`) {
		t.Fatalf("finish stop: %s", r.Text())
	}
}

func TestAnthropicStreamsThatBreakEndWithAnErrorFrame(t *testing.T) {
	cases := []struct {
		name  string
		reply testkit.Reply
	}{
		{"cut", testkit.SSEReply{Chunks: []string{aStart(25, 0), aBlock(0, `{"type":"text","text":""}`), aText(0, "par")}}},
		{"error event", testkit.SSE(aStart(25, 0), `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded at 10.0.0.9"}}`)},
	}
	for _, c := range cases {
		h := harness.Run(t, harness.Options{Configure: anthropicConfig}, func(testkit.Seen, int) testkit.Reply { return c.reply })
		r := post(t, h, chatBody("", "", true, ""))
		text := r.Text()
		if r.Status != 200 {
			t.Errorf("%s: status %d", c.name, r.Status)
		}
		if !strings.Contains(text, `"error":{`) {
			t.Errorf("%s: no error frame: %s", c.name, text)
		}
		if strings.Contains(text, `finish_reason":"stop`) {
			t.Errorf("%s: finished as stop", c.name)
		}
		if !strings.HasSuffix(text, "data: [DONE]\n\n") {
			t.Errorf("%s: no [DONE]", c.name)
		}
		if strings.Contains(text, "10.0.0.9") {
			t.Errorf("%s: address leaked", c.name)
		}
		if e := lastRequest(t, h.Log); e.Status != 502 {
			t.Errorf("%s: logged %d", c.name, e.Status)
		}
	}
}

func TestAnthropicErrorsBecomeOpenAIErrors(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   int
		code   any
	}{
		{400, `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: required"}}`, 400, nil},
		{429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, 429, "rate_limit_exceeded"},
		{529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, 529, "server_is_overloaded"},
		{401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, 502, nil},
	}
	for _, c := range cases {
		h := harness.Run(t, harness.Options{Configure: anthropicConfig}, func(testkit.Seen, int) testkit.Reply {
			return testkit.JSONReply{Status: c.status, Body: c.body, Header: map[string]string{"retry-after": "4"}}
		})
		r := post(t, h, chatBody("", "", false, ""))
		j := r.JSON()
		if r.Status != c.want {
			t.Errorf("%d: status %d", c.status, r.Status)
		}
		if _, ok := j["type"]; ok || jsonx.Str(jsonx.Get(j, "error", "message")) == "" {
			t.Errorf("%d: shape %s", c.status, r.Text())
		}
		if got := jsonx.Get(j, "error", "code"); got != c.code {
			t.Errorf("%d: code %v: %s", c.status, got, r.Text())
		}
		if c.status == 429 && r.Header.Get("Retry-After") != "4" {
			t.Errorf("429: retry-after %q", r.Header.Get("Retry-After"))
		}
	}
}

func TestImagesGoToAnAnthropicModelAndDocumentsAreRead(t *testing.T) {
	// media port landed
	reader := &testkit.DocReader{}
	messages := `[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,/9j/"}},
	  {"type":"file","file":{"filename":"a.pdf","file_data":"data:application/pdf;base64,JVBERi0xLjQK"}}]}]`
	h := harness.Run(t, harness.Options{Hooks: hooks.Hooks{Documents: reader}, Configure: anthropicConfig}, func(testkit.Seen, int) testkit.Reply {
		return testkit.JSONR(200, `{"type":"message","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{}}`)
	})
	post(t, h, chatBody("", "", false, messages))
	seen := h.Stub.Requests()
	content := jsonx.Get(seen[len(seen)-1].JSON(), "messages", 0, "content")
	if !reflect.DeepEqual(types(content), []string{"image", "text"}) {
		t.Fatalf("content: %v", content)
	}
	if got := jsonx.Get(content, 1, "text"); got != "[document: a.pdf]\nEXTRACTED 9 bytes of application/pdf" {
		t.Fatalf("document: %v", got)
	}
}

// Beyond the Swift suite: the translated stream's keepalives are SSE
// comments, and a whole message for a streaming request is replayed as
// chunks.

func TestAnthropicStreamKeepalivesAreComments(t *testing.T) {
	h := harness.Run(t, harness.Options{Configure: anthropicConfig}, func(testkit.Seen, int) testkit.Reply {
		return testkit.SSEReply{Chunks: []string{aStart(25, 0), aBlock(0, `{"type":"text","text":""}`), aText(0, "x"), aStop(0), aEnd("end_turn", 9), aMessageStop},
			Done: true, After: 350 * time.Millisecond}
	})
	r := post(t, h, chatBody("", "", true, ""))
	if !strings.HasPrefix(r.Text(), ": keepalive\n\n") || strings.Contains(r.Text(), "event:") {
		t.Fatalf("keepalives: %q", r.Text())
	}
	if !strings.HasSuffix(r.Text(), "data: [DONE]\n\n") || lastRequest(t, h.Log).Status != 200 {
		t.Fatalf("stream: %q", r.Text())
	}
}

func TestAnthropicWholeMessageForAStreamIsReplayed(t *testing.T) {
	h := harness.Run(t, harness.Options{Configure: anthropicConfig}, func(testkit.Seen, int) testkit.Reply {
		return testkit.JSONR(200, `{"type":"message","content":[{"type":"text","text":"hi there"},{"type":"tool_use","id":"toolu_1","name":"ls","input":{"dir":"/"}}],"stop_reason":"tool_use","usage":{"input_tokens":4,"output_tokens":2}}`)
	})
	r := post(t, h, chatBody(`"stream_options":{"include_usage":true}`, "", true, ""))
	if !strings.Contains(r.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("content-type %q", r.Header.Get("Content-Type"))
	}
	lines := testkit.DataLines(r.Text())
	if len(lines) != 6 || lines[5] != "[DONE]" {
		t.Fatalf("lines: %v", lines)
	}
	if jsonx.Get(jsonx.ParseObject([]byte(lines[1])), "choices", 0, "delta", "content") != "hi there" {
		t.Fatalf("content: %s", lines[1])
	}
	tc := jsonx.Get(jsonx.ParseObject([]byte(lines[2])), "choices", 0, "delta", "tool_calls", 0)
	if jsonx.Get(tc, "id") != "toolu_1" || num(jsonx.Get(tc, "index")) != 0 || jsonx.Get(tc, "function", "arguments") != `{"dir":"/"}` {
		t.Fatalf("tool call: %s", lines[2])
	}
	if jsonx.Get(jsonx.ParseObject([]byte(lines[3])), "choices", 0, "finish_reason") != "tool_calls" {
		t.Fatalf("finish: %s", lines[3])
	}
	if num(jsonx.Get(jsonx.ParseObject([]byte(lines[4])), "usage", "total_tokens")) != 6 {
		t.Fatalf("usage: %s", lines[4])
	}
}
