package gui

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"

	"codelink/internal/app"
	"codelink/internal/config"
	"codelink/internal/core"
	"codelink/internal/hooks"
	"codelink/internal/launch"
	"codelink/internal/selfupdate"
	"codelink/internal/share"

	"github.com/wailsapp/wails/v3/pkg/application"
)

func hookError(msg string) hooks.Event { return hooks.Event{Kind: "error", Message: msg} }

// Settings is the service the Settings page calls. The page holds the
// draft; every edit that has rules (routing, roles, providers) is done here
// and answered with the new view, so the rules live in one place
// (internal/app) and are tested there.
type Settings struct {
	app *app.App
	gui *GUI
}

// Platform is what the page shows differently per OS.
type Platform struct {
	OS              string          `json:"os"`
	Terminals       []launch.Choice `json:"terminals"`
	PermissionModes []launch.Choice `json:"permissionModes"`
	DefaultPort     int             `json:"defaultPort"`
	// GatewayURL fills in a new Go-LLM-Proxy provider's address.
	GatewayURL string `json:"gatewayURL"`
}

// Snapshot is everything the page needs when it opens.
type Snapshot struct {
	Saved       config.Config         `json:"saved"`
	View        app.View              `json:"view"`
	Status      app.Status            `json:"status"`
	Platform    Platform              `json:"platform"`
	Probes      map[string]core.Probe `json:"probes"`
	LoadWarning string                `json:"loadWarning"`
	Activity    []app.Entry           `json:"activity"`
	Path        string                `json:"path"`
	// GetStarted is set when only a key is missing: Settings opens on
	// "Paste your API key".
	GetStarted *app.KeyNeed `json:"getStarted"`
	// Tools are Claude Code's and Codex's installs.
	Tools []app.ToolView `json:"tools"`
}

// Load is the saved settings with their view, the status and the platform.
func (s *Settings) Load() Snapshot {
	cfg := s.app.Config()
	return Snapshot{
		Saved:  cfg,
		View:   s.app.Describe(cfg),
		Status: s.app.Status(),
		Platform: Platform{OS: runtime.GOOS, Terminals: launch.TerminalChoices(runtime.GOOS),
			PermissionModes: launch.PermissionModes, DefaultPort: config.DefaultPort, GatewayURL: config.GoLLMProxyURL},
		Probes:      s.app.Probes(),
		LoadWarning: s.app.LoadWarning(),
		Activity:    s.app.Activity.Entries(),
		Path:        s.app.Path,
		GetStarted:  getStarted(&cfg),
		Tools:       s.Tools(),
	}
}

func getStarted(c *config.Config) *app.KeyNeed {
	b, ok := app.KeyOnlySetup(c)
	if !ok {
		return nil
	}
	n := app.Needs(c, []string{b.ID})[0]
	return &n
}

// Status is the engine's status line.
func (s *Settings) Status() app.Status { return s.app.Status() }

// Describe is the view of a draft (after an edit the page made itself).
func (s *Settings) Describe(draft config.Config) app.View { return s.app.Describe(draft) }

// AddProvider adds a provider.
func (s *Settings) AddProvider(draft config.Config) app.View { return s.app.EditAddBackend(draft) }

// RenameProvider renames a provider.
func (s *Settings) RenameProvider(draft config.Config, old, name string) app.View {
	return s.app.EditRenameBackend(draft, old, name)
}

// RemoveProvider removes a provider and its models.
func (s *Settings) RemoveProvider(draft config.Config, id string) app.View {
	return s.app.EditRemoveBackend(draft, id)
}

// Probe tests a provider as the draft has it (unsaved URL and key) and
// syncs its models into the draft.
func (s *Settings) Probe(ctx context.Context, draft config.Config, id string) app.ProbeView {
	return s.app.EditProbe(ctx, draft, id)
}

// SetDefaultModel sets the default model ("" = the server's recommendation).
func (s *Settings) SetDefaultModel(draft config.Config, id string) app.View {
	return s.app.EditDefaultModel(draft, id)
}

// Tools are Claude Code's and Codex's installs.
func (s *Settings) Tools() []app.ToolView {
	var out []app.ToolView
	for _, t := range launch.Tools {
		out = append(out, s.app.ToolView(t))
	}
	return out
}

// Install installs ("install") or updates ("update") a tool, showing its
// progress window.
func (s *Settings) Install(tool launch.Tool, verb app.MenuAction) app.JobView {
	return s.gui.OpenInstall(tool, verb)
}

// Job is a tool's current or last install or update.
func (s *Settings) Job(tool launch.Tool) app.JobView { return s.app.Job(tool) }

// CancelJob stops it.
func (s *Settings) CancelJob(tool launch.Tool) { s.app.CancelJob(tool) }

// LaunchView is what a tool's launch window shows.
func (s *Settings) LaunchView(tool launch.Tool) app.LaunchView { return s.app.LaunchView(tool) }

// ChooseFolder asks for a launch folder; "" when cancelled.
func (s *Settings) ChooseFolder(tool launch.Tool, current string) string {
	return s.gui.chooseFolder(tool, current)
}

