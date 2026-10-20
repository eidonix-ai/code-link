package messages

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"

	"codelink/internal/config"
	"codelink/internal/jsonx"
	"codelink/internal/media"
	"codelink/internal/testkit"
)

var glm = config.Model{ID: "GLM", BackendID: "b", Upstream: "glm"}

func translateWith(t *testing.T, body string, model config.Model, window int) obj {
	t.Helper()
	req := jsonx.ParseObject([]byte(body))
	if req == nil {
		t.Fatalf("bad test body: %s", body)
	}
	return chatRequest(req, model, window, findWebSearch(jsonx.Arr(req["tools"])))
}

func translate(t *testing.T, body string) obj { t.Helper(); return translateWith(t, body, glm, 0) }

func msgs(t *testing.T, body string) []any {
	t.Helper()
	return jsonx.Arr(translate(t, body)["messages"])
}

func roles(m []any) []string {
	var out []string
	for _, x := range m {
		if r, ok := jsonx.StrOK(jsonx.Get(x, "role")); ok {
			out = append(out, r)
		}
	}
	return out
}

func toolNames(chat obj) []string {
	var out []string
	for _, x := range jsonx.Arr(chat["tools"]) {
		out = append(out, jsonx.Str(jsonx.Get(x, "function", "name")))
	}
	return out
}

func str(v any, path ...any) string { return jsonx.Str(jsonx.Get(v, path...)) }

func eq[T any](t *testing.T, got, want T, what string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: got %#v, want %#v", what, got, want)
	}
}

// jeq compares a value's JSON with the given JSON text.
func jeq(t *testing.T, got any, want string, what string) {
	t.Helper()
	if g := jsonx.String(got); g != testkit.Compact(want) {
		t.Errorf("%s: got %s, want %s", what, g, testkit.Compact(want))
	}
}

func hasKey(o any, k string) bool { _, ok := jsonx.Obj(o)[k]; return ok }

// MARK: request translation

func TestSystemBlocksJoinedInOrderAndMidConversationSystemBecomesReminder(t *testing.T) {
	m := msgs(t, `{"model":"m","max_tokens":10,"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2"},{"type":"text","text":"You are Claude Code.","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hi"},{"role":"system","content":"budget: 10"},{"role":"user","content":"go"}]}`)
	eq(t, str(m[0], "role"), "system", "role")
	eq(t, str(m[0], "content"), "x-anthropic-billing-header: cc_version=2\nYou are Claude Code.", "system")
	eq(t, roles(m), []string{"system", "user", "user", "user"}, "roles")
	eq(t, str(m[2], "content"), "<system-reminder>\nbudget: 10\n</system-reminder>", "reminder")
	eq(t, str(msgs(t, `{"model":"m","system":"plain","messages":[]}`)[0], "content"), "plain", "plain")
	eq(t, len(msgs(t, `{"model":"m","system":[],"messages":[{"role":"user","content":"x"}]}`)), 1, "empty system")
}

func TestToolUseAndResult(t *testing.T) {
	m := msgs(t, `{"model":"m","messages":[{"role":"user","content":"read it"},{"role":"assistant","content":[{"type":"text","text":"Reading."},{"type":"tool_use","id":"toolu_1","name":"Read","input":{"path":"/a","limit":5}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"line1"},{"type":"text","text":"line2"}]}]}]}`)
	eq(t, roles(m), []string{"user", "assistant", "tool"}, "roles")
	eq(t, str(m[1], "content"), "Reading.", "content")
	c := jsonx.Get(m[1], "tool_calls", 0)
	eq(t, str(c, "id"), "toolu_1", "id")
	eq(t, str(c, "type"), "function", "type")
	eq(t, str(c, "function", "name"), "Read", "name")
	// Go writes keys sorted (byte-stable for prefix caches).
	eq(t, str(c, "function", "arguments"), `{"limit":5,"path":"/a"}`, "arguments")
	eq(t, str(m[2], "tool_call_id"), "toolu_1", "tool_call_id")
	eq(t, str(m[2], "content"), "line1\nline2", "result")
}

