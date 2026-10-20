package automode

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"codelink/internal/config"
	"codelink/internal/hooks"
	"codelink/internal/jsonx"
	"codelink/internal/testkit"
)

// fakeCaller records every call and answers from a handler.
type fakeCaller struct {
	mu      sync.Mutex
	calls   []call
	handler func(i int, m config.Model, body map[string]any) (hooks.ChatResult, error)
}

type call struct {
	model config.Model
	body  map[string]any
}

func caller(h func(int, config.Model, map[string]any) (hooks.ChatResult, error)) *fakeCaller {
	return &fakeCaller{handler: h}
}

func fixed(r hooks.ChatResult) *fakeCaller {
	return caller(func(int, config.Model, map[string]any) (hooks.ChatResult, error) { return r, nil })
}

func (f *fakeCaller) Chat(_ context.Context, m config.Model, body map[string]any) (hooks.ChatResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call{m, body})
	i := len(f.calls) - 1
	f.mu.Unlock()
	return f.handler(i, m, body)
}

func (f *fakeCaller) Calls() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

var errFake = errors.New("fake")

const (
	sysSeverity = Marker + ". Output <severity>N</severity> where N is 0-100."
	sysBlock    = Marker + ". Output <block>yes</block> or <block>no</block>."
)

var model = config.Model{ID: "GLM", BackendID: "b", Upstream: "glm-5.3"}

func parse(s string) any {
	v, err := jsonx.Parse([]byte(s))
	if err != nil {
		panic(err)
	}
	return v
}

// canon is s parsed and written back (sorted keys), for comparing bodies.
func canon(v any) string {
	if s, ok := v.(string); ok {
		return jsonx.String(parse(s))
	}
	return jsonx.String(parse(jsonx.String(v)))
}

func request(system any, stops []string, user, mdl string) map[string]any {
	if user == "" {
		user = "<transcript>rm -rf /</transcript>"
	}
	if mdl == "" {
		mdl = "claude-sonnet-5"
	}
	o := map[string]any{
		"model": mdl, "max_tokens": 64, "system": system,
		"messages": []any{map[string]any{"role": "user", "content": user}},
	}
	if stops != nil {
		s := make([]any, len(stops))
		for i, x := range stops {
			s[i] = x
		}
		o["stop_sequences"] = s
	}
	return o
}

type pair struct {
	tok string
	p   float64
}

func top(pairs ...pair) []tokenLogprob {
	out := make([]tokenLogprob, len(pairs))
	for i, p := range pairs {
		out[i] = tokenLogprob{p.tok, math.Log(p.p)}
	}
	return out
}

func lp(p float64) string { return strconv.FormatFloat(math.Log(p), 'g', -1, 64) }

func logprobResult(pairs ...pair) hooks.ChatResult {
	var entries []string
	for _, p := range pairs {
		entries = append(entries, fmt.Sprintf(`{"token":%s,"logprob":%s}`, jsonx.String(p.tok), lp(p.p)))
	}
	return hooks.ChatResult{Status: 200, JSON: parse(`{"choices":[{"message":{"content":null,"reasoning":"x"},"logprobs":{"content":[{"top_logprobs":[` +
		strings.Join(entries, ",") + `]}]}}],"usage":{"prompt_tokens":1000,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":400}}}`)}
}

func text(r map[string]any) string { return jsonx.Str(jsonx.Get(r, "content", 0, "text")) }

func severityOf(t *testing.T, r map[string]any) int {
	t.Helper()
	s := strings.TrimPrefix(text(r), "<severity>")
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	n, err := strconv.Atoi(s[:i])
	if err != nil {
		t.Fatalf("no severity in %q", text(r))
	}
	return n
}

func head(m *config.Model, c hooks.ModelCaller) *Head { return New(m, c, nil) }

// MARK: Detection

func TestDetectsSeverityStage1(t *testing.T) {
	c, ok := classify(request(sysSeverity, []string{"</severity>"}, "", ""))
	if !ok || c.format != severityFormat || c.stage != 1 {
		t.Fatalf("%+v %v", c, ok)
	}
}

