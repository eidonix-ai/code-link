package responses_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
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

// Helpers (port of ResponsesHarness.swift).

func post(t *testing.T, h *harness.H, body string) testkit.HTTPReply {
	t.Helper()
	r, err := h.Request("POST", "/v1/responses", body, nil, true)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	return r
}

// codexFixture is a real Codex 0.163 request (testdata/codex, long texts
// shortened, identifiers replaced), with edit applied.
func codexFixture(t *testing.T, name string, edit func(map[string]any)) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "codex", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	j := jsonx.ParseObject(data)
	if j == nil {
		t.Fatalf("fixture %s is not a JSON object", name)
	}
	if edit != nil {
		edit(j)
	}
	return jsonx.String(j)
}

// body is a Responses request; input "" = "hi", model "" = GLM.
func body(input, model string, stream bool, extra string) string {
	if input == "" {
		input = `"hi"`
	}
	if model == "" {
		model = "GLM"
	}
	s := `{"model":"` + model + `","stream":` + strconv.FormatBool(stream) + `,"input":` + input
	if extra != "" {
		s += "," + extra
	}
	return s + "}"
}

// data is the data of every event with this name.
func data(ev []testkit.SSEEvent, name string) []map[string]any {
	var out []map[string]any
	for _, e := range ev {
		if e.Event == name {
			out = append(out, e.Data)
		}
	}
	return out
}

// completedOutput is the items of a response's final output, from the
// completed event.
func completedOutput(ev []testkit.SSEEvent) []any {
	d := data(ev, "response.completed")
	if len(d) == 0 {
		return nil
	}
	return jsonx.Arr(jsonx.Get(d[0], "response", "output"))
}

func types(items []any) []string {
	var out []string
	for _, it := range items {
		out = append(out, jsonx.Type(it))
	}
	return out
}

func roles(msgs []any) []string {
	var out []string
	for _, m := range msgs {
		out = append(out, jsonx.Str(jsonx.Get(m, "role")))
	}
	return out
}

func requests(h *harness.H) []hooks.Event {
	var out []hooks.Event
	for _, e := range h.Log.All() {
		if e.Kind == "request" {
			out = append(out, e)
		}
	}
	return out
}

func lastRequest(t *testing.T, h *harness.H) hooks.Event {
	t.Helper()
	r := requests(h)
	if len(r) == 0 {
		t.Fatal("no request logged")
	}
	return r[len(r)-1]
}

func num(v any) int {
	n, ok := jsonx.Int(v)
	if !ok {
		return -1
	}
	return n
}

func messages(h *harness.H) []any { return jsonx.Arr(h.Stub.LastChat()["messages"]) }

func okStream(testkit.Seen, int) testkit.Reply {
	return testkit.SSE(testkit.Content("ok"), testkit.Finish(""))
}

func okJSON(testkit.Seen, int) testkit.Reply { return testkit.JSONR(200, testkit.Text("ok")) }

func run(t *testing.T, backend func(testkit.Seen, int) testkit.Reply) *harness.H {
	return harness.Run(t, harness.Options{}, backend)
}

func configure(t *testing.T, f func(*config.Config), backend func(testkit.Seen, int) testkit.Reply) *harness.H {
	return harness.Run(t, harness.Options{Configure: f}, backend)
}

func str(s string) *string { return &s }

func eq[T comparable](t *testing.T, what string, got, want []T) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// System and developer messages.

func TestLeadingDeveloperMessagesMergeIntoOneSystemMessage(t *testing.T) {
	h := run(t, okStream)
	b := codexFixture(t, "main_turn", nil)
	input := jsonx.Arr(jsonx.ParseObject([]byte(b))["input"])
	var texts []string
	for i := range 2 {
		for _, p := range jsonx.Arr(jsonx.Get(input[i], "content")) {
			texts = append(texts, jsonx.Str(jsonx.Get(p, "text")))
		}
	}
	r := post(t, h, b)
	if r.Status != 200 {
		t.Fatalf("status %d: %s", r.Status, r.Text())
	}
	up := h.Stub.LastChat()
	msgs := jsonx.Arr(up["messages"])
	eq(t, "roles", roles(msgs), []string{"system", "user", "user"})
	// Exactly the developer texts, one blank line apart.
	if got := jsonx.Str(jsonx.Get(msgs[0], "content")); got != strings.Join(texts, "\n\n") {
		t.Errorf("system = %q", got)
	}
	if got := jsonx.Str(jsonx.Get(msgs[2], "content")); got != "run it" {
		t.Errorf("last user = %q", got)
	}
	if up["model"] != "glm-upstream" {
		t.Errorf("model = %v", up["model"])
	}
}

func TestInstructionsAndTheLeadingRunMergeExactBytes(t *testing.T) {
	h := run(t, okStream)
	input := `[{"type":"additional_tools","role":"developer","tools":[]},
	 {"type":"message","role":"developer","content":[{"type":"input_text","text":"B"},{"type":"input_text","text":"C"}]},
	 {"type":"message","role":"system","content":"D"},
	 {"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
	 {"type":"message","role":"assistant","content":[{"type":"output_text","text":"yo"}]},
	 {"type":"message","role":"developer","content":[{"type":"input_text","text":"later"}]},
	 {"type":"message","role":"user","content":"next"}]`
	post(t, h, body(input, "", true, `"instructions":"A"`))
	got := jsonx.String(messages(h))
	want := testkit.Compact(`[{"role":"system","content":"A\n\nB\n\nC\n\nD"},{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"<developer>\nlater\n</developer>"},{"role":"user","content":"next"}]`)
	if got != want {
		t.Errorf("messages\n got %s\nwant %s", got, want)
	}
}

// Tools.

func TestAdditionalToolsBecomeChatToolsAndTheOutputFormat(t *testing.T) {
	h := run(t, func(testkit.Seen, int) testkit.Reply {
		return testkit.SSE(testkit.Content(`{"outcome":"allow"}`), testkit.Finish(""))
	})
	r := post(t, h, codexFixture(t, "auto_review", nil))
	if r.Status != 200 {
		t.Fatalf("status %d: %s", r.Status, r.Text())
	}
	up := h.Stub.LastChat()
	fns := map[string]any{}
	for _, tool := range jsonx.Arr(up["tools"]) {
		fns[jsonx.Str(jsonx.Get(tool, "function", "name"))] = jsonx.Get(tool, "function")
	}
	// The implicit functions namespace is dropped: exec, wait.
	if len(fns) != 2 || fns["exec"] == nil || fns["wait"] == nil {
		t.Errorf("tools = %v", fns)
	}
	// exec is a freeform tool: one string parameter, the grammar in its description.
	params := jsonx.Get(fns["exec"], "parameters")
	if jsonx.Get(params, "properties", "input", "type") != "string" || jsonx.String(jsonx.Get(params, "required")) != `["input"]` {
		t.Errorf("exec parameters = %v", jsonx.String(params))
	}
	if !strings.Contains(jsonx.Str(jsonx.Get(fns["exec"], "description")), "lark grammar") {
		t.Errorf("exec description lacks the grammar")
	}

	msgs := jsonx.Arr(up["messages"])
	if jsonx.Get(msgs[0], "role") != "system" {
		t.Errorf("first message is not system")
	}
	for _, m := range msgs {
		if jsonx.Get(m, "role") == "developer" {
			t.Errorf("a developer message reached the backend")
		}
	}
	if n := strings.Count(jsonx.Str(jsonx.Get(msgs[0], "content")), "<permissions instructions>"); n != 1 {
		t.Errorf("permissions instructions appear %d times", n)
	}

	rf := up["response_format"]
	if jsonx.Type(rf) != "json_schema" || jsonx.Get(rf, "json_schema", "name") != "codex_output_schema" {
		t.Errorf("response_format = %s", jsonx.String(rf))
	}
	if strict, ok := jsonx.BoolOK(jsonx.Get(rf, "json_schema", "strict")); !ok || strict {
		t.Errorf("strict = %v", jsonx.Get(rf, "json_schema", "strict"))
	}
	if jsonx.Get(rf, "json_schema", "schema", "properties", "outcome") == nil {
		t.Errorf("schema lost")
	}
	if up["reasoning_effort"] != "low" {
		t.Errorf("reasoning_effort = %v", up["reasoning_effort"])
	}
	if _, ok := up["parallel_tool_calls"]; ok && up["tools"] == nil {
		t.Errorf("parallel_tool_calls without tools")
	}
	for _, k := range []string{"verbosity", "text", "store", "include", "prompt_cache_key", "reasoning", "input"} {
		if _, ok := up[k]; ok {
			t.Errorf("%s leaked into the chat request", k)
		}
	}
	out := completedOutput(r.Events())
	text := jsonx.Str(jsonx.Get(out[len(out)-1], "content", 0, "text"))
	if jsonx.Get(jsonx.ParseObject([]byte(text)), "outcome") != "allow" {
		t.Errorf("verdict = %q", text)
	}
}

