package app

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"codelink/internal/config"
	"codelink/internal/core"
	"codelink/internal/launch"
)

func gw() config.Config {
	c := config.Default()
	c.Backends[0].APIKey = "k"
	return c
}

func probeOf(models []core.ProbedModel, d *core.GatewayDefaults) core.Probe {
	p := core.Probe{Reachable: true, Models: models}
	if d != nil {
		p.Gateway = &core.Gateway{WebSearch: true, Documents: true, Defaults: *d}
	}
	return p
}

func TestSyncAddsUpdatesAndDropsModels(t *testing.T) {
	c := gw()
	id := config.DefaultGatewayID
	SyncModels(&c, id, probeOf([]core.ProbedModel{{Upstream: "A", ContextWindow: ip(1000)}, {Upstream: "B"}}, &core.GatewayDefaults{Main: "B", Opus: "A", Small: "B", Decision: "A", Vision: "gone"}))
	if !reflect.DeepEqual(modelIDs(c), []string{"A", "B"}) {
		t.Fatalf("%v", modelIDs(c))
	}
	if c.Roles != (config.Roles{Main: "B", Opus: "A", Small: "B", Decision: "A"}) {
		t.Errorf("roles from the recommendations: %+v", c.Roles)
	}
	if r := c.Backends[0].Recommended; r == nil || r.Main != "B" || r.Vision != "gone" {
		t.Errorf("kept on the provider: %+v", r)
	}
	// The user's alias and thinkingOff survive; probed facts refresh.
	c.Models[0].ID = "my-a"
	c.Models[0].ThinkingOff = map[string]any{"reasoning_effort": "low"}
	c.Models[0].AutoCompactAt = ip(900)
	c.Launch.Last = map[string]config.LaunchChoice{"claude": {Model: "B"}}
	SyncModels(&c, id, probeOf([]core.ProbedModel{{Upstream: "A", ContextWindow: ip(2000), Logprobs: bp(true)}, {Upstream: "C"}}, nil))
	a, _ := c.Model("my-a")
	if *a.ContextWindow != 2000 || !*a.Logprobs || a.ThinkingOff == nil || *a.AutoCompactAt != 900 {
		t.Errorf("%+v", a)
	}
	// B is gone from the provider but the last launch used it; C is new.
	if !reflect.DeepEqual(modelIDs(c), []string{"my-a", "B", "C"}) {
		t.Errorf("%v", modelIDs(c))
	}
	// No recommendations any more: main is the first model.
	if c.Backends[0].Recommended != nil || c.Roles != (config.Roles{Main: "my-a"}) {
		t.Errorf("%+v %+v", c.Backends[0].Recommended, c.Roles)
	}
	c.Launch.Last = nil
	SyncModels(&c, id, probeOf([]core.ProbedModel{{Upstream: "A"}}, nil))
	if !reflect.DeepEqual(modelIDs(c), []string{"my-a"}) {
		t.Errorf("unreferenced models go: %v", modelIDs(c))
	}
	// An unreachable provider changes nothing.
	SyncModels(&c, id, core.Probe{Error: "down"})
	if len(c.Models) != 1 {
		t.Error("kept while unreachable")
	}
	// Other providers' models are untouched.
	c.Backends = append(c.Backends, config.Backend{ID: "cn3", Kind: config.OpenAICompatible, BaseURL: "http://cn3/v1"})
	c.Models = append(c.Models, config.Model{ID: "Q", BackendID: "cn3", Upstream: "q"})
	SyncModels(&c, id, probeOf([]core.ProbedModel{{Upstream: "A"}}, nil))
	if len(c.Models) != 2 {
		t.Error("other providers")
	}
}

func TestTheDefaultModelIsTheUsersElseTheServersRecommendation(t *testing.T) {
	c := gw()
	SyncModels(&c, config.DefaultGatewayID, probeOf([]core.ProbedModel{{Upstream: "A"}, {Upstream: "B"}}, &core.GatewayDefaults{Main: "B"}))
	if Recommendation(&c) != "B" || DefaultModel(&c) != "B" || c.Roles.Main != "B" {
		t.Fatal(c.Roles)
	}
	c.Launch.DefaultModel = "A"
	ApplyRoles(&c)
	if DefaultModel(&c) != "A" || c.Roles.Main != "A" || Recommendation(&c) != "B" {
		t.Error(c.Roles)
	}
	c.Launch.DefaultModel = "gone"
	if DefaultModel(&c) != "B" {
		t.Error("a missing default falls back")
	}
	empty := config.Default()
	if DefaultModel(&empty) != "" || Recommendation(&empty) != "" {
		t.Error("nothing to recommend")
	}
}