func TestDetectsSeverityStage2WithBlockSystem(t *testing.T) {
	sys := []any{map[string]any{"type": "text", "text": "x-anthropic-billing-header: cc"}, map[string]any{"type": "text", "text": sysSeverity}}
	c, ok := classify(request(sys, nil, "", ""))
	if !ok || c.format != severityFormat || c.stage != 2 {
		t.Fatalf("%+v %v", c, ok)
	}
	if !strings.HasPrefix(c.system, "x-anthropic-billing-header: cc\n") {
		t.Fatalf("system %q", c.system)
	}
}

func TestDetectsBlockFormat(t *testing.T) {
	s1, ok := classify(request(sysBlock, []string{"</block>"}, "", ""))
	if !ok || s1.format != blockFormat || s1.stage != 1 {
		t.Fatalf("%+v", s1)
	}
	if s2, _ := classify(request(sysBlock, nil, "", "")); s2.stage != 2 {
		t.Fatalf("%+v", s2)
	}
}

func TestOrdinaryRequestsAreNotClassifiers(t *testing.T) {
	h := head(&model, fixed(logprobResult()))
	if h.IsClassifierRequest(request("You are Claude Code.", nil, "", "")) {
		t.Fatal("ordinary turn")
	}
	// mentions a security monitor but not the exact marker / format
	if h.IsClassifierRequest(request("You are a security monitor.", []string{"</severity>"}, "", "")) {
		t.Fatal("no marker")
	}
	// marker without any output format or stop sequence
	if h.IsClassifierRequest(request(Marker+". Be careful.", nil, "", "")) {
		t.Fatal("no format")
	}
	// classifier shape but no user text
	r := request(sysSeverity, []string{"</severity>"}, "", "")
	r["messages"] = []any{}
	if h.IsClassifierRequest(r) {
		t.Fatal("no user text")
	}
	if h.IsClassifierRequest(nil) {
		t.Fatal("nil")
	}
}

func TestUserTextIsVerbatimAcrossBlocks(t *testing.T) {
	msgs := parse(`[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]},{"role":"assistant","content":"z"},{"role":"user","content":"c"}]`)
	if got := userText(msgs); got != "a\nb\n\nc" {
		t.Fatalf("%q", got)
	}
}

// The real capture (kept outside the repo: Claude Code's prompt is
// proprietary) is a severity stage-1 request.
func TestCapturedClassifierRequestIsDetected(t *testing.T) {
	home, _ := os.UserHomeDir()
	data, err := os.ReadFile(filepath.Join(home, "cletus-worktrees", "cutover", "captured-classifier-stage1.json"))
	if err != nil {
		t.Skip("capture not present")
	}
	body := jsonx.Obj(jsonx.Get(parse(string(data)), "body"))
	c, ok := classify(body)
	if !ok || c.format != severityFormat || c.stage != 1 || !strings.HasPrefix(c.user, "<transcript>") {
		t.Fatalf("format %v stage %d ok %v", c.format, c.stage, ok)
	}
}

// MARK: Score math

