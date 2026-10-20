package messages_test

import (
	"strings"
	"testing"

	"codelink/internal/apierr"
	"codelink/internal/config"
	"codelink/internal/jsonx"
	"codelink/internal/routing"
)

// The error mapping and routing the Messages route relies on (the Swift
// EstimateAndErrorTests and RoutingTests).

func TestContextLengthErrorsRewritten(t *testing.T) {
	for _, c := range [][2]string{
		{`{"error":{"message":"This model's maximum context length is 32768 tokens. However, you requested 40000 tokens (35000 in the messages, 5000 in the completion). Please reduce the length of the messages or completion.","type":"BadRequestError","code":400}}`, "prompt is too long: 35000 tokens > 32768 maximum"},
		{`{"error":{"message":"You passed 35000 input tokens and requested 5000 output tokens. However, the model's context length is only 32768 tokens, resulting in a maximum input length of 27768 tokens."}}`, "prompt is too long: 35000 tokens > 32768 maximum"},
		{`{"error":{"message":"Requested token count exceeds the model's maximum context length of 32768 tokens. You requested a total of 40000 tokens: 35000 tokens from the input messages and 5000 tokens for the completion."}}`, "prompt is too long: 35000 tokens > 32768 maximum"},
		{`{"error":{"message":"Input length (40001) exceeds the maximum allowed length (32768)."}}`, "prompt is too long: 40001 tokens > 32768 maximum"},
		{`{"error":{"code":400,"message":"the request exceeds the available context size","type":"exceed_context_size_error","n_prompt_tokens":50000,"n_ctx":32768}}`, "prompt is too long: 50000 tokens > 32768 maximum"},
		{`{"detail":"This model's maximum context length is 1000 tokens. However, you requested 1500 tokens (1400 in the messages, 100 in the completion)."}`, "prompt is too long: 1400 tokens > 1000 maximum"},
		{`{"error":{"message":"This model's maximum context length is 32768 tokens. However, you requested 40000 tokens (30000 in the messages, 10000 in the completion)."}}`, "input length and `max_tokens` exceed context limit: 30000 + 10000 > 32768"},
	} {
		e := apierr.FromUpstream(400, []byte(c[0]), nil, false)
		if e.Status != 400 || e.Type != "invalid_request_error" || e.Message != c[1] {
			t.Errorf("%s: got %d %s %q", c[0], e.Status, e.Type, e.Message)
		}
	}
}

func TestScrubsAddressesAndCredentials(t *testing.T) {
	s := apierr.Scrub("failed calling http://10.1.2.3:8000/v1/chat with Bearer abcdefghijkl and api_key=sk-secretvalue123 on 192.168.0.5:9000")
	for _, bad := range []string{"10.1.2.3", "abcdefghijkl", "secretvalue", "192.168.0.5", "http://"} {
		if strings.Contains(s, bad) {
			t.Errorf("%s in %s", bad, s)
		}
	}
}

func TestStatusMapping(t *testing.T) {
	for _, c := range []struct {
		up, status int
		typ        string
	}{{400, 400, "invalid_request_error"}, {401, 502, "api_error"}, {403, 502, "api_error"},
		{404, 404, "not_found_error"}, {413, 413, "request_too_large"}, {422, 400, "invalid_request_error"},
		{429, 429, "rate_limit_error"}, {500, 500, "api_error"}, {503, 503, "overloaded_error"},
		{529, 529, "overloaded_error"}, {418, 400, "invalid_request_error"}} {
		e := apierr.FromUpstream(c.up, []byte(`{"error":{"message":"no"}}`), nil, false)
		if e.Status != c.status || e.Type != c.typ {
			t.Errorf("upstream %d: got %d %s", c.up, e.Status, e.Type)
		}
	}
	if m := apierr.FromUpstream(401, nil, nil, false).Message; !strings.Contains(m, "credentials") {
		t.Errorf("401: %s", m)
	}
	eq(t, apierr.FromUpstream(500, nil, nil, false).Message, "backend returned HTTP 500", "empty 500")
	eq(t, apierr.FromUpstream(422, []byte(`{"detail":[{"msg":"a"},{"msg":"b"}]}`), nil, false).Message, "a; b", "detail list")
	native := apierr.FromUpstream(429, []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`), nil, true)
	eq(t, native.Type+" "+native.Message, "rate_limit_error slow down", "native")
}

func testRouting() routing.Routing {
	return routing.Routing{Config: &config.Config{
		Backends: []config.Backend{{ID: "b", Kind: config.OpenAICompatible, BaseURL: "http://h/v1"}},
		Models: []config.Model{{ID: "GLM-5.3", BackendID: "b", Upstream: "glm"}, {ID: "Flash", BackendID: "b", Upstream: "flash"},
			{ID: "Heavy", BackendID: "b", Upstream: "heavy"}},
		Roles: config.Roles{Main: "GLM-5.3", Opus: "Heavy", Small: "Flash"},
	}}
}

func TestClaudeNamesMapToRoles(t *testing.T) {
	r := testRouting()
	for name, want := range map[string]string{
		"GLM-5.3": "GLM-5.3", "glm-5.3": "GLM-5.3", "Flash": "Flash", "claude-opus-5[1m]": "Heavy",
		"claude-opus-4-1-20250805": "Heavy", "claude-fable-5-1": "Heavy", "claude-mythos-5-1": "Heavy",
		"claude-haiku-4-5-20251001": "Flash", "claude-sonnet-5": "GLM-5.3", "claude-sonnet-4-5[1m]": "GLM-5.3",
		"GLM-5.3[1m]": "GLM-5.3", "gpt-who": "GLM-5.3", "opus": "Heavy",
	} {
		m, _ := r.Resolve(name)
		eq(t, m.ID, want, name)
	}
}

func TestUnsetRolesFallBackToMain(t *testing.T) {
	r := routing.Routing{Config: &config.Config{Models: []config.Model{{ID: "A", BackendID: "b", Upstream: "a"}}, Roles: config.Roles{Main: "A"}}}
	for _, n := range []string{"claude-opus-5", "claude-haiku-5"} {
		m, _ := r.Resolve(n)
		eq(t, m.ID, "A", n)
	}
	if _, ok := (routing.Routing{Config: &config.Config{}}).Resolve("claude-sonnet-5"); ok {
		t.Error("resolved with no models")
	}
}

func TestTargetCarriesTheKeyAndURLs(t *testing.T) {
	r := routing.Routing{Config: &config.Config{
		Backends: []config.Backend{{ID: "b", Kind: config.OpenAICompatible, BaseURL: "http://h:8000/v1/", APIKey: "sk-stub-secret"}},
		Models:   []config.Model{{ID: "A", BackendID: "b", Upstream: "up-a"}}, Roles: config.Roles{Main: "A"}}}
	m, _ := r.Resolve("A")
	tg, ok := r.Target(m)
	if !ok {
		t.Fatal("no target")
	}
	req := tg.ChatRequest(map[string]any{"messages": []any{}, "model": "ignored"}, true)
	eq(t, req.URL, "http://h:8000/v1/chat/completions", "url")
	eq(t, req.Header.Get("Authorization"), "Bearer sk-stub-secret", "auth")
	eq(t, req.Header.Get("Accept"), "text/event-stream", "accept")
	eq(t, jsonx.Str(jsonx.ParseObject(req.Body)["model"]), "up-a", "model")
	if _, ok := r.Target(config.Model{ID: "x", BackendID: "missing", Upstream: "x"}); ok {
		t.Error("target for a missing backend")
	}
}
