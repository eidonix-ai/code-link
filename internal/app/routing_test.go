package app

import (
	"context"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"codelink/internal/config"
	"codelink/internal/core"
	"codelink/internal/engine"
	"codelink/internal/hooks"
)

func ip(n int) *int          { return &n }
func bp(b bool) *bool        { return &b }
func ch(b, u string) *Choice { return &Choice{b, u} }

func base() config.Config {
	c := config.Default()
	c.AccessToken = "tok"
	c.Backends = []config.Backend{{ID: "a", Kind: config.OpenAICompatible, BaseURL: "http://a/v1"}, {ID: "b", Kind: config.OpenAICompatible, BaseURL: "http://b/v1"}}
	return c
}

func modelIDs(c config.Config) []string {
	var out []string
	for _, m := range c.Models {
		out = append(out, m.ID)
	}
	return out
}

func TestSettingATierCreatesTheModelWithProbedFacts(t *testing.T) {
	c := base()
	SetRole(&c, SlotMain, ch("a", "GLM"), &core.ProbedModel{Upstream: "GLM", ContextWindow: ip(131_072), Logprobs: bp(true), Vision: bp(true)})
	m, ok := c.Model(c.Roles.Main)
	if c.Roles.Main != "GLM" || !c.Configured() || !ok {
		t.Fatalf("roles %+v", c.Roles)
	}
	if *m.ContextWindow != 131_072 || !*m.Logprobs || !*m.Vision {
		t.Errorf("facts not copied: %+v", m)
	}
	// Facts the entry already has stay; missing ones are filled.
	c.Models[0].Vision = bp(false)
	c.Models[0].Logprobs = nil
	EnsureModel(&c, Choice{"a", "GLM"}, &core.ProbedModel{Upstream: "GLM", ContextWindow: ip(8), Logprobs: bp(false), Vision: bp(true)})
	if *c.Models[0].ContextWindow != 131_072 || *c.Models[0].Vision || *c.Models[0].Logprobs {
		t.Errorf("merge: %+v", c.Models[0])
	}
}

func TestTiersMayShareOrSplitProviders(t *testing.T) {
	c := base()
	SetRole(&c, SlotMain, ch("a", "GLM"), nil)
	SetRole(&c, SlotOpus, ch("a", "GLM"), nil)
	if len(c.Models) != 1 || c.Roles.Opus != "GLM" {
		t.Fatalf("shared: %+v", c)
	}
	SetRole(&c, SlotOpus, ch("b", "GLM"), nil)
	if c.Roles.Opus != "GLM (b)" || len(c.Models) != 2 {
		t.Fatalf("split: %+v %v", c.Roles, modelIDs(c))
	}
	if got, _ := ChoiceFor(&c, SlotOpus); got != (Choice{"b", "GLM"}) {
		t.Errorf("choice %+v", got)
	}
	// A third same-named model still gets a unique id.
	c2 := base()
	c2.Models = []config.Model{{ID: "GLM", BackendID: "a", Upstream: "GLM"}, {ID: "GLM (c)", BackendID: "b", Upstream: "other"}}
	if id := EnsureModel(&c2, Choice{"c", "GLM"}, nil); id != "GLM (c 2)" {
		t.Errorf("unique id %q", id)
	}
}

func TestClearingARoleKeepsTheModelEnabled(t *testing.T) {
	c := base()
	SetRole(&c, SlotMain, ch("a", "M"), nil)
	SetRole(&c, SlotSmall, ch("a", "S"), nil)
	SetRole(&c, SlotSmall, nil, nil)
	if c.Roles.Small != "" || !reflect.DeepEqual(modelIDs(c), []string{"M", "S"}) {
		t.Fatalf("%+v %v", c.Roles, modelIDs(c))
	}
	SetRole(&c, SlotMain, nil, nil)
	if c.Roles.Main != "" || c.Configured() || len(c.Models) != 2 {
		t.Fatalf("%+v", c.Roles)
	}
}