func TestScoreParsesIntegersAndTrimsSpaces(t *testing.T) {
	s, ok := scoreSeverity(top(pair{"0", 0.45}, pair{" 5", 0.05}, pair{"85", 0.4}, pair{"hello", 0.05}))
	if !ok {
		t.Fatal("no score")
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	if !near(s.mass, 0.9) || !near(s.pBlock, 0.4/0.9) || !near(s.expected, (0*0.45+5*0.05+85*0.4)/0.9) {
		t.Fatalf("%+v", s)
	}
}

func TestNonNumericTokensAreIgnored(t *testing.T) {
	for _, tok := range []string{"101", "-5", "1.5", "", "1000", "\n5"} {
		if _, ok := parseSeverityToken(tok); ok {
			t.Errorf("%q parsed", tok)
		}
	}
	if v, ok := parseSeverityToken(" 100"); !ok || v != 100 {
		t.Error("' 100'")
	}
	if v, ok := parseSeverityToken("0"); !ok || v != 0 {
		t.Error("'0'")
	}
}

func TestLowNumericMassIsNil(t *testing.T) {
	if _, ok := scoreSeverity(top(pair{"7", 0.2}, pair{"The", 0.8})); ok {
		t.Error("severity low mass")
	}
	if _, ok := scoreSeverity(nil); ok {
		t.Error("empty")
	}
	if _, ok := scoreBlock(top(pair{"yes", 0.1}, pair{"The", 0.9})); ok {
		t.Error("block low mass")
	}
}

func TestBlockScoreReadsYesNo(t *testing.T) {
	s, ok := scoreBlock(top(pair{"yes", 0.6}, pair{" No", 0.3}, pair{"maybe", 0.1}))
	if !ok || math.Abs(s.pBlock-0.6/0.9) > 1e-9 {
		t.Fatalf("%+v", s)
	}
}

func TestNIsForcedToTheBlockSideAndNeverFifty(t *testing.T) {
	cases := []struct {
		s     score
		n     int
		block bool
	}{
		{score{pBlock: 0.6, expected: 50, mass: 1}, 51, true}, // mean 50, 60% above the line
		{score{pBlock: 0.4, expected: 50, mass: 1}, 49, false},
		{score{pBlock: 0.97, expected: 92.4, mass: 1}, 92, true},
		{score{pBlock: 0, expected: 3.2, mass: 1}, 3, false},
	}
	for _, c := range cases {
		if n, b := decide(c.s, 0.5); n != c.n || b != c.block {
			t.Errorf("%+v: got %d %v", c.s, n, b)
		}
	}
	// exactly at the threshold blocks
	if _, b := decide(score{pBlock: 0.5, expected: 10, mass: 1}, 0.5); !b {
		t.Error("threshold")
	}
}

// MARK: Request shape

func TestDecisionRequestShapeIsExact(t *testing.T) {
	c := fixed(logprobResult(pair{"5", 0.9}))
	head(&model, c).Respond(context.Background(), request(sysSeverity, []string{"</severity>"}, "<transcript>ls</transcript>", ""))
	calls := c.Calls()
	if len(calls) == 0 {
		t.Fatal("no call")
	}
	if !reflect.DeepEqual(calls[0].model, model) {
		t.Fatalf("model %+v", calls[0].model)
	}
	want := `{"max_tokens":1,"logprobs":true,"top_logprobs":20,
	 "continue_final_message":true,"add_generation_prompt":false,
	 "messages":[{"role":"system","content":` + jsonx.String(sysSeverity) + `},
	             {"role":"user","content":"<transcript>ls</transcript>"},
	             {"role":"assistant","content":"<severity>"}]}`
	if got := canon(calls[0].body); got != canon(want) {
		t.Fatalf("body\n%s\nwant\n%s", got, canon(want))
	}
}

func TestBlockRequestPrefillsBlock(t *testing.T) {
	c := fixed(logprobResult(pair{"no", 0.9}))
	head(&model, c).Respond(context.Background(), request(sysBlock, []string{"</block>"}, "", ""))
	msgs := jsonx.Arr(c.Calls()[0].body["messages"])
	if got := canon(msgs[len(msgs)-1]); got != canon(`{"role":"assistant","content":"<block>"}`) {
		t.Fatal(got)
	}
}

// MARK: Response shapes

func TestStage1ResponseShape(t *testing.T) {
	c := fixed(logprobResult(pair{"90", 0.6}, pair{"95", 0.35}))
	r := head(&model, c).Respond(context.Background(), request(sysSeverity, []string{"</severity>"}, "", ""))
	if r == nil {
		t.Fatal("nil")
	}
	if len(c.Calls()) != 1 {
		t.Fatalf("%d calls", len(c.Calls()))
	}
	if text(r) != "<severity>92" { // 0.6/0.95*90 + 0.35/0.95*95 = 92.1
		t.Fatal(text(r))
	}
	checks := map[string]any{"stop_reason": "stop_sequence", "stop_sequence": "</severity>", "type": "message",
		"role": "assistant", "model": "claude-sonnet-5"}
	for k, v := range checks {
		if r[k] != v {
			t.Errorf("%s = %v", k, r[k])
		}
	}
	if !strings.HasPrefix(jsonx.Str(r["id"]), "msg_") || len(jsonx.Str(r["id"])) != 28 {
		t.Errorf("id %v", r["id"])
	}
	if got := canon(r["usage"]); got != canon(`{"input_tokens":600,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":400}`) {
		t.Errorf("usage %s", got)
	}
}

func TestStage1AllowForcedBelowFifty(t *testing.T) {
	c := fixed(logprobResult(pair{"2", 0.7}, pair{"60", 0.1}, pair{"0", 0.2}))
	r := head(&model, c).Respond(context.Background(), request(sysSeverity, []string{"</severity>"}, "", ""))
	if n := severityOf(t, r); n > 49 {
		t.Fatal(n)
	}
}

func TestStage2BlockAddsCategory(t *testing.T) {
	c := caller(func(i int, _ config.Model, _ map[string]any) (hooks.ChatResult, error) {
		if i == 0 {
			return logprobResult(pair{"90", 0.97}, pair{"5", 0.03}), nil
		}
		return hooks.ChatResult{Status: 200, JSON: parse(`{"choices":[{"message":{"content":"Force Push!"}}],"usage":{"prompt_tokens":1000,"completion_tokens":3}}`)}, nil
	})
	r := head(&model, c).Respond(context.Background(), request(sysSeverity, nil, "", ""))
	if r == nil {
		t.Fatal("nil")
	}
	calls := c.Calls()
	if len(calls) != 2 {
		t.Fatalf("%d calls", len(calls))
	}
	n := severityOf(t, r)
	if n <= 50 {
		t.Fatal(n)
	}
	if want := fmt.Sprintf("<severity>%d</severity><category>Force Push</category>", n); text(r) != want {
		t.Fatal(text(r))
	}
	if r["stop_reason"] != "end_turn" || r["stop_sequence"] != nil {
		t.Fatalf("%v %v", r["stop_reason"], r["stop_sequence"])
	}
	// category request shape
	cat := calls[1].body
	if canon(cat["max_tokens"]) != "16" || canon(cat["stop"]) != `["</category>"]` || cat["logprobs"] != nil || cat["temperature"] != nil {
		t.Fatalf("category body %s", canon(cat))
	}
	msgs := jsonx.Arr(cat["messages"])
	if got, want := canon(msgs[len(msgs)-1]), canon(fmt.Sprintf(`{"role":"assistant","content":"<severity>%d</severity><category>"}`, n)); got != want {
		t.Fatal(got)
	}
	// usage summed over both calls
	if out := jsonx.Get(r, "usage", "output_tokens"); out != 4 {
		t.Fatalf("output_tokens %v", out)
	}
}

func TestStage2AllowHasNoCategoryAndOneCall(t *testing.T) {
	c := fixed(logprobResult(pair{"3", 0.95}))
	r := head(&model, c).Respond(context.Background(), request(sysSeverity, nil, "", ""))
	if len(c.Calls()) != 1 || text(r) != "<severity>3</severity>" {
		t.Fatalf("%d calls, %q", len(c.Calls()), text(r))
	}
}

func TestCategoryFailureOmitsCategory(t *testing.T) {
	c := caller(func(i int, _ config.Model, _ map[string]any) (hooks.ChatResult, error) {
		if i == 0 {
			return logprobResult(pair{"90", 1}), nil
		}
		return hooks.ChatResult{}, errFake
	})
	r := head(&model, c).Respond(context.Background(), request(sysSeverity, nil, "", ""))
	if text(r) != "<severity>90</severity>" {
		t.Fatal(text(r))
	}
}

func TestBlockFormatResponses(t *testing.T) {
	yes := head(&model, fixed(logprobResult(pair{"yes", 0.9}, pair{"no", 0.1})))
	r1 := yes.Respond(context.Background(), request(sysBlock, []string{"</block>"}, "", ""))
	if text(r1) != "<block>yes" || r1["stop_reason"] != "stop_sequence" || r1["stop_sequence"] != "</block>" {
		t.Fatalf("%q %v %v", text(r1), r1["stop_reason"], r1["stop_sequence"])
	}
	r2 := yes.Respond(context.Background(), request(sysBlock, nil, "", ""))
	if text(r2) != "<block>yes</block>" || r2["stop_reason"] != "end_turn" {
		t.Fatalf("%q", text(r2))
	}
	no := head(&model, fixed(logprobResult(pair{"no", 0.8}, pair{"yes", 0.2})))
	if r3 := no.Respond(context.Background(), request(sysBlock, nil, "", "")); text(r3) != "<block>no</block>" {
		t.Fatal(text(r3))
	}
}

// MARK: Failures -> nil

func TestFailuresReturnNil(t *testing.T) {
	sys := request(sysSeverity, []string{"</severity>"}, "", "")
	ctx := context.Background()
	throwing := head(&model, caller(func(int, config.Model, map[string]any) (hooks.ChatResult, error) { return hooks.ChatResult{}, errFake }))
	if throwing.Respond(ctx, sys) != nil {
		t.Error("throwing")
	}
	if head(&model, fixed(hooks.ChatResult{Status: 500})).Respond(ctx, sys) != nil {
		t.Error("500")
	}
	if head(&model, fixed(logprobResult(pair{"The", 0.9}, pair{"5", 0.1}))).Respond(ctx, sys) != nil {
		t.Error("low mass")
	}
	none := head(nil, fixed(logprobResult(pair{"5", 1})))
	if none.Respond(ctx, sys) != nil {
		t.Error("no model")
	}
	if _, ok := none.DecisionModel(); ok {
		t.Error("no model has a decision model")
	}
	if head(&model, fixed(logprobResult(pair{"5", 1}))).Respond(ctx, request("hi", nil, "", "")) != nil {
		t.Error("not a classifier")
	}
}

func TestMissingLogprobsIsRememberedAndSkipsTheCall(t *testing.T) {
	c := fixed(hooks.ChatResult{Status: 200, JSON: parse(`{"choices":[{"message":{"content":"<severity>5"}}]}`)})
	h := head(&model, c)
	now := time.Now()
	h.now = func() time.Time { return now }
	sys := request(sysSeverity, []string{"</severity>"}, "", "")
	if h.Respond(context.Background(), sys) != nil || h.Respond(context.Background(), sys) != nil {
		t.Fatal("not nil")
	}
	if len(c.Calls()) != 1 {
		t.Fatalf("%d calls", len(c.Calls()))
	}
	// ten minutes later the model is tried again
	now = now.Add(noLogprobsTTL + time.Second)
	h.Respond(context.Background(), sys)
	if len(c.Calls()) != 2 {
		t.Fatalf("%d calls after the TTL", len(c.Calls()))
	}
	// a model declared without logprobs is never called
	m := model
	f := false
	m.Logprobs = &f
	none := fixed(logprobResult(pair{"5", 1}))
	if head(&m, none).Respond(context.Background(), sys) != nil || len(none.Calls()) != 0 {
		t.Fatal("declared without logprobs was called")
	}
}

func TestCategorySanitizing(t *testing.T) {
	if got := sanitizeCategory("  Force-Push: main! "); got != "ForcePush main" {
		t.Fatal(got)
	}
}

func TestDecisionsAreLogged(t *testing.T) {
	log := &testkit.Log{}
	h := New(&model, fixed(logprobResult(pair{"90", 1})), log)
	h.Respond(context.Background(), request(sysSeverity, []string{"</severity>"}, "", ""))
	New(&model, fixed(hooks.ChatResult{Status: 503}), log).Respond(context.Background(), request(sysSeverity, nil, "", ""))
	ev := log.All()
	if len(ev) != 2 {
		t.Fatalf("%d events", len(ev))
	}
	if e := ev[0]; e.Kind != "decision" || e.Stage != 1 || e.Model != "GLM" || e.Severity != 90 || e.PBlock != 1 || e.Fallback != "" {
		t.Fatalf("%+v", e)
	}
	if e := ev[1]; e.Kind != "decision" || e.Stage != 2 || e.Fallback != "upstream HTTP 503" {
		t.Fatalf("%+v", e)
	}
}

// MARK: Servers that refuse a prefill (Flare's DeepSeek)

// locatedResult: the model wrote "<severity>" itself, then the score token.
func locatedResult(pairs []pair, extra ...string) hooks.ChatResult {
	entry := func(tok string, top []pair) any {
		arr := []any{}
		for _, p := range top {
			arr = append(arr, map[string]any{"token": p.tok, "logprob": math.Log(p.p)})
		}
		return map[string]any{"token": tok, "logprob": -0.1, "top_logprobs": arr}
	}
	var content []any
	for _, t := range []string{"<", "sever", "ity", ">"} {
		content = append(content, entry(t, []pair{{t, 1}}))
	}
	content = append(content, entry(pairs[0].tok, pairs))
	for _, t := range extra {
		content = append(content, entry(t, []pair{{t, 1}}))
	}
	return hooks.ChatResult{Status: 200, JSON: map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": ""}, "logprobs": map[string]any{"content": content}}},
		"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
	}}
}