func TestToolResultsComeBeforeTheUserTextOfTheSameTurn(t *testing.T) {
	m := msgs(t, `{"model":"m","messages":[{"role":"user","content":"go"},{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"A","input":{}},{"type":"tool_use","id":"b","name":"B","input":{}}]},{"role":"user","content":[{"type":"text","text":"<system-reminder>x</system-reminder>"},{"type":"tool_result","tool_use_id":"a","content":"ra"},{"type":"tool_result","tool_use_id":"b","content":"rb"},{"type":"text","text":"now summarise"}]}]}`)
	eq(t, roles(m), []string{"user", "assistant", "tool", "tool", "user"}, "roles")
	if !hasKey(m[1], "content") || jsonx.Get(m[1], "content") != nil {
		t.Errorf("assistant content should be null: %v", m[1])
	}
	eq(t, str(m[2], "tool_call_id")+str(m[3], "tool_call_id"), "ab", "order")
	var texts []string
	for _, p := range jsonx.Arr(jsonx.Get(m[4], "content")) {
		texts = append(texts, str(p, "text"))
	}
	eq(t, texts, []string{"<system-reminder>x</system-reminder>", "now summarise"}, "parts")
}

func TestMediaInToolResultRidesInTheFollowingUserMessage(t *testing.T) {
	m := msgs(t, `{"model":"m","messages":[{"role":"user","content":"look"},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_9","name":"Screenshot","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_9","content":[{"type":"text","text":"captured"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}]}]}]}`)
	eq(t, roles(m), []string{"user", "assistant", "tool", "user"}, "roles")
	eq(t, str(m[2], "content"), "captured", "tool text")
	parts := jsonx.Arr(jsonx.Get(m[3], "content"))
	eq(t, []string{jsonx.Type(parts[0]), jsonx.Type(parts[1])}, []string{"text", "image_url"}, "types")
	if !strings.Contains(str(parts[0], "text"), "toolu_9") {
		t.Errorf("attachment header: %v", parts[0])
	}
	eq(t, str(parts[1], "image_url", "url"), "data:image/png;base64,iVBORw0KGgo=", "url")
	// Only media: the tool message says so.
	m2 := msgs(t, `{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"`+media.AttachmentTextType+`","text":"[document: a.pdf]\nhello"}]}]}]}`)
	if !strings.Contains(str(m2[0], "content"), "attachments") {
		t.Errorf("tool text: %v", m2[0])
	}
	eq(t, str(m2[1], "content", 1, "text"), "[document: a.pdf]\nhello", "attachment text")
}

func TestToolErrorIsPrefixed(t *testing.T) {
	m := msgs(t, `{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","is_error":true,"content":"boom"}]}]}`)
	eq(t, str(m[0], "content"), "[tool error] boom", "content")
}

func TestThinkingReplayedAsReasoningAndRedactedDropped(t *testing.T) {
	m := msgs(t, `{"model":"m","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"first thoughts","signature":"x"},{"type":"redacted_thinking","data":"zzz"},{"type":"text","text":"answer"}]},{"role":"user","content":[{"type":"thinking","thinking":"ignored"},{"type":"text","text":"ok"}]}]}`)
	eq(t, str(m[0], "reasoning_content"), "first thoughts", "reasoning_content")
	eq(t, str(m[0], "reasoning"), "first thoughts", "reasoning")
	eq(t, str(m[0], "content"), "answer", "content")
	eq(t, str(m[1], "content"), "ok", "user")
}

func TestImagesBecomeImageURLParts(t *testing.T) {
	m := msgs(t, `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"what is this"},{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"AAAA"}},{"type":"image","source":{"type":"url","url":"https://x/y.png"}}]}]}`)
	parts := jsonx.Arr(jsonx.Get(m[0], "content"))
	eq(t, []string{jsonx.Type(parts[0]), jsonx.Type(parts[1]), jsonx.Type(parts[2])}, []string{"text", "image_url", "image_url"}, "types")
	eq(t, str(parts[1], "image_url", "url"), "data:image/jpeg;base64,AAAA", "base64")
	eq(t, str(parts[2], "image_url", "url"), "https://x/y.png", "url")
}