func TestTurningAModelOffClearsEveryRoleThatUsedIt(t *testing.T) {
	c := base()
	m := Choice{"a", "M"}
	SetRole(&c, SlotMain, &m, nil)
	SetRole(&c, SlotOpus, &m, nil)
	SetRole(&c, SlotCodex, ch("b", "X"), nil)
	cleared := SetEnabled(&c, m, false, nil)
	if !reflect.DeepEqual(cleared, []Slot{SlotMain, SlotOpus}) {
		t.Fatalf("cleared %v", cleared)
	}
	if c.Roles.Main != "" || c.Roles.Opus != "" || c.Roles.Codex != "X" || IsEnabled(&c, m) {
		t.Fatalf("%+v", c.Roles)
	}
	if again := SetEnabled(&c, m, false, nil); len(again) != 0 {
		t.Error("nothing to clear twice")
	}
	SetEnabled(&c, m, true, nil)
	if !IsEnabled(&c, m) || c.Roles.Main != "" {
		t.Error("re-enabling doesn't pick a role")
	}
}

func TestModelAliasesFollowIntoRoles(t *testing.T) {
	c := base()
	SetRole(&c, SlotMain, ch("a", "GLM-5.3"), nil)
	SetRole(&c, SlotOpus, ch("b", "Q"), nil)
	if !RenameModel(&c, "GLM-5.3", "sonnet-glm") {
		t.Fatal("rename refused")
	}
	if got, _ := ChoiceFor(&c, SlotMain); c.Roles.Main != "sonnet-glm" || got.Upstream != "GLM-5.3" {
		t.Errorf("%+v %+v", c.Roles, got)
	}
	if RenameModel(&c, "sonnet-glm", "Q") || RenameModel(&c, "sonnet-glm", "  ") {
		t.Error("taken and blank names are refused")
	}
}

func TestGroupedChoicesListEnabledFirstThenProbedAndHonourEnabledOnly(t *testing.T) {
	c := base()
	SetRole(&c, SlotMain, ch("b", "Q2"), &core.ProbedModel{Upstream: "Q2", ContextWindow: ip(262_144), Logprobs: bp(true)})
	probes := map[string]core.Probe{
		"a": {Reachable: true, Models: []core.ProbedModel{{Upstream: "A1", ContextWindow: ip(1_048_576)}}},
		"b": {Reachable: true, Models: []core.ProbedModel{{Upstream: "Q1"}, {Upstream: "Q2"}}},
	}
	all := Groups(&c, probes, false)
	if len(all) != 2 || all[0].Backend != "a" || all[1].Backend != "b" {
		t.Fatalf("%+v", all)
	}
	if all[1].Items[0].Title != "Q2" || all[1].Items[1].Title != "Q1" || !all[1].Items[0].Enabled || all[1].Items[1].Enabled {
		t.Errorf("enabled first: %+v", all[1].Items)
	}
	if all[1].Items[0].Facts != "262k · logprobs" || all[0].Items[0].Facts != "1M" {
		t.Errorf("facts %q %q", all[1].Items[0].Facts, all[0].Items[0].Facts)
	}
	enabled := Groups(&c, probes, true)
	if len(enabled) != 1 || enabled[0].Backend != "b" || len(enabled[0].Items) != 1 {
		t.Errorf("enabled only: %+v", enabled)
	}
	if v := (ChoiceItem{Vision: bp(true), Logprobs: bp(false)}); Facts(v) != "vision" {
		t.Errorf("vision fact %q", Facts(v))
	}
	if got, ok := ParseTag(all[1].Items[1].Tag); !ok || got != (Choice{"b", "Q1"}) {
		t.Errorf("tag round trip %+v", got)
	}
	if _, ok := ParseTag(""); ok {
		t.Error("empty tag")
	}
}