func TestRefusedPrefillFallsBackToLocatingTheScoreAndRemembersIt(t *testing.T) {
	refused := hooks.ChatResult{Status: 400, JSON: parse(`{"error":{"message":"DeepSeek prompt encoding does not support continue_final_message"}}`)}
	c := caller(func(i int, _ config.Model, _ map[string]any) (hooks.ChatResult, error) {
		if i == 0 {
			return refused, nil
		}
		return locatedResult([]pair{{"85", 0.8}, {"10", 0.2}}), nil
	})
	h := head(&model, c)
	req := request(sysSeverity, []string{"</severity>"}, "", "")
	out := h.Respond(context.Background(), req)
	if text(out) != "<severity>70" {
		t.Fatal(text(out))
	}
	located := c.Calls()[1].body
	if located["continue_final_message"] != nil || located["temperature"] != nil {
		t.Fatalf("located body %s", canon(located))
	}
	if canon(located["chat_template_kwargs"]) != `{"enable_thinking":false}` {
		t.Fatal(canon(located["chat_template_kwargs"]))
	}
	if canon(located["max_tokens"]) != "12" || len(jsonx.Arr(located["messages"])) != 2 {
		t.Fatalf("located body %s", canon(located))
	}
	// The next decision goes straight to the located form.
	h.Respond(context.Background(), req)
	calls := c.Calls()
	if len(calls) != 3 || calls[2].body["continue_final_message"] != nil {
		t.Fatalf("%d calls", len(calls))
	}
}

