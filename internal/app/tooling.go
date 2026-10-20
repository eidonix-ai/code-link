package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"codelink/internal/launch"
	"codelink/internal/tools"
)

// Tools checks, installs and updates Claude Code and Codex
// (internal/tools); tests fake it.
type Tools interface {
	Status(ctx context.Context, t launch.Tool) tools.Info
	Install(ctx context.Context, t launch.Tool, progress func(string)) error
	Update(ctx context.Context, t launch.Tool, progress func(string)) error
}

type systemTools struct{}

func (systemTools) Status(ctx context.Context, t launch.Tool) tools.Info { return tools.Status(ctx, t) }
func (systemTools) Install(ctx context.Context, t launch.Tool, p func(string)) error {
	return tools.Install(ctx, t, p)
}
func (systemTools) Update(ctx context.Context, t launch.Tool, p func(string)) error {
	return tools.Update(ctx, t, p)
}

// toolState is what the app knows of the tools.
type toolState struct {
	mu   sync.Mutex
	info map[launch.Tool]tools.Info // missing = not checked yet
	jobs map[launch.Tool]*job
}

// ToolInfo is the last status of a tool; ok false before the first check.
func (a *App) ToolInfo(t launch.Tool) (tools.Info, bool) {
	a.tools.mu.Lock()
	defer a.tools.mu.Unlock()
	i, ok := a.tools.info[t]
	return i, ok
}

// RefreshTools checks both tools' installs (at start, every few hours and
// after an install or update).
func (a *App) RefreshTools(ctx context.Context) {
	for _, t := range launch.Tools {
		a.refreshTool(ctx, t)
	}
}

func (a *App) refreshTool(ctx context.Context, t launch.Tool) {
	i := a.Tools.Status(ctx, t)
	a.tools.mu.Lock()
	if a.tools.info == nil {
		a.tools.info = map[launch.Tool]tools.Info{}
	}
	a.tools.info[t] = i
	a.tools.mu.Unlock()
	a.changed()
}

// WatchTools re-checks the tools every interval until ctx ends.
func (a *App) WatchTools(ctx context.Context, interval time.Duration) {
	tk := time.NewTicker(interval)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			a.RefreshTools(ctx)
		}
	}
}

// MenuAction is what a tray item does.
type MenuAction string

const (
	ActLaunch  MenuAction = "launch"
	ActInstall MenuAction = "install"
	ActUpdate  MenuAction = "update"
)

// MenuItem is a tool's tray item.
type MenuItem struct {
	Tool   launch.Tool
	Action MenuAction
	Label  string
}

// ToolMenu are a tool's tray items: "Install Claude Code…" while it is
// missing; "Launch Claude Code…" when installed (or not checked yet), with
// "Update Claude Code (2.1.296 → 2.1.304)" when an update is available and
// code-link knows how to apply it.
func ToolMenu(t launch.Tool, info tools.Info, checked bool) []MenuItem {
	name := t.DisplayName()
	if checked && !info.Installed {
		return []MenuItem{{t, ActInstall, "Install " + name + "…"}}
	}
	out := []MenuItem{{t, ActLaunch, "Launch " + name + "…"}}
	if checked && info.UpdateAvailable && info.CanUpdate {
		out = append(out, MenuItem{t, ActUpdate, "Update " + name + " (" + info.Version + " → " + info.Latest + ")"})
	}
	return out
}

// ToolView is a tool's line in Settings and the launch window.
type ToolView struct {
	Tool      launch.Tool `json:"tool"`
	Name      string      `json:"name"`
	Checked   bool        `json:"checked"`
	Installed bool        `json:"installed"`
	Version   string      `json:"version"`
	Latest    string      `json:"latest"`
	// Action is the button: "install", "update" or "" (none).
	Action     string `json:"action"`
	InstallURL string `json:"installURL"`
	Problem    string `json:"problem"`
	Busy       bool   `json:"busy"`
}

// ToolView describes a tool for the pages.
func (a *App) ToolView(t launch.Tool) ToolView {
	info, checked := a.ToolInfo(t)
	v := ToolView{Tool: t, Name: t.DisplayName(), Checked: checked, Installed: info.Installed, Version: info.Version,
		Latest: info.Latest, InstallURL: t.InstallURL(), Problem: info.Error, Busy: a.Job(t).Running}
	switch {
	case checked && !info.Installed:
		v.Action = string(ActInstall)
	case info.UpdateAvailable && info.CanUpdate:
		v.Action = string(ActUpdate)
	}
	return v
}