func TestCustomToolCallRoundTripsWithItsEvents(t *testing.T) {
	chunks := []string{
		testkit.ToolCall(0, "call_x", "exec", ""),
		testkit.ToolCall(0, "", "", `{"input":"// @exec: {}\nawait tools.`),
		testkit.ToolCall(0, "", "", `exec_command({cmd:'ls'})"}`),
		testkit.Finish("tool_calls"),
	}
	h := run(t, func(testkit.Seen, int) testkit.Reply { return testkit.SSE(chunks...) })
	r := post(t, h, codexFixture(t, "auto_review", nil))
	ev := r.Events()
	if slices.Contains(testkit.Names(ev), "response.failed") {
		t.Fatalf("failed: %s", r.Text())
	}
	added := data(ev, "response.output_item.added")
	if len(added) != 1 {
		t.Fatalf("added = %d", len(added))
	}
	item := jsonx.Obj(added[0]["item"])
	if jsonx.Type(item) != "custom_tool_call" || item["name"] != "exec" {
		t.Errorf("item = %s", jsonx.String(item))
	}
	if item["namespace"] != "functions" || item["call_id"] != "call_x" {
		t.Errorf("item = %s", jsonx.String(item))
	}
	if len(data(ev, "response.function_call_arguments.delta")) != 0 {
		t.Errorf("function_call_arguments.delta for a custom tool")
	}
	want := "// @exec: {}\nawait tools.exec_command({cmd:'ls'})"
	deltas := data(ev, "response.custom_tool_call_input.delta")
	done := data(ev, "response.custom_tool_call_input.done")
	if len(deltas) != 1 || deltas[0]["delta"] != want {
		t.Errorf("deltas = %v", deltas)
	}
	if len(done) != 1 || done[0]["input"] != want {
		t.Errorf("done = %v", done)
	}
	if len(deltas) == 1 && deltas[0]["item_id"] != item["id"] {
		t.Errorf("delta item_id %v != %v", deltas[0]["item_id"], item["id"])
	}
	itemDone := data(ev, "response.output_item.done")
	fin := itemDone[len(itemDone)-1]["item"]
	if jsonx.Type(fin) != "custom_tool_call" || jsonx.Get(fin, "input") != want || jsonx.Get(fin, "status") != "completed" {
		t.Errorf("final item = %s", jsonx.String(fin))
	}
	if out := completedOutput(ev); len(out) == 0 || jsonx.Type(out[0]) != "custom_tool_call" {
		t.Errorf("completed output = %v", jsonx.String(out))
	}
}

func TestNamespacedToolCallMapsBackWithItsNamespace(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprint("stream=", stream), func(t *testing.T) {
			call := testkit.ToolCall(0, "call_n", "multi_agent_v1__close_agent", `{"target":"a1"}`)
			message := `[{"id":"call_n","type":"function","function":{"name":"multi_agent_v1__close_agent","arguments":"{\"target\":\"a1\"}"}}]`
			h := run(t, func(testkit.Seen, int) testkit.Reply {
				if stream {
					return testkit.SSE(call, testkit.Finish("tool_calls"))
				}
				return testkit.JSONR(200, testkit.Completion{ToolCalls: message, Finish: "tool_calls"}.String())
			})
			r := post(t, h, codexFixture(t, "main_turn", func(j map[string]any) { j["stream"] = stream }))
			if r.Status != 200 {
				t.Fatalf("status %d: %s", r.Status, r.Text())
			}
			var names []string
			for _, tool := range jsonx.Arr(h.Stub.LastChat()["tools"]) {
				names = append(names, jsonx.Str(jsonx.Get(tool, "function", "name")))
			}
			for _, n := range []string{"multi_agent_v1__close_agent", "exec_command", "web_search"} {
				if !slices.Contains(names, n) {
					t.Errorf("tools lack %s: %v", n, names)
				}
			}
			var item any
			if stream {
				d := data(r.Events(), "response.output_item.done")
				item = d[len(d)-1]["item"]
			} else {
				item = jsonx.Get(r.JSON(), "output", 0)
			}
			if jsonx.Type(item) != "function_call" || jsonx.Get(item, "name") != "close_agent" {
				t.Errorf("item = %s", jsonx.String(item))
			}
			if jsonx.Get(item, "namespace") != "multi_agent_v1" || jsonx.Get(item, "arguments") != `{"target":"a1"}` {
				t.Errorf("item = %s", jsonx.String(item))
			}
			if jsonx.Get(item, "call_id") != "call_n" {
				t.Errorf("call_id = %v", jsonx.Get(item, "call_id"))
			}
		})
	}
}

func TestToolFollowupHistoryBecomesAToolCallAndItsResult(t *testing.T) {
	h := run(t, okStream)
	post(t, h, codexFixture(t, "tool_followup", nil))
	msgs := messages(h)
	eq(t, "roles", roles(msgs), []string{"system", "user", "user", "assistant", "tool"})
	call := jsonx.Get(msgs[3], "tool_calls", 0)
	if jsonx.Get(call, "id") != "call_1" || jsonx.Get(call, "function", "name") != "exec_command" {
		t.Errorf("call = %s", jsonx.String(call))
	}
	if !strings.Contains(jsonx.Str(jsonx.Get(call, "function", "arguments")), "escalated-test") {
		t.Errorf("arguments = %v", jsonx.Get(call, "function", "arguments"))
	}
	if c, ok := jsonx.Obj(msgs[3])["content"]; !ok || c != nil {
		t.Errorf("assistant content = %v, want null", c)
	}
	if jsonx.Get(msgs[4], "tool_call_id") != "call_1" || !strings.Contains(jsonx.Str(jsonx.Get(msgs[4], "content")), "Process exited with code 0") {
		t.Errorf("tool message = %s", jsonx.String(msgs[4]))
	}
}