func TestLocatedUsesTheModelsThinkingOff(t *testing.T) {
	m := model
	m.ThinkingOff = map[string]any{"reasoning_effort": "low"}
	c := caller(func(i int, _ config.Model, _ map[string]any) (hooks.ChatResult, error) {
		if i == 0 {
			return hooks.ChatResult{Status: 422, JSON: parse(`{"detail":"assistant prefill is not supported"}`)}, nil
		}
		return locatedResult([]pair{{"5", 1}}), nil
	})
	if out := head(&m, c).Respond(context.Background(), request(sysSeverity, nil, "", "")); text(out) != "<severity>5</severity>" {
		t.Fatal(text(out))
	}
	b := c.Calls()[1].body
	if b["reasoning_effort"] != "low" || b["chat_template_kwargs"] != nil || canon(b["max_tokens"]) != "48" {
		t.Fatalf("located body %s", canon(b))
	}
}

func TestLocatedStageTwoReadsTheCategoryFromTheSameReply(t *testing.T) {
	refused := hooks.ChatResult{Status: 400, JSON: parse(`{"error":{"message":"continue_final_message is not supported"}}`)}
	c := caller(func(i int, _ config.Model, _ map[string]any) (hooks.ChatResult, error) {
		if i == 0 {
			return refused, nil
		}
		return locatedResult([]pair{{"90", 0.95}, {"5", 0.05}}, "</", "severity", ">", "<category>", "Force", " Push", "</", "category", ">"), nil
	})
	out := head(&model, c).Respond(context.Background(), request(sysSeverity, nil, "", ""))
	if s := text(out); !strings.HasPrefix(s, "<severity>") || !strings.HasSuffix(s, "<category>Force Push</category>") {
		t.Fatal(s)
	}
	if len(c.Calls()) != 2 {
		t.Fatalf("%d calls", len(c.Calls()))
	}
}