func TestDeferredToolsLoadAfterAToolReference(t *testing.T) {
	tools := `"tools":[{"name":"Read","description":"read","input_schema":{"type":"object"}},{"name":"ToolSearch","description":"find tools","input_schema":{"type":"object"}},{"name":"DeferredToolPlaceholder","description":"never call","input_schema":{"type":"object"},"defer_loading":true},{"name":"mcp__mini__shout","description":"shout","input_schema":{"type":"object"},"defer_loading":true}]`
	before := translate(t, `{"model":"m",`+tools+`,"messages":[{"role":"user","content":"shout hello"}]}`)
	eq(t, toolNames(before), []string{"Read", "ToolSearch"}, "before")
	after := translate(t, `{"model":"m",`+tools+`,"messages":[{"role":"user","content":"shout hello"},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"ToolSearch","input":{"query":"shout"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"tool_reference","tool_name":"mcp__mini__shout"},{"type":"tool_reference","tool_name":"DeferredToolPlaceholder"}]}]}]}`)
	eq(t, toolNames(after), []string{"Read", "ToolSearch", "mcp__mini__shout"}, "after")
	m := jsonx.Arr(after["messages"])
	last := m[len(m)-1]
	eq(t, str(last, "role"), "tool", "role")
	if !strings.Contains(str(last, "content"), "Tool mcp__mini__shout is now loaded and can be called.") {
		t.Errorf("tool text: %v", last)
	}
}

func TestToolSchemasPassThroughByteStable(t *testing.T) {
	schema := `{"type":"object","properties":{"zeta":{"type":"string"},"alpha":{"type":"integer","minimum":0.0}},"required":["zeta"]}`
	body := `{"model":"m","tools":[{"name":"T","description":"d","input_schema":` + schema + `}],"messages":[]}`
	a := jsonx.String(jsonx.Get(translate(t, body), "tools", 0))
	// Keys are written sorted and numbers as spelt, so the bytes are the
	// same every turn.
	eq(t, a, `{"function":{"description":"d","name":"T","parameters":{"properties":{"alpha":{"minimum":0.0,"type":"integer"},"zeta":{"type":"string"}},"required":["zeta"],"type":"object"}},"type":"function"}`, "tool")
	eq(t, jsonx.String(jsonx.Get(translate(t, body), "tools", 0)), a, "stable")
}

func TestServerToolsDroppedAndWebSearchOffered(t *testing.T) {
	chat := translate(t, `{"model":"m","tools":[{"type":"code_execution_20250522","name":"code_execution"},{"name":"Bash","input_schema":{"type":"object"}},{"type":"web_search_20250305","name":"web_search","max_uses":3}],"messages":[]}`)
	eq(t, toolNames(chat), []string{"Bash", "web_search"}, "names")
	jeq(t, jsonx.Get(chat, "tools", 1, "function", "parameters", "required"), `["query"]`, "required")
	parse := func(s string) []any { v, _ := jsonx.Parse([]byte(s)); return jsonx.Arr(v) }
	eq(t, findWebSearch(parse(`[{"type":"web_search_20250305","name":"web_search","max_uses":3}]`)).maxUses, 3, "max_uses")
	eq(t, findWebSearch(parse(`[{"type":"web_search_20250305","name":"web_search"}]`)).maxUses, 5, "default")
	// Only a server tool that isn't web search: no tools at all.
	none := translate(t, `{"model":"m","tools":[{"type":"bash_20250124","name":"bash"}],"tool_choice":{"type":"auto"},"messages":[]}`)
	if hasKey(none, "tools") || hasKey(none, "tool_choice") {
		t.Errorf("tools left: %v", none)
	}
}

func TestToolChoiceMapping(t *testing.T) {
	tools := `"tools":[{"name":"t","input_schema":{"type":"object"}}]`
	for tc, want := range map[string]string{
		`{"type":"auto"}`: `"auto"`, `{"type":"any"}`: `"required"`, `{"type":"none"}`: `"none"`,
		`{"type":"tool","name":"t"}`: `{"type":"function","function":{"name":"t"}}`,
	} {
		jeq(t, translate(t, `{"model":"m",`+tools+`,"tool_choice":`+tc+`,"messages":[]}`)["tool_choice"], want, tc)
	}
	p := translate(t, `{"model":"m",`+tools+`,"tool_choice":{"type":"auto","disable_parallel_tool_use":true},"messages":[]}`)
	eq(t, p["parallel_tool_calls"], any(false), "parallel")
	if hasKey(translate(t, `{"model":"m",`+tools+`,"messages":[]}`), "parallel_tool_calls") {
		t.Error("parallel_tool_calls without disable")
	}
	if hasKey(translate(t, `{"model":"m","tool_choice":{"type":"any"},"messages":[]}`), "tool_choice") {
		t.Error("tool_choice without tools")
	}
}