func TestHistoryOfNamespacedAndCustomCallsUsesTheFlatNames(t *testing.T) {
	input := `[{"role":"user","content":"go"},
	 {"type":"custom_tool_call","call_id":"c1","name":"exec","namespace":"functions","input":"ls -la"},
	 {"type":"custom_tool_call_output","call_id":"c1","output":"files"},
	 {"type":"function_call","call_id":"c2","name":"wait","namespace":"multi_agent_v1","arguments":"{}"}]`
	tools := `"tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"}]},
	 {"type":"namespace","name":"multi_agent_v1","tools":[{"type":"function","name":"wait"}]}]`
	h := run(t, okStream)
	post(t, h, body(input, "", true, tools))
	msgs := messages(h)
	if got := jsonx.Get(msgs[1], "tool_calls", 0, "function", "name"); got != "exec" {
		t.Errorf("custom call name = %v", got)
	}
	if got := jsonx.Get(msgs[1], "tool_calls", 0, "function", "arguments"); got != `{"input":"ls -la"}` {
		t.Errorf("custom call arguments = %v", got)
	}
	if jsonx.Get(msgs[2], "role") != "tool" {
		t.Errorf("msgs[2] = %s", jsonx.String(msgs[2]))
	}
	if got := jsonx.Get(msgs[3], "tool_calls", 0, "function", "name"); got != "multi_agent_v1__wait" {
		t.Errorf("namespaced call name = %v", got)
	}
}

func TestToolCallDeltasAreKeyedByIndex(t *testing.T) {
	chunks := []string{
		testkit.ToolCall(0, "call_a", "f", `{"x"`),
		testkit.ToolCall(0, "", "", ":1}"),
		testkit.ToolCall(1, "", "g", "{}"),
		testkit.ToolCall(0, "call_a", "", ""),
		testkit.Finish("tool_calls"),
	}
	h := run(t, func(testkit.Seen, int) testkit.Reply { return testkit.SSE(chunks...) })
	r := post(t, h, body("", "", true, `"tools":[{"type":"function","name":"f"},{"type":"function","name":"g"}]`))
	out := completedOutput(r.Events())
	if len(out) != 2 {
		t.Fatalf("output = %s", jsonx.String(out))
	}
	if jsonx.Get(out[0], "name") != "f" || jsonx.Get(out[0], "arguments") != `{"x":1}` || jsonx.Get(out[0], "call_id") != "call_a" {
		t.Errorf("out[0] = %s", jsonx.String(out[0]))
	}
	if jsonx.Get(out[1], "name") != "g" || jsonx.Get(out[1], "arguments") != "{}" || jsonx.Str(jsonx.Get(out[1], "call_id")) == "" {
		t.Errorf("out[1] = %s", jsonx.String(out[1]))
	}
}

// Request fields.

func TestRequestFieldsMapToChatFields(t *testing.T) {
	h := run(t, okStream)
	extra := `"reasoning":{"effort":"high","summary":"auto"},"max_output_tokens":99,"temperature":0.3,"top_p":0.9,"user":"u1",
	 "parallel_tool_calls":false,"tool_choice":"required","store":false,"include":["reasoning.encrypted_content"],
	 "text":{"format":{"type":"json_object"},"verbosity":"low"},
	 "tools":[{"type":"function","name":"f","description":"d","parameters":{"type":"object"},"strict":true}]`
	post(t, h, body("", "", true, extra))
	up := h.Stub.LastChat()
	if up["reasoning_effort"] != "high" || num(up["max_tokens"]) != 99 || jsonx.String(up["temperature"]) != "0.3" || jsonx.String(up["top_p"]) != "0.9" {
		t.Errorf("sampling fields: %s", jsonx.String(up))
	}
	if up["user"] != "u1" || up["parallel_tool_calls"] != false || up["tool_choice"] != "required" {
		t.Errorf("tool fields: %s", jsonx.String(up))
	}
	if jsonx.Get(up, "response_format", "type") != "json_object" {
		t.Errorf("response_format = %v", up["response_format"])
	}
	if up["stream"] != true || jsonx.Get(up, "stream_options", "include_usage") != true {
		t.Errorf("stream fields: %v %v", up["stream"], up["stream_options"])
	}
	if jsonx.Get(up, "tools", 0, "function", "strict") != true || jsonx.Get(up, "tools", 0, "function", "description") != "d" {
		t.Errorf("tools = %s", jsonx.String(up["tools"]))
	}
	for _, k := range []string{"store", "include", "text"} {
		if _, ok := up[k]; ok {
			t.Errorf("%s leaked", k)
		}
	}
}

func TestEffortNoneUsesTheModelsThinkingOff(t *testing.T) {
	h := configure(t, func(c *config.Config) {
		c.Models[0].ThinkingOff = map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}}
	}, okStream)
	post(t, h, body("", "", true, `"reasoning":{"effort":"none"}`))
	up := h.Stub.LastChat()
	if _, ok := up["reasoning_effort"]; ok {
		t.Errorf("reasoning_effort = %v", up["reasoning_effort"])
	}
	if jsonx.Get(up, "chat_template_kwargs", "enable_thinking") != false {
		t.Errorf("chat_template_kwargs = %v", up["chat_template_kwargs"])
	}
}

func TestReasoningItemsReplayAsReasoningContent(t *testing.T) {
	input := `[{"role":"user","content":"q"},
	 {"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"S"}],"content":[{"type":"reasoning_text","text":"raw thoughts"}],"encrypted_content":"xyz"},
	 {"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
	 {"type":"function_call_output","call_id":"c1","output":"ok"},
	 {"type":"reasoning","summary":[{"type":"summary_text","text":"only summary"}]},
	 {"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]},
	 {"type":"reasoning","summary":[{"type":"summary_text","text":"orphan"}]},
	 {"role":"user","content":"next"}]`
	h := run(t, okStream)
	post(t, h, body(input, "", true, ""))
	msgs := messages(h)
	eq(t, "roles", roles(msgs), []string{"user", "assistant", "tool", "assistant", "user"})
	if jsonx.Get(msgs[1], "reasoning_content") != "raw thoughts" || jsonx.Get(msgs[1], "reasoning") != "raw thoughts" {
		t.Errorf("msgs[1] = %s", jsonx.String(msgs[1]))
	}
	if jsonx.Get(msgs[1], "tool_calls") == nil {
		t.Errorf("msgs[1] lost its tool calls")
	}
	if jsonx.Get(msgs[3], "reasoning_content") != "only summary" || jsonx.Get(msgs[3], "content") != "done" {
		t.Errorf("msgs[3] = %s", jsonx.String(msgs[3]))
	}
	if _, ok := jsonx.Obj(msgs[4])["reasoning_content"]; ok {
		t.Errorf("orphan reasoning attached to a user message")
	}
}

func TestStatefulRequestsAreRefused(t *testing.T) {
	h := run(t, okStream)
	for _, extra := range []string{`"previous_response_id":"resp_1"`, `"store":true`} {
		r := post(t, h, body("", "", true, extra))
		if r.Status != 400 {
			t.Errorf("%s: status %d", extra, r.Status)
		}
		if jsonx.Get(r.JSON(), "error", "type") != "invalid_request_error" {
			t.Errorf("%s: %s", extra, r.Text())
		}
		if !strings.HasSuffix(jsonx.Str(jsonx.Get(r.JSON(), "error", "code")), "_not_supported") {
			t.Errorf("%s: code %v", extra, jsonx.Get(r.JSON(), "error", "code"))
		}
	}
	if n := len(h.Stub.ChatRequests()); n != 0 {
		t.Errorf("%d chat requests reached the backend", n)
	}
	// Codex's own shape.
	ok := post(t, h, body("", "", true, `"store":false`))
	if names := testkit.Names(ok.Events()); ok.Status != 200 || len(names) == 0 || names[len(names)-1] != "response.completed" {
		t.Errorf("store:false: %d %v", ok.Status, names)
	}
	bad := post(t, h, "{nope")
	if bad.Status != 400 || !strings.Contains(jsonx.Str(jsonx.Get(bad.JSON(), "error", "message")), "JSON") {
		t.Errorf("bad JSON: %d %s", bad.Status, bad.Text())
	}
	none := post(t, h, `{"model":"GLM","input":[]}`)
	if none.Status != 400 || jsonx.Get(none.JSON(), "error", "code") != "invalid_prompt" {
		t.Errorf("empty input: %d %s", none.Status, none.Text())
	}
	if noModel := post(t, h, `{"input":"hi"}`); noModel.Status != 400 {
		t.Errorf("no model: %d", noModel.Status)
	}
}

