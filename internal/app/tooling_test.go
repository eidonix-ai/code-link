package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codelink/internal/launch"
	"codelink/internal/tools"
)

// fakeTools answers Status from a map and runs scripted installs.
type fakeTools struct {
	mu    sync.Mutex
	info  map[launch.Tool]tools.Info
	lines []string
	err   error
	block chan struct{} // when set, an install waits for it or for cancel
	calls []string
}

func (f *fakeTools) Status(_ context.Context, t launch.Tool) tools.Info {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i, ok := f.info[t]; ok {
		return i
	}
	return tools.Info{Tool: t}
}

func (f *fakeTools) run(ctx context.Context, t launch.Tool, verb string, p func(string)) error {
	f.mu.Lock()
	f.calls = append(f.calls, verb+" "+string(t))
	lines, err, block := f.lines, f.err, f.block
	f.mu.Unlock()
	for _, l := range lines {
		p(l)
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return &tools.Error{Message: "Installing " + t.DisplayName() + " was cancelled.", Err: ctx.Err()}
		}
	}
	if err == nil {
		f.mu.Lock()
		f.info[t] = tools.Info{Tool: t, Installed: true, Version: "2.1.304", Latest: "2.1.304", CanUpdate: true}
		f.mu.Unlock()
	}
	return err
}

func (f *fakeTools) Install(ctx context.Context, t launch.Tool, p func(string)) error {
	return f.run(ctx, t, "install", p)
}
func (f *fakeTools) Update(ctx context.Context, t launch.Tool, p func(string)) error {
	return f.run(ctx, t, "update", p)
}

func labels(items []MenuItem) []string {
	var out []string
	for _, i := range items {
		out = append(out, string(i.Action)+": "+i.Label)
	}
	return out
}

func TestToolMenuStates(t *testing.T) {
	cases := []struct {
		name    string
		info    tools.Info
		checked bool
		upkeep  string // "" = none
	}{
		{"not checked yet", tools.Info{}, false, ""},
		{"missing", tools.Info{Installed: false, Method: tools.Native}, true, "install: Install Claude Code…"},
		{"outdated", tools.Info{Installed: true, Version: "2.1.296", Latest: "2.1.304", UpdateAvailable: true, CanUpdate: true, Method: tools.Native}, true,
			"update: Update Claude Code (2.1.296 → 2.1.304)"},
		{"current", tools.Info{Installed: true, Version: "2.1.304", Latest: "2.1.304", CanUpdate: true}, true, ""},
		{"unknown method", tools.Info{Installed: true, Version: "2.1.296", Latest: "2.1.304", UpdateAvailable: true, CanUpdate: false, Method: tools.Unknown}, true, ""},
		{"offline", tools.Info{Installed: true, Version: "2.1.296", CanUpdate: true}, true, ""},
	}
	for _, c := range cases {
		l, m := ToolMenu(launch.ClaudeCode, c.info, c.checked)
		if got := labels([]MenuItem{l}); !reflect.DeepEqual(got, []string{"launch: Launch Claude Code…"}) {
			t.Errorf("%s: launch item %v", c.name, got)
		}
		got := ""
		if m != nil {
			got = labels([]MenuItem{*m})[0]
		}
		if got != c.upkeep {
			t.Errorf("%s: maintenance %q, want %q", c.name, got, c.upkeep)
		}
	}
	if _, m := ToolMenu(launch.Codex, tools.Info{}, true); m == nil || m.Label != "Install Codex…" {
		t.Error(m)
	}
}

func TestToolViewsAndRefresh(t *testing.T) {
	a := newTestApp(t)
	f := a.Tools.(*fakeTools)
	f.info = map[launch.Tool]tools.Info{
		launch.ClaudeCode: {Installed: true, Version: "2.1.296", Latest: "2.1.304", UpdateAvailable: true, CanUpdate: true},
	}
	if v := a.ToolView(launch.ClaudeCode); v.Checked || v.Action != "" {
		t.Errorf("before a check: %+v", v)
	}
	var changes atomic.Int32
	a.OnChange = func() { changes.Add(1) }
	a.RefreshTools(context.Background())
	if v := a.ToolView(launch.ClaudeCode); !v.Checked || v.Action != "update" || v.Version != "2.1.296" || v.Latest != "2.1.304" {
		t.Errorf("%+v", v)
	}
	if v := a.ToolView(launch.Codex); v.Action != "install" || v.Installed || v.InstallURL != launch.Codex.InstallURL() {
		t.Errorf("%+v", v)
	}
	if changes.Load() == 0 {
		t.Error("the tray hears about it")
	}
	if lv := a.LaunchView(launch.Codex); lv.Installed || lv.ToolInfo.Action != "install" {
		t.Errorf("launch window: %+v", lv)
	}
}

func waitJob(t *testing.T, a *App, tool launch.Tool) JobView {
	t.Helper()
	for i := 0; i < 200; i++ {
		if v := a.Job(tool); v.Done {
			return v
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("job didn't finish")
	return JobView{}
}

func TestInstallStreamsLinesAndRefreshes(t *testing.T) {
	a := newTestApp(t)
	f := a.Tools.(*fakeTools)
	f.info = map[launch.Tool]tools.Info{}
	f.lines = []string{"Downloading…", "Verifying checksum"}
	var mu sync.Mutex
	var heard []JobView
	a.OnJob = func(v JobView) { mu.Lock(); heard = append(heard, v); mu.Unlock() }
	v := a.StartJob(launch.ClaudeCode, ActInstall)
	if !v.Running || v.Verb != "install" {
		t.Fatalf("%+v", v)
	}
	done := waitJob(t, a, launch.ClaudeCode)
	if !done.OK || done.Message != "Claude Code 2.1.304 is ready." || !reflect.DeepEqual(done.Lines, f.lines) {
		t.Errorf("%+v", done)
	}
	if i, ok := a.ToolInfo(launch.ClaudeCode); !ok || !i.Installed {
		t.Error("re-checked after the install")
	}
	mu.Lock()
	if len(heard) < 3 || !heard[len(heard)-1].Done {
		t.Errorf("heard %d", len(heard))
	}
	mu.Unlock()
	a.StartJob(launch.ClaudeCode, ActUpdate)
	waitJob(t, a, launch.ClaudeCode)
	f.mu.Lock()
	if f.calls[1] != "update claude" {
		t.Errorf("%v", f.calls)
	}
	f.mu.Unlock()
}

func TestFailedAndCancelledJobsSayWhy(t *testing.T) {
	a := newTestApp(t)
	f := a.Tools.(*fakeTools)
	f.info = map[launch.Tool]tools.Info{}
	f.mu.Lock()
	f.err = &tools.Error{Message: "The download failed.", Tail: []string{"curl: (6) Could not resolve host"}}
	f.mu.Unlock()
	a.StartJob(launch.Codex, ActInstall)
	v := waitJob(t, a, launch.Codex)
	if v.OK || v.Message != "The download failed." || len(v.Tail) != 1 {
		t.Errorf("%+v", v)
	}
	f.mu.Lock()
	f.err = errors.New("plain")
	f.block = make(chan struct{})
	f.mu.Unlock()
	r := a.StartJob(launch.Codex, ActInstall)
	if again := a.StartJob(launch.Codex, ActInstall); !again.Running || !reflect.DeepEqual(again.Lines, r.Lines) {
		t.Errorf("one job at a time: %+v", again)
	}
	a.CancelJob(launch.Codex)
	v = waitJob(t, a, launch.Codex)
	if v.OK || !strings.Contains(v.Message, "cancelled") {
		t.Errorf("%+v", v)
	}
}