func TestThinkingDisabledAppliesThinkingOffAndDropsEffort(t *testing.T) {
	m := glm
	m.ThinkingOff = map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}}
	off := translateWith(t, `{"model":"m","max_tokens":100,"thinking":{"type":"disabled"},"output_config":{"effort":"high"},"messages":[]}`, m, 0)
	eq(t, jsonx.Get(off, "chat_template_kwargs", "enable_thinking"), any(false), "enable_thinking")
	if hasKey(off, "reasoning_effort") || hasKey(off, "thinking") {
		t.Errorf("off: %v", off)
	}
	on := translateWith(t, `{"model":"m","max_tokens":100,"thinking":{"type":"enabled","budget_tokens":2000},"output_config":{"effort":"max"},"messages":[]}`, m, 0)
	if hasKey(on, "chat_template_kwargs") || hasKey(on, "thinking") {
		t.Errorf("on: %v", on)
	}
	eq(t, on["reasoning_effort"], any("xhigh"), "effort")
	// No thinkingOff configured: nothing is added.
	plain := translate(t, `{"model":"m","thinking":{"type":"disabled"},"messages":[]}`)
	if hasKey(plain, "chat_template_kwargs") || hasKey(plain, "enable_thinking") {
		t.Errorf("plain: %v", plain)
	}
	// A reasoning_effort thinkingOff (GLM-5.3 with vLLM) merges in.
	m.ThinkingOff = map[string]any{"reasoning_effort": "low"}
	eq(t, translateWith(t, `{"model":"m","thinking":{"type":"disabled"},"output_config":{"effort":"high"},"messages":[]}`, m, 0)["reasoning_effort"], any("low"), "low")
	// The model's configuration is not modified by a request.
	eq(t, m.ThinkingOff["reasoning_effort"], any("low"), "config")
}

func TestThinkingOffMergesIntoExistingObjects(t *testing.T) {
	merged := deepMerge(obj{"a": obj{"x": 1, "y": 2}, "b": 1}, obj{"a": obj{"y": 3, "z": 4}, "c": true})
	eq(t, merged, obj{"a": obj{"x": 1, "y": 3, "z": 4}, "b": 1, "c": true}, "merged")
}

func TestOutputFormatBecomesStrictJSONSchema(t *testing.T) {
	for _, field := range []string{`"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"a":{"type":"string"}}}}}`,
		`"output_format":{"type":"json_schema","schema":{"type":"object","properties":{"a":{"type":"string"}}}}`} {
		rf := translate(t, `{"model":"m","messages":[],`+field+`}`)["response_format"]
		eq(t, str(rf, "type"), "json_schema", "type")
		eq(t, str(rf, "json_schema", "name"), "output", "name")
		eq(t, jsonx.Get(rf, "json_schema", "strict"), any(true), "strict")
		if jsonx.Get(rf, "json_schema", "schema", "properties", "a") == nil {
			t.Errorf("schema: %v", rf)
		}
	}
	if hasKey(translate(t, `{"model":"m","messages":[],"output_config":{"format":{"type":"text"}}}`), "response_format") {
		t.Error("text format")
	}
}