// Model routing.

func TestModelNamesRoute(t *testing.T) {
	h := configure(t, func(c *config.Config) { c.Roles.Codex = "Small" }, okJSON)
	for _, c := range [][2]string{{"Big", "big-upstream"}, {"gpt-5-codex", "small-upstream"}, {"opus", "big-upstream"},
		{"sonnet", "glm-upstream"}, {"claude-haiku-4-5", "small-upstream"}, {"Small", "small-upstream"}} {
		r := post(t, h, body("", c[0], false, ""))
		if r.Status != 200 {
			t.Errorf("%s: status %d", c[0], r.Status)
		}
		if got := h.Stub.LastChat()["model"]; got != c[1] {
			t.Errorf("%s → %v, want %s", c[0], got, c[1])
		}
		if got := r.JSON()["model"]; got != c[0] {
			t.Errorf("%s: response model %v", c[0], got)
		}
	}

	h = run(t, okJSON)
	post(t, h, body("", "gpt-5-codex", true, ""))
	if got := h.Stub.LastChat()["model"]; got != "glm-upstream" {
		t.Errorf("no codex role → %v", got)
	}

	h = configure(t, func(c *config.Config) { c.Roles = config.Roles{} }, okJSON)
	r := post(t, h, body("", "gpt-5-codex", true, ""))
	if r.Status != 404 || jsonx.Get(r.JSON(), "error", "code") != "model_not_found" {
		t.Errorf("no main: %d %s", r.Status, r.Text())
	}
}

// autoMode is a fake decision head that only names its model.
type autoMode struct{ model config.Model }

func (autoMode) IsClassifierRequest(map[string]any) bool                { return false }
func (autoMode) Respond(context.Context, map[string]any) map[string]any { return nil }
func (a autoMode) DecisionModel() (config.Model, bool)                  { return a.model, true }

func TestAutoReviewRunsOnTheDecisionModelGeneratively(t *testing.T) {
	// roles.decision
	h := configure(t, func(c *config.Config) { c.Roles.Decision = "Big"; c.Roles.Codex = "Small" }, okStream)
	r := post(t, h, codexFixture(t, "auto_review", nil))
	if r.Status != 200 {
		t.Errorf("status %d", r.Status)
	}
	up := h.Stub.LastChat()
	if up["model"] != "big-upstream" {
		t.Errorf("model = %v", up["model"])
	}
	if _, ok := up["logprobs"]; ok {
		t.Errorf("logprobs sent")
	}
	if _, ok := up["max_tokens"]; ok {
		t.Errorf("max_tokens sent")
	}
	if jsonx.Get(up, "response_format", "type") != "json_schema" {
		t.Errorf("response_format = %v", up["response_format"])
	}

	// The decision head's own model wins when the hook has one.
	w := 400_000
	big := config.Model{ID: "Big", BackendID: "stub", Upstream: "big-upstream", ContextWindow: &w}
	h = harness.Run(t, harness.Options{Hooks: hooks.Hooks{AutoMode: autoMode{big}},
		Configure: func(c *config.Config) { c.Roles.Codex = "Small" }}, okStream)
	post(t, h, codexFixture(t, "auto_review", nil))
	if got := h.Stub.LastChat()["model"]; got != "big-upstream" {
		t.Errorf("hook model → %v", got)
	}
	if n := len(h.Stub.ChatRequests()); n != 1 {
		t.Errorf("%d chat requests", n)
	}

	// Else the Codex model, else main.
	h = configure(t, func(c *config.Config) { c.Roles.Codex = "Small" }, okStream)
	post(t, h, codexFixture(t, "auto_review", nil))
	if got := h.Stub.LastChat()["model"]; got != "small-upstream" {
		t.Errorf("codex role → %v", got)
	}
	h = run(t, okStream)
	post(t, h, codexFixture(t, "auto_review", nil))
	if got := h.Stub.LastChat()["model"]; got != "glm-upstream" {
		t.Errorf("main → %v", got)
	}
}

func TestAnthropicBackendsAreRefusedClearly(t *testing.T) {
	h := configure(t, func(c *config.Config) { c.Backends[0].Kind = config.Anthropic }, okStream)
	r := post(t, h, body("", "", true, ""))
	if r.Status != 400 || jsonx.Get(r.JSON(), "error", "code") != "unsupported_backend" {
		t.Errorf("%d %s", r.Status, r.Text())
	}
	if n := len(h.Stub.Requests()); n != 0 {
		t.Errorf("%d requests reached the backend", n)
	}
}

// Streaming.

func TestStreamingEventOrder(t *testing.T) {
	chunks := []string{
		testkit.Reasoning("think "), testkit.Reasoning("hard"),
		testkit.Content("an"), testkit.Content("swer"),
		testkit.ToolCall(0, "call_a", "exec_command", ""),
		testkit.ToolCall(0, "", "", `{"cmd":`),
		testkit.ToolCall(0, "", "", `"ls"}`),
		testkit.Finish("tool_calls"),
		testkit.Usage(100, 20, 64),
	}
	h := run(t, func(testkit.Seen, int) testkit.Reply { return testkit.SSE(chunks...) })
	r := post(t, h, body("", "", true, ""))
	if r.Status != 200 || r.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("%d %s", r.Status, r.Header.Get("Content-Type"))
	}
	ev := r.Events()
	eq(t, "events", testkit.Names(ev), []string{
		"response.created", "response.in_progress",
		"response.output_item.added", "response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta", "response.reasoning_text.delta",
		"response.reasoning_summary_text.delta", "response.reasoning_text.delta",
		"response.reasoning_summary_text.done", "response.reasoning_text.done", "response.output_item.done",
		"response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done", "response.output_item.done",
		"response.output_item.added",
		"response.function_call_arguments.delta", "response.function_call_arguments.delta",
		"response.function_call_arguments.done", "response.output_item.done",
		"response.completed",
	})
	// Every payload carries its event name as type and a rising sequence number.
	for i, e := range ev {
		if jsonx.Type(e.Data) != e.Event {
			t.Errorf("event %s has type %v", e.Event, e.Data["type"])
		}
		if num(e.Data["sequence_number"]) != i {
			t.Errorf("event %d has sequence_number %v", i, e.Data["sequence_number"])
		}
	}
	created := jsonx.Get(ev[0].Data, "response")
	if jsonx.Get(created, "status") != "in_progress" || jsonx.Get(created, "model") != "GLM" || !strings.HasPrefix(jsonx.Str(jsonx.Get(created, "id")), "resp_") {
		t.Errorf("created = %s", jsonx.String(created))
	}
	var idx []int
	for _, d := range data(ev, "response.output_item.added") {
		idx = append(idx, num(d["output_index"]))
	}
	eq(t, "output indexes", idx, []int{0, 1, 2})

	reasoning := data(ev, "response.output_item.done")[0]["item"]
	if jsonx.Type(reasoning) != "reasoning" || jsonx.Get(reasoning, "content", 0, "text") != "think hard" {
		t.Errorf("reasoning = %s", jsonx.String(reasoning))
	}
	if jsonx.Get(reasoning, "summary", 0, "type") != "summary_text" || jsonx.Get(reasoning, "summary", 0, "text") != "think hard" {
		t.Errorf("reasoning summary = %s", jsonx.String(reasoning))
	}
	if got := data(ev, "response.reasoning_text.done")[0]["text"]; got != "think hard" {
		t.Errorf("reasoning_text.done = %v", got)
	}

	done := jsonx.Get(data(ev, "response.completed")[0], "response")
	if jsonx.Get(done, "status") != "completed" || jsonx.Get(done, "output_text") != "answer" {
		t.Errorf("completed = %s", jsonx.String(done))
	}
	eq(t, "output types", types(jsonx.Arr(jsonx.Get(done, "output"))), []string{"reasoning", "message", "function_call"})
	if jsonx.Get(done, "output", 1, "content", 0, "text") != "answer" {
		t.Errorf("message = %s", jsonx.String(jsonx.Get(done, "output", 1)))
	}
	if jsonx.Get(done, "output", 2, "arguments") != `{"cmd":"ls"}` || jsonx.Get(done, "output", 2, "status") != "completed" {
		t.Errorf("call = %s", jsonx.String(jsonx.Get(done, "output", 2)))
	}
	u := jsonx.Get(done, "usage")
	if num(jsonx.Get(u, "input_tokens")) != 100 || num(jsonx.Get(u, "output_tokens")) != 20 || num(jsonx.Get(u, "total_tokens")) != 120 {
		t.Errorf("usage = %s", jsonx.String(u))
	}
	if num(jsonx.Get(u, "input_tokens_details", "cached_tokens")) != 64 {
		t.Errorf("cached = %s", jsonx.String(u))
	}
	if last := lastRequest(t, h); last.Route != "/v1/responses" || last.Status != 200 {
		t.Errorf("logged %+v", last)
	}
}