func TestEnabledModelsPersistWithoutRoles(t *testing.T) {
	c := base()
	SetEnabled(&c, Choice{"a", "Spare"}, true, &core.ProbedModel{Upstream: "Spare", ContextWindow: ip(8192)})
	c.Models[0].ID = "spare-alias"
	path := filepath.Join(t.TempDir(), "code-link.json")
	if err := config.Save(path, c); err != nil {
		t.Fatal(err)
	}
	back, warn := LoadSettings(path)
	if warn != "" || !reflect.DeepEqual(back, c) || back.Models[0].ID != "spare-alias" || *back.Models[0].ContextWindow != 8192 {
		t.Errorf("round trip:\n%+v\n%+v (%s)", back, c, warn)
	}
}

func TestShortContextLabels(t *testing.T) {
	for n, want := range map[int]string{1_048_576: "1M", 262_144: "262k", 8192: "8k", 512: "512"} {
		if got := ShortContext(n); got != want {
			t.Errorf("%d: %s, want %s", n, got, want)
		}
	}
}

func TestRenamingAProviderFollowsIntoModels(t *testing.T) {
	c := base()
	SetRole(&c, SlotMain, ch("a", "M"), nil)
	if !RenameBackend(&c, "a", "cn3") {
		t.Fatal("refused")
	}
	m, _ := c.Model(c.Roles.Main)
	if c.Backends[0].ID != "cn3" || m.BackendID != "cn3" || !c.Configured() {
		t.Errorf("%+v", c)
	}
	if RenameBackend(&c, "cn3", "b") || RenameBackend(&c, "cn3", " ") || RenameBackend(&c, "nope", "x") {
		t.Error("taken, blank and unknown are refused")
	}
}

func TestRemovingAProviderClearsItsRoles(t *testing.T) {
	c := base()
	SetRole(&c, SlotMain, ch("a", "M"), nil)
	SetRole(&c, SlotOpus, ch("b", "O"), nil)
	cleared := RemoveBackend(&c, "a")
	if !reflect.DeepEqual(cleared, []Slot{SlotMain}) || c.Roles.Main != "" || c.Roles.Opus != "O" || len(c.Backends) != 1 || c.Backends[0].ID != "b" {
		t.Errorf("%v %+v", cleared, c)
	}
}

func TestAddBackendPicksAFreeName(t *testing.T) {
	c := base()
	c.Backends = append(c.Backends, config.Backend{ID: "provider-3", BaseURL: "http://x/v1"})
	if id := AddBackend(&c); id != "provider-4" || c.Backends[3].Kind != config.OpenAICompatible || c.Backends[3].BaseURL != "http://localhost:8000/v1" {
		t.Errorf("%s %+v", id, c.Backends)
	}
}

func TestProblemsAreReportedBeforeSaving(t *testing.T) {
	c := base()
	if p := Problems(&c); len(p) != 0 {
		t.Fatalf("clean config: %v", p)
	}
	c.Backends = append(c.Backends, config.Backend{ID: "a", BaseURL: "ftp://x"})
	c.Port = 80
	p := strings.Join(Problems(&c), "\n")
	for _, want := range []string{"unique", "port", "http"} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q in %s", want, p)
		}
	}
	c = base()
	c.Roles.Main = "gone"
	c.WebSearch.Provider = config.Tavily
	c.Documents.PaddleOCR = "nope"
	p = strings.Join(Problems(&c), "\n")
	for _, want := range []string{"main model (gone)", "API key", "PaddleOCR"} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q in %s", want, p)
		}
	}
}

func TestDecisionNoteSaysWhetherLogprobsWork(t *testing.T) {
	c := base()
	if DecisionNote(&c, nil) != "" {
		t.Error("no model, no note")
	}
	SetRole(&c, SlotMain, ch("a", "M"), nil)
	if n := DecisionNote(&c, nil); !strings.Contains(n, "Not checked") {
		t.Errorf("unknown: %s", n)
	}
	probes := map[string]core.Probe{"a": {Models: []core.ProbedModel{{Upstream: "M", Logprobs: bp(false)}, {Upstream: "D", Logprobs: bp(true)}}}}
	if n := DecisionNote(&c, probes); !strings.Contains(n, "M has no logprobs") {
		t.Errorf("probed: %s", n)
	}
	SetRole(&c, SlotDecision, ch("a", "D"), &probes["a"].Models[1])
	if n := DecisionNote(&c, nil); !strings.Contains(n, "D returns logprobs") {
		t.Errorf("decision role: %s", n)
	}
}

func newTestApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir) // never the real LaunchAgents or caches
	a := New(filepath.Join(dir, "support", "code-link.json"), nil)
	a.LoginItem = &fakeLogin{}
	a.Exe = func() (string, error) { return "/Applications/code-link.app/Contents/MacOS/code-link", nil }
	t.Cleanup(a.Stop)
	return a
}

type fakeLogin struct {
	enabled bool
	exe     string
}

func (f *fakeLogin) Enabled() bool           { return f.enabled }
func (f *fakeLogin) Enable(exe string) error { f.enabled, f.exe = true, exe; return nil }
func (f *fakeLogin) Disable() error          { f.enabled = false; return nil }

func TestDisablingAUsedModelWarnsAndClearsTheRole(t *testing.T) {
	a := newTestApp(t)
	m := Choice{"a", "M"}
	v := a.EditSetRole(base(), SlotMain, m.Tag())
	v = a.EditSetRole(v.Draft, SlotOpus, m.Tag())
	v = a.EditSetEnabled(v.Draft, m.Tag(), false)
	if v.Draft.Roles.Main != "" || v.Draft.Roles.Opus != "" {
		t.Fatalf("%+v", v.Draft.Roles)
	}
	if !strings.Contains(v.Warning, "Main, Opus have no model now: M was turned off") {
		t.Errorf("warning %q", v.Warning)
	}
	if v = a.EditSetEnabled(v.Draft, m.Tag(), true); v.Warning != "" || len(v.Draft.Models) != 1 {
		t.Errorf("enabling: %+v", v)
	}
}

func TestRemovingAProviderWarnsAboutLostRoles(t *testing.T) {
	a := newTestApp(t)
	v := a.EditSetRole(base(), SlotOpus, Choice{"b", "O"}.Tag())
	v = a.EditRemoveBackend(v.Draft, "b")
	if v.Draft.Roles.Opus != "" || !strings.Contains(v.Warning, "Opus has no model now: provider b was removed") {
		t.Errorf("%+v", v)
	}
}

func TestEditsUseProbedFactsAndFollowRenames(t *testing.T) {
	a := newTestApp(t)
	a.probes["a"] = core.Probe{Reachable: true, Models: []core.ProbedModel{{Upstream: "M", ContextWindow: ip(1000), Vision: bp(true)}}}
	v := a.EditSetEnabled(base(), Choice{"a", "M"}.Tag(), true)
	if m := v.Draft.Models[0]; *m.ContextWindow != 1000 || !*m.Vision {
		t.Errorf("probed facts: %+v", m)
	}
	v = a.EditRenameBackend(v.Draft, "a", "cn3")
	if _, ok := a.Probes()["cn3"]; !ok || v.Draft.Models[0].BackendID != "cn3" || v.Groups[0].Backend != "cn3" {
		t.Errorf("rename: %+v", v)
	}
	v = a.EditRenameModel(v.Draft, "M", "glm")
	if v.Draft.Models[0].ID != "glm" {
		t.Error("alias")
	}
	v = a.EditAddBackend(v.Draft)
	if len(v.Draft.Backends) != 3 {
		t.Error("add")
	}
	if v = a.EditSetRole(v.Draft, SlotMain, ""); v.Draft.Roles.Main != "" {
		t.Error("clear")
	}
}