func TestSamplingStopAndMetadata(t *testing.T) {
	c := translate(t, `{"model":"m","max_tokens":321,"stream":true,"stop_sequences":["</x>","END"],"temperature":0.7,"top_p":0.9,"top_k":40,"metadata":{"user_id":"u1"},"messages":[]}`)
	eq(t, c["max_tokens"], any(321), "max_tokens")
	jeq(t, c["stop"], `["</x>","END"]`, "stop")
	jeq(t, []any{c["temperature"], c["top_p"], c["top_k"]}, `[0.7,0.9,40]`, "sampling")
	eq(t, c["user"], any("u1"), "user")
	eq(t, c["stream"], any(true), "stream")
	eq(t, jsonx.Get(c, "stream_options", "include_usage"), any(true), "include_usage")
	if hasKey(c, "model") {
		t.Error("model set") // the backend target sets the upstream name
	}
	n := translate(t, `{"model":"m","max_tokens":5,"messages":[]}`)
	eq(t, n["stream"], any(false), "stream false")
	if hasKey(n, "stream_options") || hasKey(n, "stop") {
		t.Errorf("non-stream: %v", n)
	}
}

func TestMaxTokensClampedToTheContextWindow(t *testing.T) {
	big := strings.Repeat("abcd ", 4000) // ~5000 tokens
	body := `{"model":"m","max_tokens":8000,"messages":[{"role":"user","content":"` + big + `"}]}`
	clamped, _ := jsonx.Int(translateWith(t, body, glm, 10_000)["max_tokens"])
	if clamped >= 5000 || clamped <= 3000 {
		t.Errorf("clamped to %d", clamped)
	}
	eq(t, translate(t, body)["max_tokens"], any(8000), "window unknown")
	eq(t, translateWith(t, body, glm, 100_000)["max_tokens"], any(8000), "fits")
	huge := strings.Repeat("abcd ", 12000)
	eq(t, translateWith(t, `{"model":"m","max_tokens":8000,"messages":[{"role":"user","content":"`+huge+`"}]}`, glm, 10_000)["max_tokens"], any(8000), "no room")
}

func TestEarlierWebSearchesReplayAsCallAndResult(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString([]byte("Swift 6.4 shipped"))
	m := msgs(t, `{"model":"m","messages":[{"role":"user","content":"search swift"},{"role":"assistant","content":[{"type":"text","text":"Let me look."},{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"swift 6.4"}},{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[{"type":"web_search_result","url":"https://swift.org","title":"Swift","encrypted_content":"`+enc+`","page_age":"2026-09-01"}]},{"type":"text","text":"It shipped."}]},{"role":"user","content":"thanks"}]}`)
	eq(t, roles(m), []string{"user", "assistant", "tool", "assistant", "user"}, "roles")
	eq(t, str(m[1], "tool_calls", 0, "function", "name"), "web_search", "call")
	eq(t, str(m[1], "content"), "Let me look.", "content")
	eq(t, str(m[2], "tool_call_id"), "srvtoolu_1", "tool_call_id")
	result := str(m[2], "content")
	for _, want := range []string{"https://swift.org", "Swift 6.4 shipped", "2026-09-01"} {
		if !strings.Contains(result, want) {
			t.Errorf("result lacks %q: %s", want, result)
		}
	}
	eq(t, str(m[3], "content"), "It shipped.", "after")

	// A call without its result stays as text.
	lone := msgs(t, `{"model":"m","messages":[{"role":"assistant","content":[{"type":"server_tool_use","id":"s","name":"web_search","input":{"query":"q"}},{"type":"text","text":"done"}]}]}`)
	eq(t, len(lone), 1, "lone count")
	eq(t, str(lone[0], "content"), "[Searched the web: q]done", "lone")

	// An error result replays as the failure text.
	e := msgs(t, `{"model":"m","messages":[{"role":"assistant","content":[{"type":"server_tool_use","id":"s","name":"web_search","input":{"query":"q"}},{"type":"web_search_tool_result","tool_use_id":"s","content":{"type":"web_search_tool_result_error","error_code":"max_uses_exceeded"}}]}]}`)
	if !strings.Contains(str(e[1], "content"), "limit") {
		t.Errorf("error replay: %v", e)
	}
}

