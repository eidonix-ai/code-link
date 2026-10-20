package gui

import (
	"context"
	"os"
	"runtime"
	"strings"

	"codelink/internal/app"
	"codelink/internal/config"
	"codelink/internal/core"
	"codelink/internal/hooks"
	"codelink/internal/launch"
	"codelink/internal/share"
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

// SetRole points a role at a model by its picker tag ("" = same as main / none).
func (s *Settings) SetRole(draft config.Config, slot app.Slot, tag string) app.View {
	return s.app.EditSetRole(draft, slot, tag)
}

// SetEnabled ticks or unticks a provider's model.
func (s *Settings) SetEnabled(draft config.Config, tag string, on bool) app.View {
	return s.app.EditSetEnabled(draft, tag, on)
}

// RenameModel changes the name sessions see for a model.
func (s *Settings) RenameModel(draft config.Config, old, name string) app.View {
	return s.app.EditRenameModel(draft, old, name)
}

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

// Probe tests a provider as the page has it (unsaved URL and key).
func (s *Settings) Probe(ctx context.Context, b config.Backend) core.Probe {
	return s.app.Probe(ctx, b)
}

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

// Close hides the Settings window.
func (s *Settings) Close() { s.gui.settings.Hide() }