// LaunchResult is a launch's answer: "" or the problem in plain words.
type LaunchResult struct {
	Error string `json:"error"`
	// Missing: the CLI isn't installed (the window offers Install).
	Missing bool `json:"missing"`
}

// Launch starts a tool with the window's choices and hides the window.
func (s *Settings) Launch(tool launch.Tool, opts app.LaunchOptions) LaunchResult {
	err := s.app.Launch(tool, opts)
	var missing *app.MissingCLIError
	switch {
	case err == nil:
		if w := s.gui.launch[tool]; w != nil {
			w.Hide()
		}
		return LaunchResult{}
	case errors.As(err, &missing):
		return LaunchResult{Error: err.Error(), Missing: true}
	case errors.Is(err, app.ErrNotConfigured):
		return LaunchResult{Error: "There are no models yet. Open Settings and add a provider (or paste your API key)."}
	}
	return LaunchResult{Error: upperFirst(err.Error())}
}

// OpenLaunch shows a tool's launch window.
func (s *Settings) OpenLaunch(tool launch.Tool) { s.gui.OpenLaunch(tool) }

// CloseWindow hides a tool's launch or install window ("launch" / "install").
func (s *Settings) CloseWindow(kind string, tool launch.Tool) {
	var w *application.WebviewWindow
	if kind == "install" {
		w = s.gui.install[tool]
	} else {
		w = s.gui.launch[tool]
	}
	if w != nil {
		w.Hide()
	}
}

// OpenSettings shows Settings (from a launch window).
func (s *Settings) OpenSettings() { s.gui.OpenSettings() }

// TestOCR checks a PaddleOCR URL.
func (s *Settings) TestOCR(ctx context.Context, url string) app.CheckResult {
	return s.app.CheckOCR(ctx, url)
}

// TestSearch checks a web search key.
func (s *Settings) TestSearch(ctx context.Context, provider config.SearchProvider, key string) app.CheckResult {
	return s.app.CheckSearch(ctx, provider, key)
}

// Save writes the draft and applies it live.
func (s *Settings) Save(draft config.Config) app.SaveResult { return s.app.Save(draft) }

// Activity is Recent activity, newest first.
func (s *Settings) Activity() []app.Entry { return s.app.Activity.Entries() }

// ClearActivity empties it.
func (s *Settings) ClearActivity() { s.app.Activity.Clear() }

// SetSearch applies the web search picker.
func (s *Settings) SetSearch(draft config.Config, value string) app.View {
	return s.app.EditSetSearch(draft, value)
}

// SetDocuments applies the documents picker.
func (s *Settings) SetDocuments(draft config.Config, value string) app.View {
	return s.app.EditSetDocuments(draft, value)
}

// ApplyKeys puts keys on providers (testing and enabling a Go-LLM-Proxy)
// and, with save, saves when they all work.
func (s *Settings) ApplyKeys(ctx context.Context, draft config.Config, keys map[string]string, save bool) app.KeysResult {
	return s.app.ApplyKeys(ctx, draft, keys, save)
}

// OpenURL opens a link (the gateway's "Get a key" page) in the browser.
func (s *Settings) OpenURL(url string) {
	if strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://") {
		s.gui.wails.Browser.OpenURL(url)
	}
}

// Export asks where to save a .codelink file of the saved settings and
// writes it; "" when cancelled.
func (s *Settings) Export() (string, error) {
	path, err := s.gui.wails.Dialog.SaveFile().
		SetFilename("code-link settings"+share.Extension).
		AddFilter("code-link settings", "*"+share.Extension).
		AttachToWindow(s.gui.settings).
		PromptForSingleSelection()
	if err != nil || path == "" {
		return "", err
	}
	if !strings.HasSuffix(strings.ToLower(path), share.Extension) {
		path += share.Extension
	}
	return path, s.app.ExportTo(path)
}

// Import asks for a .codelink file and previews it; nil when cancelled.
func (s *Settings) Import() (*app.ImportPreview, error) {
	path, err := s.gui.wails.Dialog.OpenFile().
		CanChooseFiles(true).
		AddFilter("code-link settings", "*"+share.Extension).
		AttachToWindow(s.gui.settings).
		PromptForSingleSelection()
	if err != nil || path == "" {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p, err := app.PreviewImport(data)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ImportInto merges a previewed file into the draft.
func (s *Settings) ImportInto(draft config.Config, data string) (app.ImportResult, error) {
	return s.app.ImportInto(draft, []byte(data))
}

// Update is code-link's own version and update state.
func (s *Settings) Update() selfupdate.State { return s.gui.updater.State() }

// CheckForUpdates checks for a newer code-link now.
func (s *Settings) CheckForUpdates(ctx context.Context) selfupdate.State {
	return s.gui.updater.Check(ctx)
}

// StartUpdate installs the newer code-link, showing its progress window.
func (s *Settings) StartUpdate() selfupdate.State { return s.gui.OpenUpdate() }

// CancelUpdate stops an update that is still downloading.
func (s *Settings) CancelUpdate() { s.gui.updater.Cancel() }

// CloseUpdate hides the update's progress window.
func (s *Settings) CloseUpdate() { s.gui.updateWin.Hide() }

// Close hides the Settings window.
func (s *Settings) Close() { s.gui.settings.Hide() }