func TestSearchAlongsideClientToolCallsKeepsToolMessagesTogether(t *testing.T) {
	// Client call first: the search result waits for the client's results.
	m := msgs(t, `{"model":"m","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{}},{"type":"server_tool_use","id":"s1","name":"web_search","input":{"query":"q"}},{"type":"web_search_tool_result","tool_use_id":"s1","content":[]}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"file"}]}]}`)
	eq(t, roles(m), []string{"assistant", "tool", "tool"}, "roles")
	eq(t, len(jsonx.Arr(jsonx.Get(m[0], "tool_calls"))), 2, "calls")
	eq(t, str(m[1], "tool_call_id")+","+str(m[2], "tool_call_id"), "s1,t1", "order")
	// Search first: its call and result, then the client's call.
	n := msgs(t, `{"model":"m","messages":[{"role":"assistant","content":[{"type":"server_tool_use","id":"s1","name":"web_search","input":{"query":"q"}},{"type":"web_search_tool_result","tool_use_id":"s1","content":[]},{"type":"tool_use","id":"t1","name":"Read","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"file"}]}]}`)
	eq(t, roles(n), []string{"assistant", "tool", "assistant", "tool"}, "roles")
}

func TestWebSearchResultInUserMessageBecomesText(t *testing.T) {
	m := msgs(t, `{"model":"m","messages":[{"role":"user","content":[{"type":"web_search_tool_result","tool_use_id":"x","content":[{"type":"web_search_result","title":"T","url":"https://u"}]},{"type":"text","text":"so?"}]}]}`)
	eq(t, str(m[0], "content", 0, "text"), "[Web search results:\n- T (https://u)]", "text")
}

func TestUnknownBlocksAndEmptyMessages(t *testing.T) {
	m := msgs(t, `{"model":"m","messages":[{"role":"user","content":[{"type":"mystery"}]},{"role":"assistant","content":[]},{"role":"user","content":"x"}]}`)
	eq(t, roles(m), []string{"assistant", "user"}, "roles")
	eq(t, jsonx.Get(m[0], "content"), any(""), "empty assistant")
}

// MARK: response translation