func TestLocatedReplyWithoutTheTagFallsBack(t *testing.T) {
	refused := hooks.ChatResult{Status: 400, JSON: parse(`{"error":{"message":"no continue_final_message"}}`)}
	thinking := hooks.ChatResult{Status: 200, JSON: parse(`{"choices":[{"message":{"content":null},"logprobs":{"content":[{"token":"We","logprob":-0.1,"top_logprobs":[{"token":"We","logprob":-0.1}]}]}}]}`)}
	c := caller(func(i int, _ config.Model, _ map[string]any) (hooks.ChatResult, error) {
		if i == 0 {
			return refused, nil
		}
		return thinking, nil
	})
	h := head(&model, c)
	req := request(sysSeverity, []string{"</severity>"}, "", "")
	if h.Respond(context.Background(), req) != nil {
		t.Fatal("not nil")
	}
	// logprobs did come back: the model is not marked as lacking them
	h.Respond(context.Background(), req)
	if len(c.Calls()) != 3 {
		t.Fatalf("%d calls", len(c.Calls()))
	}
}

func TestDecisionModel(t *testing.T) {
	m, ok := head(&model, fixed(hooks.ChatResult{})).DecisionModel()
	if !ok || m.ID != "GLM" {
		t.Fatal(m, ok)
	}
}