func TestRememberedChoices(t *testing.T) {
	c := gw()
	SyncModels(&c, config.DefaultGatewayID, probeOf([]core.ProbedModel{{Upstream: "A"}, {Upstream: "B"}, {Upstream: "S"}}, &core.GatewayDefaults{Main: "A", Opus: "B", Small: "S"}))
	dirs := map[string]bool{"/code/one": true, "/code/two": true}
	isDir := func(p string) bool { return dirs[p] }
	c.Launch.RecentFolders = []string{"/gone", "/code/two", "/code/one"}
	// First launch: the default model, the recommended tiers, the newest folder.
	if o := Remembered(&c, launch.ClaudeCode, isDir); o != (LaunchOptions{Model: "A", Heavy: "B", Background: "S", Folder: "/code/two"}) {
		t.Errorf("%+v", o)
	}
	// Codex has no tiers.
	if o := Remembered(&c, launch.Codex, isDir); o != (LaunchOptions{Model: "A", Folder: "/code/two"}) {
		t.Errorf("%+v", o)
	}
	// The last launch wins, "same as the model" included.
	c.Launch.Last = map[string]config.LaunchChoice{"claude": {Model: "B", Folder: "/code/one"}}
	if o := Remembered(&c, launch.ClaudeCode, isDir); o != (LaunchOptions{Model: "B", Folder: "/code/one"}) {
		t.Errorf("%+v", o)
	}
	// Gone models and folders fall back.
	c.Launch.Last["claude"] = config.LaunchChoice{Model: "X", Heavy: "Y", Background: "S", Folder: "/gone"}
	if o := Remembered(&c, launch.ClaudeCode, isDir); o != (LaunchOptions{Model: "A", Heavy: "B", Background: "S", Folder: "/code/two"}) {
		t.Errorf("%+v", o)
	}
	// A tier equal to the model reads as "same as the model".
	c.Launch.Last["claude"] = config.LaunchChoice{Model: "B", Heavy: "B"}
	if o := Remembered(&c, launch.ClaudeCode, isDir); o.Heavy != "" {
		t.Errorf("%+v", o)
	}
}

func TestRememberSavesPerToolAndCopyUsesIt(t *testing.T) {
	a := newTestApp(t)
	cfg := stubConfig(t)
	if r := a.Save(cfg); !r.OK {
		t.Fatal(r)
	}
	folder := t.TempDir()
	a.Remember(launch.ClaudeCode, LaunchOptions{Model: "Small", Heavy: "Big", Folder: folder})
	a.Remember(launch.Codex, LaunchOptions{Model: "Big", Heavy: "ignored", Folder: folder})
	back, _ := LoadSettings(a.Path)
	if back.Launch.Last["claude"] != (config.LaunchChoice{Model: "Small", Heavy: "Big", Folder: folder}) ||
		back.Launch.Last["codex"] != (config.LaunchChoice{Model: "Big", Folder: folder}) || back.Launch.RecentFolders[0] != folder {
		t.Errorf("%+v", back.Launch)
	}
	if cmd, err := a.Command(launch.ClaudeCode); err != nil || !contains(cmd, "ANTHROPIC_MODEL=Small ", "ANTHROPIC_DEFAULT_OPUS_MODEL=Big ") {
		t.Errorf("%s %v", cmd, err)
	}
	if cmd, _ := a.Command(launch.Codex); !contains(cmd, `-c 'model="Big"'`) {
		t.Error(cmd)
	}
	// Saving Settings keeps the remembered choices.
	if r := a.Save(back); !r.OK || a.Config().Launch.Last["codex"].Model != "Big" {
		t.Error("kept")
	}
	lv := a.LaunchView(launch.ClaudeCode)
	if lv.Options.Model != "Small" || !lv.Ready || !lv.Tiers || len(lv.Groups) != 1 || len(lv.Recent) != 1 {
		t.Errorf("%+v", lv)
	}
}

func contains(s string, subs ...string) bool {
	for _, x := range subs {
		if !strings.Contains(s, x) {
			return false
		}
	}
	return true
}

func TestSyncAllSavesTheProvidersModels(t *testing.T) {
	a := newTestApp(t)
	srv := gatewayStub(t, map[string]string{"main": "B", "opus": "A"})
	c := fresh(t, srv)
	c.Backends[0].APIKey = "good"
	if r := a.Save(c); !r.OK {
		t.Fatal(r)
	}
	a.SyncAll(context.Background())
	got := a.Config()
	if !reflect.DeepEqual(modelIDs(got), []string{"A", "B"}) || got.Roles.Main != "B" || got.Roles.Opus != "A" || !a.Running() {
		t.Fatalf("%+v %+v", modelIDs(got), got.Roles)
	}
	b, _ := got.Model("B")
	if b.Logprobs == nil || !*b.Logprobs || *b.ContextWindow != 1_048_576 {
		t.Errorf("probed facts %+v", b)
	}
	if back, _ := LoadSettings(a.Path); len(back.Models) != 2 {
		t.Error("saved")
	}
	// A keyless built-in gateway isn't probed.
	if Syncable(config.Default().Backends[0]) {
		t.Error("keyless")
	}
}