func TestUsageCarriesReasoningTokens(t *testing.T) {
	usage := `{"id":"c","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":64},"completion_tokens_details":{"reasoning_tokens":12}}}`
	h := run(t, func(testkit.Seen, int) testkit.Reply {
		return testkit.SSE(testkit.Content("x"), testkit.Finish(""), usage)
	})
	ev := post(t, h, body("", "", true, "")).Events()
	u := jsonx.Get(data(ev, "response.completed")[0], "response", "usage")
	if num(jsonx.Get(u, "output_tokens_details", "reasoning_tokens")) != 12 || num(jsonx.Get(u, "input_tokens_details", "cached_tokens")) != 64 {
		t.Errorf("usage = %s", jsonx.String(u))
	}
	if num(jsonx.Get(u, "total_tokens")) != 120 {
		t.Errorf("total = %v", jsonx.Get(u, "total_tokens"))
	}
}

func TestThinkTagsInContentBecomeReasoning(t *testing.T) {
	chunks := []string{testkit.Content("<thi"), testkit.Content("nk>let me see</th"), testkit.Content("ink>The answer"), testkit.Finish("")}
	h := run(t, func(testkit.Seen, int) testkit.Reply { return testkit.SSE(chunks...) })
	out := completedOutput(post(t, h, body("", "", true, "")).Events())
	eq(t, "types", types(out), []string{"reasoning", "message"})
	if len(out) == 2 && (jsonx.Get(out[0], "content", 0, "text") != "let me see" || jsonx.Get(out[1], "content", 0, "text") != "The answer") {
		t.Errorf("output = %s", jsonx.String(out))
	}
}

func TestLengthEndsIncomplete(t *testing.T) {
	h := run(t, func(testkit.Seen, int) testkit.Reply {
		return testkit.SSE(testkit.Content("cut"), testkit.Finish("length"))
	})
	ev := post(t, h, body("", "", true, "")).Events()
	names := testkit.Names(ev)
	if names[len(names)-1] != "response.incomplete" {
		t.Fatalf("events %v", names)
	}
	r := jsonx.Get(data(ev, "response.incomplete")[0], "response")
	if jsonx.Get(r, "status") != "incomplete" || jsonx.Get(r, "incomplete_details", "reason") != "max_output_tokens" {
		t.Errorf("incomplete = %s", jsonx.String(r))
	}
}

func TestMissingDoneAfterAFinishReasonStillCompletes(t *testing.T) {
	h := run(t, func(testkit.Seen, int) testkit.Reply {
		return testkit.SSEReply{Chunks: []string{testkit.Content("hi"), testkit.Finish("")}}
	})
	names := testkit.Names(post(t, h, body("", "", true, "")).Events())
	if names[len(names)-1] != "response.completed" {
		t.Errorf("events %v", names)
	}
}

func TestLargeLinesPassIntact(t *testing.T) {
	big := strings.Repeat("x", 2<<20)
	h := run(t, func(testkit.Seen, int) testkit.Reply { return testkit.SSE(testkit.Content(big), testkit.Finish("")) })
	out := completedOutput(post(t, h, body("", "", true, "")).Events())
	if len(out) == 0 || len(jsonx.Str(jsonx.Get(out[0], "content", 0, "text"))) != len(big) {
		t.Errorf("large line lost")
	}
}

func TestTruncatedStreamsFail(t *testing.T) {
	cases := []struct {
		name  string
		reply testkit.Reply
	}{
		{"no finish", testkit.SSEReply{Chunks: []string{testkit.Content("partial")}}},
		{"done without finish", testkit.SSE(testkit.Content("partial"))},
		{"empty", testkit.SSEReply{}},
		{"mid tool call", testkit.SSEReply{Chunks: []string{testkit.ToolCall(0, "call_1", "f", `{"a":`)}}},
		{"cut line", testkit.RawStream{Text: "data: " + testkit.Content("par") + "\n\ndata: {\"choi"}},
		{"error frame", testkit.SSE(testkit.Content("par"), `{"error":{"message":"engine died at 10.0.0.5","code":500}}`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := run(t, func(testkit.Seen, int) testkit.Reply { return c.reply })
			r := post(t, h, body("", "", true, `"tools":[{"type":"function","name":"f"}]`))
			ev := r.Events()
			names := testkit.Names(ev)
			if r.Status != 200 {
				t.Errorf("status %d", r.Status)
			}
			if len(names) == 0 || names[len(names)-1] != "response.failed" {
				t.Fatalf("events %v", names)
			}
			if slices.Contains(names, "response.completed") {
				t.Errorf("completed too")
			}
			failed := jsonx.Get(data(ev, "response.failed")[0], "response")
			if jsonx.Get(failed, "status") != "failed" || jsonx.Str(jsonx.Get(failed, "error", "message")) == "" {
				t.Errorf("failed = %s", jsonx.String(failed))
			}
			if jsonx.Str(jsonx.Get(failed, "error", "code")) == "" {
				t.Errorf("no error code")
			}
			if strings.Contains(r.Text(), "10.0.0.5") {
				t.Errorf("address leaked")
			}
			// Only items that finished before the failure are reported.
			for _, it := range jsonx.Arr(jsonx.Get(failed, "output")) {
				if jsonx.Get(it, "status") != "completed" {
					t.Errorf("unfinished item reported: %s", jsonx.String(it))
				}
			}
			if s := lastRequest(t, h).Status; s != 502 {
				t.Errorf("logged status %d", s)
			}
		})
	}
}

func TestAStreamThatGoesQuietGetsCommentKeepalives(t *testing.T) {
	tuning := testkit.FastTuning()
	tuning.PingInterval = 25 * time.Millisecond
	h := harness.Run(t, harness.Options{Tuning: &tuning}, func(testkit.Seen, int) testkit.Reply {
		return testkit.SSEReply{Chunks: []string{testkit.Content("slow"), testkit.Finish("")}, Done: true, Every: 150 * time.Millisecond}
	})
	r := post(t, h, body("", "", true, ""))
	if !strings.Contains(r.Text(), ": keepalive\n\n") {
		t.Errorf("no keepalive comment")
	}
	if strings.Contains(r.Text(), "event: ping") {
		t.Errorf("ping event sent")
	}
	if names := testkit.Names(r.Events()); names[len(names)-1] != "response.completed" {
		t.Errorf("events %v", names)
	}
}