func parseChat(t *testing.T, s string) any {
	t.Helper()
	v, err := jsonx.Parse([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func ptr(s string) *string { return &s }

func TestContentThinkingToolsUsageAndStop(t *testing.T) {
	chat := parseChat(t, testkit.Completion{Content: ptr("<think>inner</think>The answer"),
		ToolCalls: `[{"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"path\":\"/a\"}"}},{"id":"","type":"function","function":{"name":"Bad","arguments":"not json"}}]`,
		Finish:    "stop", Prompt: 100, CompletionN: 4, Cached: 60}.String())
	msg := translateMessage(chat, "GLM", nil)
	c := jsonx.Arr(msg["content"])
	var types []string
	for _, b := range c {
		types = append(types, jsonx.Type(b))
	}
	eq(t, types, []string{"thinking", "text", "tool_use", "tool_use"}, "types")
	eq(t, str(c[0], "thinking"), "inner", "thinking")
	eq(t, str(c[1], "text"), "The answer", "text")
	jeq(t, jsonx.Get(c[2], "input"), `{"path":"/a"}`, "input")
	jeq(t, jsonx.Get(c[3], "input"), `{}`, "bad input")
	if !strings.HasPrefix(str(c[3], "id"), "toolu_") || len(str(c[3], "id")) != 30 {
		t.Errorf("id: %v", c[3])
	}
	eq(t, msg["stop_reason"], any("tool_use"), "stop")
	eq(t, msg["model"], any("GLM"), "model")
	jeq(t, msg["usage"], `{"input_tokens":40,"output_tokens":4,"cache_creation_input_tokens":0,"cache_read_input_tokens":60}`, "usage")
}

func TestReasoningFieldsAndStopSequence(t *testing.T) {
	chat := parseChat(t, testkit.Completion{Content: ptr("<block>no"), Reasoning: "hmm", Finish: "stop", StopReason: "</block>"}.String())
	msg := translateMessage(chat, "m", []string{"</block>"})
	eq(t, str(msg, "content", 0, "thinking"), "hmm", "thinking")
	eq(t, str(msg, "content", 1, "text"), "<block>no", "text")
	eq(t, msg["stop_reason"], any("stop_sequence"), "stop")
	eq(t, msg["stop_sequence"], any("</block>"), "seq")
	// A stop_reason not among the request's sequences is an ordinary end.
	other := translateMessage(chat, "m", nil)
	eq(t, other["stop_reason"], any("end_turn"), "other stop")
	if !hasKey(other, "stop_sequence") || other["stop_sequence"] != nil {
		t.Errorf("stop_sequence should be null: %v", other)
	}
	if translateMessage(parseChat(t, `{"choices":[]}`), "m", nil) != nil {
		t.Error("no choices should give nil")
	}
}

func TestStopReasonMapping(t *testing.T) {
	for _, c := range []struct {
		finish string
		tools  bool
		want   string
	}{{"length", false, "max_tokens"}, {"tool_calls", false, "tool_use"}, {"stop", true, "tool_use"},
		{"length", true, "max_tokens"}, {"content_filter", false, "end_turn"}} {
		got, _ := resolveStopReason(c.finish, "", nil, c.tools)
		eq(t, got, c.want, c.finish)
	}
}

func TestLoneLeadingCloseThinkStripped(t *testing.T) {
	_, content := stripThink("\n</think>\n\n<severity>12")
	eq(t, content, "<severity>12", "content")
	r, _ := stripThink("a<think>b")
	eq(t, r, "b", "reasoning")
}

// MARK: think tags

func runThink(chunks ...string) []segment {
	var f thinkFilter
	var out []segment
	for _, c := range chunks {
		out = append(out, f.process(c)...)
	}
	out = append(out, f.flush()...)
	var merged []segment
	for _, s := range out {
		if n := len(merged); n > 0 && merged[n-1].reasoning == s.reasoning {
			merged[n-1].text += s.text
		} else {
			merged = append(merged, s)
		}
	}
	return merged
}

func TestThinkTagsSplitAcrossChunks(t *testing.T) {
	eq(t, runThink("<th", "ink>reason", "ing</thi", "nk>answer"), []segment{{"reasoning", true}, {"answer", false}}, "split")
	eq(t, runThink("a <", "b"), []segment{{"a <b", false}}, "lone <")
	eq(t, runThink("x<think>y"), []segment{{"x", false}, {"y", true}}, "unclosed")
}

func TestThinkLoneLeadingCloseDropped(t *testing.T) {
	eq(t, runThink("</think>", "\n\nanswer"), []segment{{"\n\nanswer", false}}, "close")
	eq(t, runThink("</th", "ink>yes"), []segment{{"yes", false}}, "split close")
	eq(t, runThink("ok </think> later"), []segment{{"ok </think> later", false}}, "not leading")
}

// MARK: estimate

func TestEstimateSkipsBase64Media(t *testing.T) {
	img := strings.Repeat("A", 4<<20)
	est := estimateTokens([]byte(`{"messages":[{"image":"data:image/png;base64,` + img + `"}]}`))
	if est >= 5000 || est <= 1000 {
		t.Errorf("image estimate %d", est)
	}
	pdf := "JVBERi" + strings.Repeat("B", 300_000)
	eq(t, estimateTokens([]byte(pdf)), 300_006*3/4/150, "pdf")
	eq(t, estimateTokens([]byte(strings.Repeat("word ", 400))), 500, "text")
}

// MARK: passthrough body

func TestSetTopLevelKeepsEveryOtherByte(t *testing.T) {
	body := `{"z":1, "model" : "Claude","tools":[{"input_schema":{"properties":{"b":{},"a":{}}}}],"n":2.50}`
	out, err := setTopLevel([]byte(body), "model", "claude-real-1")
	if err != nil {
		t.Fatal(err)
	}
	eq(t, string(out), `{"z":1, "model" : "claude-real-1","tools":[{"input_schema":{"properties":{"b":{},"a":{}}}}],"n":2.50}`, "spliced")
	// A nested "model" is not the top-level one.
	out, _ = setTopLevel([]byte(`{"meta":{"model":"x"},"model":"y"}`), "model", "u")
	eq(t, string(out), `{"meta":{"model":"x"},"model":"u"}`, "nested")
	out, _ = setTopLevel([]byte(`{"a":1}`), "model", "u")
	eq(t, string(out), `{"a":1,"model":"u"}`, "added")
	if _, err := setTopLevel([]byte(`[1]`), "model", "u"); err == nil {
		t.Error("array accepted")
	}
}