// --- Install and update jobs ---

// JobView is an install or update as its window shows it.
type JobView struct {
	Tool    launch.Tool `json:"tool"`
	Name    string      `json:"name"`
	Verb    string      `json:"verb"` // install, update
	Lines   []string    `json:"lines"`
	Running bool        `json:"running"`
	Done    bool        `json:"done"`
	OK      bool        `json:"ok"`
	// Message is the outcome in plain words; Tail the installer's last lines
	// when it failed ("Show details").
	Message string   `json:"message"`
	Tail    []string `json:"tail"`
	Version string   `json:"version"`
}

type job struct {
	view   JobView
	cancel context.CancelFunc
}

// maxJobLines keeps a job's output to what the window can show.
const maxJobLines = 400

// StartJob installs or updates a tool in the background (one job per
// tool; starting one that runs returns it). OnJob hears every line and the
// end; the tools are re-checked afterwards.
func (a *App) StartJob(t launch.Tool, verb MenuAction) JobView {
	a.tools.mu.Lock()
	if a.tools.jobs == nil {
		a.tools.jobs = map[launch.Tool]*job{}
	}
	if j := a.tools.jobs[t]; j != nil && j.view.Running {
		v := j.view
		a.tools.mu.Unlock()
		return v
	}
	ctx, cancel := context.WithCancel(context.Background())
	word := "install"
	if verb == ActUpdate {
		word = "update"
	}
	j := &job{view: JobView{Tool: t, Name: t.DisplayName(), Verb: word, Lines: []string{}, Running: true}, cancel: cancel}
	a.tools.jobs[t] = j
	v := j.view
	a.tools.mu.Unlock()
	a.notifyJob(v)

	go func() {
		defer cancel()
		progress := func(line string) {
			a.tools.mu.Lock()
			j.view.Lines = append(j.view.Lines, line)
			if len(j.view.Lines) > maxJobLines {
				j.view.Lines = j.view.Lines[len(j.view.Lines)-maxJobLines:]
			}
			v := copyJob(j.view)
			a.tools.mu.Unlock()
			a.notifyJob(v)
		}
		var err error
		if verb == ActUpdate {
			err = a.Tools.Update(ctx, t, progress)
		} else {
			err = a.Tools.Install(ctx, t, progress)
		}
		a.refreshTool(context.Background(), t)
		info, _ := a.ToolInfo(t)
		a.tools.mu.Lock()
		j.view.Running, j.view.Done, j.view.OK = false, true, err == nil
		if err == nil {
			j.view.Version = info.Version
			j.view.Message = t.DisplayName() + " is ready."
			if info.Version != "" {
				j.view.Message = t.DisplayName() + " " + info.Version + " is ready."
			}
		} else {
			var te *tools.Error
			if errors.As(err, &te) {
				j.view.Message, j.view.Tail = te.Message, te.Tail
			} else {
				j.view.Message = err.Error()
			}
		}
		v := copyJob(j.view)
		a.tools.mu.Unlock()
		a.notifyJob(v)
		a.changed()
	}()
	return v
}

func copyJob(v JobView) JobView {
	v.Lines = append([]string{}, v.Lines...)
	v.Tail = append([]string{}, v.Tail...)
	return v
}

func (a *App) notifyJob(v JobView) {
	if f := a.OnJob; f != nil {
		f(v)
	}
}

// CancelJob stops a tool's running install or update.
func (a *App) CancelJob(t launch.Tool) {
	a.tools.mu.Lock()
	defer a.tools.mu.Unlock()
	if j := a.tools.jobs[t]; j != nil && j.view.Running {
		j.cancel()
	}
}

// Job is a tool's current or last job (zero when there was none).
func (a *App) Job(t launch.Tool) JobView {
	a.tools.mu.Lock()
	defer a.tools.mu.Unlock()
	if j := a.tools.jobs[t]; j != nil {
		return copyJob(j.view)
	}
	return JobView{Tool: t, Name: t.DisplayName(), Lines: []string{}}
}