func TestClientDisconnectCancelsTheUpstream(t *testing.T) {
	h := run(t, func(testkit.Seen, int) testkit.Reply { return testkit.Endless{Every: 20 * time.Millisecond} })
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/v1/responses", h.Port), strings.NewReader(body("", "", true, "")))
	req.Header.Set("Authorization", "Bearer "+testkit.Token)
	tr := &http.Transport{}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(resp.Body)
	n := 0
	for n < 3 && sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "event:") && strings.Contains(line, "output_text.delta") {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("read %d deltas", n)
	}
	resp.Body.Close()
	tr.CloseIdleConnections()
	deadline := time.Now().Add(5 * time.Second)
	for h.Stub.Disconnected() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if d := h.Stub.Disconnected(); d != 1 {
		t.Errorf("upstream disconnected %d times", d)
	}
}

// Non-streaming.

func TestNonStreamingResponse(t *testing.T) {
	calls := `[{"id":"call_n","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]`
	h := run(t, func(testkit.Seen, int) testkit.Reply {
		return testkit.JSONR(200, testkit.Completion{Content: str("<think>hmm</think>done"), ToolCalls: calls, Finish: "tool_calls",
			Prompt: 30, CompletionN: 8, Cached: 10}.String())
	})
	r := post(t, h, body("", "", false, `"tools":[{"type":"function","name":"f"}]`))
	if r.Status != 200 || !strings.Contains(r.Header.Get("Content-Type"), "json") {
		t.Fatalf("%d %s", r.Status, r.Header.Get("Content-Type"))
	}
	j := r.JSON()
	if j["object"] != "response" || j["status"] != "completed" || j["model"] != "GLM" {
		t.Errorf("response = %s", r.Text())
	}
	eq(t, "types", types(jsonx.Arr(j["output"])), []string{"reasoning", "message", "function_call"})
	if jsonx.Get(j, "output", 0, "content", 0, "text") != "hmm" || j["output_text"] != "done" {
		t.Errorf("text: %s", r.Text())
	}
	if jsonx.Get(j, "output", 2, "name") != "f" || jsonx.Get(j, "output", 2, "call_id") != "call_n" {
		t.Errorf("call: %s", r.Text())
	}
	if num(jsonx.Get(j, "usage", "input_tokens")) != 30 || num(jsonx.Get(j, "usage", "input_tokens_details", "cached_tokens")) != 10 {
		t.Errorf("usage: %s", jsonx.String(j["usage"]))
	}
	up := h.Stub.LastChat()
	if _, ok := up["stream_options"]; up["stream"] != false || ok {
		t.Errorf("stream fields: %v %v", up["stream"], up["stream_options"])
	}
}

func TestNonStreamingLengthIsIncomplete(t *testing.T) {
	h := run(t, func(testkit.Seen, int) testkit.Reply {
		return testkit.JSONR(200, testkit.Completion{Content: str("cut"), Finish: "length"}.String())
	})
	j := post(t, h, body("", "", false, "")).JSON()
	if j["status"] != "incomplete" || jsonx.Get(j, "incomplete_details", "reason") != "max_output_tokens" {
		t.Errorf("response = %s", jsonx.String(j))
	}
}

func TestABackendThatIgnoresStreamStillAnswersAStream(t *testing.T) {
	h := run(t, func(testkit.Seen, int) testkit.Reply {
		return testkit.JSONR(200, testkit.Completion{Content: str("plain"), Reasoning: "r"}.String())
	})
	ev := post(t, h, body("", "", true, "")).Events()
	names := testkit.Names(ev)
	if names[len(names)-1] != "response.completed" {
		t.Fatalf("events %v", names)
	}
	eq(t, "types", types(completedOutput(ev)), []string{"reasoning", "message"})
	if got := jsonx.Get(data(ev, "response.completed")[0], "response", "output_text"); got != "plain" {
		t.Errorf("output_text = %v", got)
	}
}

// Structured output.

func TestFencedJSONIsCleaned(t *testing.T) {
	fenced := "```json\n{\"outcome\":\"deny\"}\n```"
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprint("stream=", stream), func(t *testing.T) {
			h := run(t, func(testkit.Seen, int) testkit.Reply {
				if stream {
					return testkit.SSE(testkit.Content(fenced[:12]), testkit.Content(fenced[12:]), testkit.Finish(""))
				}
				return testkit.JSONR(200, testkit.Text(fenced))
			})
			r := post(t, h, codexFixture(t, "auto_review", func(j map[string]any) { j["stream"] = stream }))
			var text any
			if stream {
				text = jsonx.Get(data(r.Events(), "response.completed")[0], "response", "output_text")
			} else {
				text = r.JSON()["output_text"]
			}
			if text != `{"outcome":"deny"}` {
				t.Errorf("output_text = %v", text)
			}
			if stream {
				var b strings.Builder
				for _, d := range data(r.Events(), "response.output_text.delta") {
					b.WriteString(jsonx.Str(d["delta"]))
				}
				if b.String() != `{"outcome":"deny"}` {
					t.Errorf("deltas = %q", b.String())
				}
			}
		})
	}
}

// Errors before output.

func TestErrorsBeforeOutputAreRealHTTPErrors(t *testing.T) {
	type tc struct {
		status int
		body   string
		header map[string]string
		want   int
		code   string
	}
	cases := []tc{
		{400, `{"error":{"message":"This model's maximum context length is 131072 tokens. However, you requested 140000 tokens. See http://203.0.113.3:8000/help","type":"BadRequestError","code":400}}`,
			nil, 400, "context_length_exceeded"},
		{400, `{"error":{"message":"bad schema"}}`, nil, 400, "invalid_prompt"},
		{422, `{"detail":"nope"}`, nil, 400, "invalid_prompt"},
		{401, "{}", nil, 502, "upstream_auth_error"},
		{403, "{}", nil, 502, "upstream_auth_error"},
		{429, "{}", map[string]string{"retry-after": "7"}, 429, "rate_limit_exceeded"},
		{503, "{}", map[string]string{"retry-after": "3"}, 503, "server_is_overloaded"},
		{500, "secret internals at 10.1.2.3", nil, 500, "upstream_error"},
	}
	for _, stream := range []bool{true, false} {
		for _, c := range cases {
			t.Run(fmt.Sprintf("%d stream=%v", c.status, stream), func(t *testing.T) {
				h := run(t, func(testkit.Seen, int) testkit.Reply {
					return testkit.JSONReply{Status: c.status, Body: c.body, Header: c.header}
				})
				r := post(t, h, body("", "", stream, ""))
				if r.Status != c.want {
					t.Errorf("status %d, want %d", r.Status, c.want)
				}
				if strings.Contains(r.Header.Get("Content-Type"), "event-stream") {
					t.Errorf("answered as a stream")
				}
				if got := jsonx.Get(r.JSON(), "error", "code"); got != c.code {
					t.Errorf("code %v: %s", got, r.Text())
				}
				if jsonx.Str(jsonx.Get(r.JSON(), "error", "message")) == "" {
					t.Errorf("no message")
				}
				if strings.Contains(r.Text(), "203.0.113.3") || strings.Contains(r.Text(), "secret") || strings.Contains(r.Text(), "10.1.2.3") {
					t.Errorf("leaked: %s", r.Text())
				}
				if retry := c.header["retry-after"]; retry != "" && r.Header.Get("Retry-After") != retry {
					t.Errorf("Retry-After = %q", r.Header.Get("Retry-After"))
				}
				if s := lastRequest(t, h).Status; s != c.want {
					t.Errorf("logged %d", s)
				}
			})
		}
	}
	h := run(t, func(testkit.Seen, int) testkit.Reply { return testkit.JSONR(400, cases[0].body) })
	r := post(t, h, body("", "", true, ""))
	if !strings.Contains(jsonx.Str(jsonx.Get(r.JSON(), "error", "message")), "131072") {
		t.Errorf("message = %v", jsonx.Get(r.JSON(), "error", "message"))
	}
	if jsonx.Get(r.JSON(), "error", "type") != "invalid_request_error" {
		t.Errorf("type = %v", jsonx.Get(r.JSON(), "error", "type"))
	}
}

