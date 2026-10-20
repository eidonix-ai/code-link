package app

import (
	"os"

	"codelink/internal/config"
	"codelink/internal/launch"
)

// LaunchOptions are a launch window's choices.
type LaunchOptions struct {
	Model string `json:"model"`
	// Heavy (Opus / Fable) and Background (Haiku) for Claude Code; "" =
	// the same as Model.
	Heavy      string `json:"heavy"`
	Background string `json:"background"`
	Folder     string `json:"folder"`
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// Remembered are the choices a tool's launch window opens with: the last
// ones (when their models and folder still exist), else the default model,
// the gateway's recommended heavy and background models (else the same
// as the model) and the most recent folder. dir checks a folder (nil =
// the file system).
func Remembered(c *config.Config, t launch.Tool, dir func(string) bool) LaunchOptions {
	if dir == nil {
		dir = isDir
	}
	exists := func(id string) bool { _, ok := c.Model(id); return ok }
	last, had := c.Launch.Last[string(t)]
	o := LaunchOptions{Model: DefaultModel(c)}
	if had && exists(last.Model) {
		o.Model = last.Model
	}
	if t == launch.ClaudeCode {
		if had && (last.Heavy == "" || exists(last.Heavy)) {
			o.Heavy = last.Heavy
		} else {
			o.Heavy = c.Roles.Opus
		}
		if had && (last.Background == "" || exists(last.Background)) {
			o.Background = last.Background
		} else {
			o.Background = c.Roles.Small
		}
		if o.Heavy == o.Model {
			o.Heavy = ""
		}
		if o.Background == o.Model {
			o.Background = ""
		}
	}
	if had && last.Folder != "" && dir(last.Folder) {
		o.Folder = last.Folder
	} else if r := ExistingFolders(c.Launch.RecentFolders, dir); len(r) > 0 {
		o.Folder = r[0]
	}
	return o
}

// Remember keeps a launch's choices for the tool's next launch window and
// its folder among the recent ones (saved right away).
func (a *App) Remember(t launch.Tool, o LaunchOptions) {
	a.mu.Lock()
	if a.cfg.Launch.Last == nil {
		a.cfg.Launch.Last = map[string]config.LaunchChoice{}
	}
	ch := config.LaunchChoice{Model: o.Model, Folder: NormalizeFolder(o.Folder)}
	if t == launch.ClaudeCode {
		ch.Heavy, ch.Background = o.Heavy, o.Background
	}
	a.cfg.Launch.Last[string(t)] = ch
	if o.Folder != "" {
		a.cfg.Launch.RecentFolders = AddRecent(a.cfg.Launch.RecentFolders, o.Folder)
	}
	config.Save(a.Path, a.cfg)
	a.mu.Unlock()
	a.changed()
}

// LaunchView is what a tool's launch window shows.
type LaunchView struct {
	Tool     launch.Tool   `json:"tool"`
	ToolName string        `json:"toolName"`
	Groups   []ChoiceGroup `json:"groups"`
	Options  LaunchOptions `json:"options"`
	// Recent are the remembered folders that still exist.
	Recent []string `json:"recent"`
	// Installed: the CLI was found. ToolInfo has its version and the
	// install / update button.
	Installed bool     `json:"installed"`
	ToolInfo  ToolView `json:"toolInfo"`
	// Ready: there is a model to launch with.
	Ready bool `json:"ready"`
	// Tiers: the window offers the heavy and background models (Claude Code).
	Tiers bool `json:"tiers"`
}

// LaunchView describes a tool's launch window.
func (a *App) LaunchView(t launch.Tool) LaunchView {
	cfg := a.Config()
	tv := a.ToolView(t)
	installed := tv.Installed
	if !tv.Checked {
		_, installed = a.Find(t)
	}
	return LaunchView{
		Tool: t, ToolName: t.DisplayName(),
		Groups:    Groups(&cfg, nil, true),
		Options:   Remembered(&cfg, t, nil),
		Recent:    ExistingFolders(cfg.Launch.RecentFolders, nil),
		Installed: installed, ToolInfo: tv,
		Ready: len(cfg.Models) > 0,
		Tiers: t == launch.ClaudeCode,
	}
}