func TestStatusLineAndIcon(t *testing.T) {
	c := base()
	if s := MakeStatus(&c, engine.State{}, false, nil, 0); s.Kind != NotConfigured {
		t.Errorf("%+v", s)
	}
	SetRole(&c, SlotMain, ch("a", "GLM"), nil)
	if s := MakeStatus(&c, engine.State{}, false, nil, 0); s.Kind != Stopped || s.Line != "Stopped · GLM" {
		t.Errorf("%+v", s)
	}
	running := MakeStatus(&c, engine.State{Running: true, Port: 18765}, false, bp(true), 0)
	if running.Kind != Running || running.Line != "Running on 127.0.0.1:18765 · GLM" || running.Port != 18765 || running.Level != "ok" {
		t.Errorf("%+v", running)
	}
	down := MakeStatus(&c, engine.State{Running: true, Port: 18765}, false, bp(false), 0)
	if down.Kind != BackendUnreachable || !strings.Contains(down.Line, "a is unreachable") || down.Level != "error" {
		t.Errorf("%+v", down)
	}
	if s := MakeStatus(&c, engine.State{Error: "port in use"}, false, nil, 0); s.Kind != Failed || s.Line != "Failed — port in use" {
		t.Errorf("%+v", s)
	}
	if s := MakeStatus(&c, engine.State{}, true, nil, 0); s.Kind != Starting {
		t.Errorf("%+v", s)
	}
	if s := MakeStatus(&c, engine.State{Running: true, Port: 18776}, false, nil, 18775); s.Line != "Running on 127.0.0.1:18776 · GLM (port 18775 was busy)" {
		t.Errorf("%+v", s)
	}
}

func TestActivityDescribesEventsAndCaps(t *testing.T) {
	_, text, level := Describe(hooks.Event{Kind: "request", Route: "/v1/messages", Model: "GLM", Status: 529, LatencyMS: 12})
	if text != "/v1/messages · GLM · 529 · 12 ms" || level != "error" {
		t.Errorf("%q %s", text, level)
	}
	if _, _, l := Describe(hooks.Event{Kind: "request", Status: 404}); l != "warning" {
		t.Error("4xx is a warning")
	}
	_, text, level = Describe(hooks.Event{Kind: "decision", Stage: 1, Model: "GLM", PBlock: 0.5, Severity: 51, LatencyMS: 300, Fallback: "no logprobs"})
	if !strings.Contains(text, "p(block) 0.50") || !strings.Contains(text, "fallback: no logprobs") || level != "warning" {
		t.Errorf("%q %s", text, level)
	}
	if _, text, _ = Describe(hooks.Event{Kind: "websearch", Query: "go wails", Results: 1}); text != `web search "go wails" · 1 result` {
		t.Errorf("%q", text)
	}
	if _, text, _ = Describe(hooks.Event{Kind: "document", Name: "a.pdf", Pages: 3, OCRPages: 2}); text != "a.pdf · 3 pages · 2 read by OCR" {
		t.Errorf("%q", text)
	}
	var got []Entry
	act := &Activity{OnAppend: func(e Entry) { got = append(got, e) }}
	for i := 0; i < ActivityLimit+20; i++ {
		act.Record(hooks.Event{Kind: "error", Message: "e" + strings.Repeat("x", i%2)})
	}
	es := act.Entries()
	if len(es) != ActivityLimit || es[0].ID != ActivityLimit+20 || len(got) != ActivityLimit+20 {
		t.Errorf("cap: %d entries, first id %d", len(es), es[0].ID)
	}
	act.Clear()
	if len(act.Entries()) != 0 {
		t.Error("clear")
	}
}

func TestSearchTestRequests(t *testing.T) {
	tv := SearchRequest(context.Background(), config.Tavily, "k")
	if tv.Method != http.MethodPost || tv.URL.Host != "api.tavily.com" || tv.Header.Get("Authorization") != "Bearer k" {
		t.Errorf("tavily %+v", tv)
	}
	br := SearchRequest(context.Background(), config.Brave, "k")
	if br.Method != http.MethodGet || br.Header.Get("X-Subscription-Token") != "k" || br.URL.Query().Get("count") != "1" {
		t.Errorf("brave %+v", br)
	}
	for code, want := range map[int]string{200: "Key accepted", 401: "Key rejected", 403: "Key rejected", 429: "Rate limited (the key is valid)", 500: "HTTP 500"} {
		if r := SearchResult(code); r.Detail != want || r.OK != (code == 200) {
			t.Errorf("%d: %+v", code, r)
		}
	}
}