func TestUnreachableBackendIs502AndSlowOneIs504(t *testing.T) {
	h := configure(t, func(c *config.Config) { c.Backends[0].BaseURL = "http://127.0.0.1:1/v1" }, okStream)
	r := post(t, h, body("", "", true, ""))
	if r.Status != 502 || jsonx.Get(r.JSON(), "error", "code") != "upstream_unreachable" {
		t.Errorf("unreachable: %d %s", r.Status, r.Text())
	}
	h = configure(t, func(c *config.Config) { c.HeaderTimeoutSeconds = 0.3 }, func(testkit.Seen, int) testkit.Reply {
		return testkit.Delayed{After: 3 * time.Second, Reply: testkit.JSONR(200, testkit.Text("late"))}
	})
	r = post(t, h, body("", "", false, ""))
	if r.Status != 504 || jsonx.Get(r.JSON(), "error", "code") != "upstream_timeout" {
		t.Errorf("slow: %d %s", r.Status, r.Text())
	}
}

// Documents and images.

const mediaSkip = "needs media port"

func TestInputFilePDFsGoThroughTheDocumentReader(t *testing.T) {
	t.Skip(mediaSkip)
	reader := &testkit.DocReader{}
	pdf := "JVBERi0xLjQK"
	input := `[{"role":"user","content":[
	  {"type":"input_text","text":"read these"},
	  {"type":"input_file","filename":"a.pdf","file_data":"data:application/pdf;base64,` + pdf + `"},
	  {"type":"input_file","filename":"b.pdf","file_data":"` + pdf + `"},
	  {"type":"input_file","filename":"c.txt","file_data":"data:text/plain;base64,aGkgdGhlcmU="},
	  {"type":"input_file","file_id":"file-123"},
	  {"type":"input_image","image_url":"data:application/pdf;base64,` + pdf + `"}]}]`
	h := harness.Run(t, harness.Options{Hooks: hooks.Hooks{Documents: reader}}, okStream)
	r := post(t, h, body(input, "", true, ""))
	if r.Status != 200 {
		t.Fatalf("status %d", r.Status)
	}
	content := jsonx.Arr(jsonx.Get(messages(h), 0, "content"))
	var texts []string
	for _, p := range content {
		if jsonx.Type(p) != "text" {
			t.Errorf("part %s", jsonx.String(p))
		}
		texts = append(texts, jsonx.Str(jsonx.Get(p, "text")))
	}
	if len(texts) != 6 {
		t.Fatalf("texts = %q", texts)
	}
	want := []string{"read these", "[document: a.pdf]\nEXTRACTED 9 bytes of application/pdf",
		"[document: b.pdf]\nEXTRACTED 9 bytes of application/pdf", "[document: c.txt]\nhi there"}
	eq(t, "texts", texts[:4], want)
	if !strings.Contains(texts[4], "could not be read") || !strings.Contains(texts[4], "unnamed file") {
		t.Errorf("file_id part = %q", texts[4])
	}
	if !strings.HasPrefix(texts[5], "[document: document]\nEXTRACTED") {
		t.Errorf("PDF image part = %q", texts[5])
	}
	// The same PDF is read once per distinct name.
	if calls := reader.Calls(); len(calls) != 3 {
		t.Errorf("reader called %d times", len(calls))
	} else {
		for _, c := range calls {
			if c.MediaType != "application/pdf" {
				t.Errorf("media type %s", c.MediaType)
			}
		}
	}
	found := false
	for _, e := range h.Log.All() {
		if e.Kind == "document" && e.Name == "a.pdf" {
			found = true
		}
	}
	if !found {
		t.Errorf("no document event for a.pdf")
	}
}

func TestDocumentsWithoutAReaderAreNotedNotDropped(t *testing.T) {
	t.Skip(mediaSkip)
	input := `[{"role":"user","content":[{"type":"input_file","filename":"a.pdf","file_data":"data:application/pdf;base64,JVBERi0="}]}]`
	h := run(t, okStream)
	post(t, h, body(input, "", true, ""))
	if got := jsonx.Get(messages(h), 0, "content"); got != "[document: a.pdf could not be read (document reading is not available)]" {
		t.Errorf("content = %v", got)
	}
}

func TestImagesForAModelWithoutVisionAreDescribed(t *testing.T) {
	t.Skip(mediaSkip)
	png := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("\x07", 64)))
	input := `[{"role":"user","content":[{"type":"input_text","text":"what is this"},
	   {"type":"input_image","image_url":"data:image/png;base64,` + png + `"}]},
	 {"type":"function_call","call_id":"c1","name":"view_image","arguments":"{}"},
	 {"type":"function_call_output","call_id":"c1","output":[{"type":"input_image","image_url":"data:image/png;base64,` + png + `"}]}]`
	describer := &testkit.Describer{}
	no := false
	h := harness.Run(t, harness.Options{Hooks: hooks.Hooks{Images: describer},
		Configure: func(c *config.Config) { c.Roles.Vision = "Big"; c.Models[0].Vision = &no }}, okStream)
	post(t, h, body(input, "", true, ""))
	msgs := messages(h)
	if jsonx.Get(msgs[0], "content", 0, "text") != "what is this" {
		t.Errorf("msgs[0] = %s", jsonx.String(msgs[0]))
	}
	if jsonx.Get(msgs[0], "content", 1, "text") != "[image: a cat on a mat]" || jsonx.Type(jsonx.Get(msgs[0], "content", 1)) != "text" {
		t.Errorf("msgs[0] = %s", jsonx.String(msgs[0]))
	}
	// A tool's image rides in a user message after the tool message.
	eq(t, "roles", roles(msgs), []string{"user", "assistant", "tool", "user"})
	if jsonx.Get(msgs[2], "content") != "[the tool returned attachments; they follow in the next message]" {
		t.Errorf("tool = %s", jsonx.String(msgs[2]))
	}
	if jsonx.Get(msgs[3], "content", 0, "text") != "[attachments from tool result c1]" || jsonx.Get(msgs[3], "content", 1, "text") != "[image: a cat on a mat]" {
		t.Errorf("attachments = %s", jsonx.String(msgs[3]))
	}
	if describer.Calls() != 1 { // the same image is described once
		t.Errorf("described %d times", describer.Calls())
	}
}

func TestImagesWithoutAnyVisionAreOmittedWithANote(t *testing.T) {
	t.Skip(mediaSkip)
	input := `[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]`
	no := false
	h := configure(t, func(c *config.Config) { c.Models[0].Vision = &no }, okStream)
	post(t, h, body(input, "", true, ""))
	if got := jsonx.Get(messages(h), 0, "content"); got != "[image omitted: GLM has no vision and no vision model is configured]" {
		t.Errorf("content = %v", got)
	}
}

func TestImagesStayForAModelWithVision(t *testing.T) {
	input := `[{"role":"user","content":[{"type":"input_text","text":"look"},{"type":"input_image","image_url":"data:image/png;base64,AAAA","detail":"high"}]}]`
	describer := &testkit.Describer{}
	yes := true
	h := harness.Run(t, harness.Options{Hooks: hooks.Hooks{Images: describer},
		Configure: func(c *config.Config) { c.Models[0].Vision = &yes; c.Roles.Vision = "Big" }}, okStream)
	post(t, h, body(input, "", true, ""))
	parts := jsonx.Arr(jsonx.Get(messages(h), 0, "content"))
	eq(t, "types", types(parts), []string{"text", "image_url"})
	if len(parts) == 2 && (jsonx.Get(parts[1], "image_url", "url") != "data:image/png;base64,AAAA" || jsonx.Get(parts[1], "image_url", "detail") != "high") {
		t.Errorf("image = %s", jsonx.String(parts[1]))
	}
	if describer.Calls() != 0 {
		t.Errorf("described %d times", describer.Calls())
	}
}

// Web search.

var searchCallChunk = testkit.ToolCall(0, "call_s", "web_search", `{"query":"swift testing"}`)

func TestWebSearchLoopStreamsSearchCallItems(t *testing.T) {
	searcher := &testkit.Searcher{}
	h := harness.Run(t, harness.Options{Hooks: hooks.Hooks{WebSearch: searcher}}, func(_ testkit.Seen, n int) testkit.Reply {
		if n == 0 {
			return testkit.SSE(testkit.Content("Let me look. "), searchCallChunk, testkit.Finish("tool_calls"))
		}
		return testkit.SSE(testkit.Content("Found it."), testkit.Finish(""), testkit.Usage(20, 3, 0))
	})
	r := post(t, h, codexFixture(t, "main_turn", nil))
	if r.Status != 200 {
		t.Fatalf("status %d", r.Status)
	}
	ev := r.Events()
	names := testkit.Names(ev)
	if names[len(names)-1] != "response.completed" {
		t.Fatalf("events %v", names)
	}
	eq(t, "queries", searcher.Queries(), []string{"swift testing"})
	chats := h.Stub.ChatRequests()
	if len(chats) != 2 {
		t.Fatalf("%d chat requests", len(chats))
	}
	// The model was offered the function, and got the results back.
	offered := false
	for _, tool := range jsonx.Arr(chats[0]["tools"]) {
		if jsonx.Get(tool, "function", "name") == "web_search" {
			offered = true
		}
	}
	if !offered {
		t.Errorf("web_search not offered")
	}
	msgs := jsonx.Arr(chats[1]["messages"])
	eq(t, "last roles", roles(msgs[len(msgs)-2:]), []string{"assistant", "tool"})
	asst := msgs[len(msgs)-2]
	if jsonx.Get(asst, "tool_calls", 0, "function", "name") != "web_search" || jsonx.Get(asst, "content") != "Let me look. " {
		t.Errorf("assistant = %s", jsonx.String(asst))
	}
	if !strings.Contains(jsonx.Str(jsonx.Get(msgs[len(msgs)-1], "content")), "Result for swift testing") {
		t.Errorf("tool = %s", jsonx.String(msgs[len(msgs)-1]))
	}

	out := completedOutput(ev)
	eq(t, "types", types(out), []string{"message", "web_search_call", "message"})
	if len(out) == 3 {
		ws := out[1]
		if jsonx.Get(ws, "status") != "completed" || jsonx.Get(ws, "action", "query") != "swift testing" {
			t.Errorf("web_search_call = %s", jsonx.String(ws))
		}
		if jsonx.Get(ws, "sources", 0, "type") != "url_citation" || !strings.HasPrefix(jsonx.Str(jsonx.Get(ws, "sources", 0, "url")), "https://example.com") {
			t.Errorf("sources = %s", jsonx.String(jsonx.Get(ws, "sources")))
		}
	}
	if !slices.Contains(names, "response.web_search_call.searching") {
		t.Errorf("no searching event")
	}
	for _, name := range []string{"response.output_item.added", "response.output_item.done"} {
		var idx []int
		for _, d := range data(ev, name) {
			idx = append(idx, num(d["output_index"]))
		}
		eq(t, name+" indexes", idx, []int{0, 1, 2})
	}
	// No function_call item for the search itself.
	if slices.Contains(types(out), "function_call") {
		t.Errorf("function_call for the search")
	}
	if got := num(jsonx.Get(data(ev, "response.completed")[0], "response", "usage", "input_tokens")); got != 20 {
		t.Errorf("input_tokens = %d", got)
	}
	logged := false
	for _, e := range h.Log.All() {
		if e.Kind == "websearch" && e.Query == "swift testing" {
			logged = true
		}
	}
	if !logged {
		t.Errorf("search not logged")
	}
}

func TestWebSearchLoopNonStreaming(t *testing.T) {
	searcher := &testkit.Searcher{}
	calls := `[{"id":"call_s","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"q\"}"}}]`
	h := harness.Run(t, harness.Options{Hooks: hooks.Hooks{WebSearch: searcher}}, func(_ testkit.Seen, n int) testkit.Reply {
		if n == 0 {
			return testkit.JSONR(200, testkit.Completion{ToolCalls: calls, Finish: "tool_calls", Prompt: 10, CompletionN: 2}.String())
		}
		return testkit.JSONR(200, testkit.Completion{Content: str("Answer."), Prompt: 30, CompletionN: 4}.String())
	})
	r := post(t, h, body("", "", false, `"tools":[{"type":"web_search"}]`))
	j := r.JSON()
	eq(t, "types", types(jsonx.Arr(j["output"])), []string{"web_search_call", "message"})
	if j["output_text"] != "Answer." {
		t.Errorf("output_text = %v", j["output_text"])
	}
	if num(jsonx.Get(j, "usage", "input_tokens")) != 40 || num(jsonx.Get(j, "usage", "output_tokens")) != 6 {
		t.Errorf("usage = %s", jsonx.String(j["usage"]))
	}
	eq(t, "queries", searcher.Queries(), []string{"q"})
}

func TestWebSearchWithoutAProviderTellsTheModel(t *testing.T) {
	h := run(t, func(_ testkit.Seen, n int) testkit.Reply {
		if n == 0 {
			return testkit.SSE(searchCallChunk, testkit.Finish("tool_calls"))
		}
		return testkit.SSE(testkit.Content("I could not search."), testkit.Finish(""))
	})
	ev := post(t, h, body("", "", true, `"tools":[{"type":"web_search"}]`)).Events()
	names := testkit.Names(ev)
	if names[len(names)-1] != "response.completed" {
		t.Fatalf("events %v", names)
	}
	if out := completedOutput(ev); len(out) == 0 || jsonx.Get(out[0], "status") != "failed" {
		t.Errorf("output = %s", jsonx.String(out))
	}
	msgs := messages(h)
	if !strings.Contains(jsonx.Str(jsonx.Get(msgs[len(msgs)-1], "content")), "unavailable") {
		t.Errorf("last message = %s", jsonx.String(msgs[len(msgs)-1]))
	}
}

func TestAClientFunctionNamedWebSearchWins(t *testing.T) {
	searcher := &testkit.Searcher{}
	h := harness.Run(t, harness.Options{Hooks: hooks.Hooks{WebSearch: searcher}}, func(testkit.Seen, int) testkit.Reply {
		return testkit.SSE(searchCallChunk, testkit.Finish("tool_calls"))
	})
	r := post(t, h, body("", "", true, `"tools":[{"type":"function","name":"web_search"},{"type":"web_search"}]`))
	if len(searcher.Queries()) != 0 {
		t.Errorf("searched %v", searcher.Queries())
	}
	if out := completedOutput(r.Events()); len(out) == 0 || jsonx.Type(out[0]) != "function_call" {
		t.Errorf("output = %s", jsonx.String(out))
	}
	if n := len(h.Stub.ChatRequests()); n != 1 {
		t.Errorf("%d chat requests", n)
	}
}
