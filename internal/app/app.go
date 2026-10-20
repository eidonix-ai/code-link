// Package app is code-link's state behind the tray and the Settings
// window: the saved settings, the engine running them, its status, launches
// and Recent activity (docs/DESIGN.md §3, §6). It has no UI code; the Wails
// layer (internal/gui) calls it, and so do the tests.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"codelink/internal/config"
	"codelink/internal/core"
	"codelink/internal/engine"
	"codelink/internal/hooks"
	"codelink/internal/launch"
	"codelink/internal/loginitem"
	"codelink/internal/upstream"
)

// LoginItem opens code-link at login (internal/loginitem); tests fake it.
type LoginItem interface {
	Enabled() bool
	Enable(exe string) error
	Disable() error
}

type systemLoginItem struct{}

func (systemLoginItem) Enabled() bool           { return loginitem.Enabled() }
func (systemLoginItem) Enable(exe string) error { return loginitem.Enable(exe) }
func (systemLoginItem) Disable() error          { return loginitem.Disable() }

// App is the running code-link.
type App struct {
	// Path is the settings file; claude-settings.json goes next to it.
	Path     string
	Activity *Activity
	// OnChange is called after the status, the settings or the recent
	// folders change (from any goroutine).
	OnChange func()

	engine *engine.Engine
	client *upstream.Client
	http   *http.Client

	// OnJob hears an install or update's progress and end.
	OnJob func(JobView)

	// Seams for tests.
	LoginItem LoginItem
	Tools     Tools
	Find      func(launch.Tool) (launch.Found, bool)
	Open      func(o launch.Opener, command, folder, pathDir string) error
	Exe       func() (string, error)

	mu          sync.Mutex
	cfg         config.Config
	starting    bool
	startErr    string
	busyPort    int // the configured port, taken, while the engine runs on another
	reachable   *bool
	probes      map[string]core.Probe
	loadWarning string
	launching   bool
	tools       toolState
}

// New loads the settings at path and builds the engine (not started).
// factory builds the feature hooks; nil = none.
func New(path string, factory hooks.Factory) *App {
	cfg, warning := LoadSettings(path)
	a := &App{Path: path, Activity: &Activity{}, client: upstream.NewClient(), http: &http.Client{},
		LoginItem: systemLoginItem{}, Tools: systemTools{}, Find: launch.Find, Exe: loginitem.Exe,
		Open: func(o launch.Opener, command, folder, pathDir string) error { return o.Open(command, folder, pathDir) },
		cfg:  cfg, probes: map[string]core.Probe{}, loadWarning: warning}
	a.engine = engine.New(cfg, a.Activity, factory)
	if warning != "" {
		a.Activity.Record(hooks.Event{Kind: "error", Message: warning})
	}
	return a
}

// LoadSettings reads the settings file. A file that isn't valid JSON is set
// aside as code-link.unreadable.json (never overwritten) and the defaults
// are used, with a warning saying so.
func LoadSettings(path string) (config.Config, string) {
	c, err := config.Load(path)
	if err == nil {
		return c, ""
	}
	var syntax *json.SyntaxError
	var typ *json.UnmarshalTypeError
	if errors.As(err, &syntax) || errors.As(err, &typ) {
		aside := filepath.Join(filepath.Dir(path), "code-link.unreadable.json")
		os.Remove(aside)
		if rerr := os.Rename(path, aside); rerr == nil {
			c2, err2 := config.Load(path)
			if err2 == nil {
				return c2, fmt.Sprintf("%s could not be read (%v); it was moved to %s.", filepath.Base(path), err, filepath.Base(aside))
			}
			c, err = c2, err2
		}
	}
	// Unreadable and not set aside: run on the defaults without saving.
	if c.AccessToken == "" {
		c.AccessToken = config.NewAccessToken()
	}
	return c, fmt.Sprintf("Settings could not be read: %v. Saving will replace them.", err)
}

// Dir is the folder of the settings file.
func (a *App) Dir() string { return filepath.Dir(a.Path) }

// Config is the saved settings.
func (a *App) Config() config.Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

// Configured: the saved settings have a main model on a known provider.
func (a *App) Configured() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.Configured()
}

// LoadWarning is the note from loading the settings ("" = none).
func (a *App) LoadWarning() string { return a.loadWarning }

// Engine is the engine (for tests and the headless mode).
func (a *App) Engine() *engine.Engine { return a.engine }

func (a *App) changed() {
	if f := a.OnChange; f != nil {
		f()
	}
}

// Status is the engine's state in words.
func (a *App) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.engine.State()
	if !st.Running && st.Error == "" {
		st.Error = a.startErr
	}
	return MakeStatus(&a.cfg, st, a.starting, a.reachable, a.busyPort)
}

// Running: the engine is listening.
func (a *App) Running() bool { return a.engine.State().Running }

// PortRange is how many ports after the configured one are tried.
const PortRange = 10

func portFree(port int) bool {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// StartOnFreePort starts the engine on the configured port or, when another
// program holds it, the first free one of the next PortRange; it returns
// the port.
func StartOnFreePort(e *engine.Engine, cfg config.Config) (int, error) {
	var last error
	for p := cfg.Port; p <= cfg.Port+PortRange && p <= 65535; p++ {
		if !portFree(p) {
			continue
		}
		c := cfg
		c.Port = p
		e.Apply(c)
		if last = e.Start(); last == nil {
			return e.State().Port, nil
		}
	}
	e.Apply(cfg)
	if last != nil {
		return 0, last
	}
	return 0, fmt.Errorf("ports %d–%d are all in use", cfg.Port, cfg.Port+PortRange)
}

// Start starts the engine when the settings are complete (on the next free
// port when the configured one is taken) and checks the main backend.
func (a *App) Start() error {
	a.mu.Lock()
	cfg := a.cfg
	if !cfg.Configured() || a.engine.State().Running || a.starting {
		a.mu.Unlock()
		return nil
	}
	a.starting, a.startErr = true, ""
	a.mu.Unlock()
	a.changed()

	port, err := StartOnFreePort(a.engine, cfg)
	a.mu.Lock()
	a.starting, a.busyPort = false, 0
	if err != nil {
		a.startErr = err.Error()
	} else if port != cfg.Port {
		a.busyPort = cfg.Port
	}
	a.mu.Unlock()
	if err == nil && port != cfg.Port {
		a.Activity.Record(hooks.Event{Kind: "notice", Message: fmt.Sprintf("Port %d was busy; the engine runs on %d.", cfg.Port, port)})
	}
	a.changed()
	if err == nil {
		go a.CheckMainBackend(context.Background())
	}
	return err
}

// Stop stops the engine.
func (a *App) Stop() {
	a.engine.Stop()
	a.mu.Lock()
	a.reachable, a.busyPort = nil, 0
	a.mu.Unlock()
	a.changed()
}

// CheckMainBackend asks the main model's backend for its model list (5 s);
// the answer drives the "unreachable" status.
func (a *App) CheckMainBackend(ctx context.Context) {
	a.mu.Lock()
	cfg := a.cfg
	a.mu.Unlock()
	m, ok := cfg.Model(cfg.Roles.Main)
	b, okb := cfg.Backend(m.BackendID)
	if !ok || !okb || !a.Running() {
		return
	}
	_, err := core.ListModels(ctx, a.client, b, 5*time.Second)
	ok = err == nil
	a.mu.Lock()
	same := a.reachable != nil && *a.reachable == ok
	a.reachable = &ok
	a.mu.Unlock()
	if !same {
		a.changed()
	}
}

// Watch re-checks the main backend every interval until ctx ends.
func (a *App) Watch(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.CheckMainBackend(ctx)
		}
	}
}

// SaveResult is Save's answer.
type SaveResult struct {
	OK       bool          `json:"ok"`
	Problems []string      `json:"problems"`
	Note     string        `json:"note"`
	Saved    config.Config `json:"saved"`
	Status   Status        `json:"status"`
}

// Save validates and writes the settings, then applies them live: routing,
// models, roles and hooks change for the next request, a new port moves the
// listener, and the engine starts or stops with the main model. The access
// token and the recent folders are the app's, not the page's.
func (a *App) Save(draft config.Config) SaveResult {
	ApplyRoles(&draft)
	if p := Problems(&draft); len(p) > 0 {
		return SaveResult{Problems: p, Saved: a.Config(), Status: a.Status()}
	}
	DropOrphanModels(&draft)
	a.mu.Lock()
	old := a.cfg
	draft.AccessToken = old.AccessToken
	draft.Launch.RecentFolders = old.Launch.RecentFolders
	draft.Launch.Last = old.Launch.Last
	if err := config.Save(a.Path, draft); err != nil {
		a.mu.Unlock()
		return SaveResult{Problems: []string{"Could not save: " + err.Error()}, Saved: old, Status: a.Status()}
	}
	launch.EnsureClaudeSettings(a.Dir())
	a.cfg = draft
	a.loadWarning = ""
	a.mu.Unlock()

	var notes []string
	if draft.Launch.OpenAtLogin != old.Launch.OpenAtLogin {
		if err := a.SyncLoginItem(); err != nil {
			notes = append(notes, "Open at login: "+err.Error())
		}
	}
	if n := a.applyEngine(old, draft); n != "" {
		notes = append(notes, n)
	}
	a.changed()
	res := SaveResult{OK: true, Saved: draft, Status: a.Status()}
	if len(notes) > 0 {
		res.Note = notes[0]
		for _, n := range notes[1:] {
			res.Note += " " + n
		}
	}
	return res
}

func (a *App) applyEngine(old, cfg config.Config) string {
	if !cfg.Configured() {
		a.engine.Stop()
		a.engine.Apply(cfg)
		a.mu.Lock()
		a.busyPort, a.reachable = 0, nil
		a.mu.Unlock()
		return ""
	}
	st := a.engine.State()
	if !st.Running {
		a.engine.Apply(cfg)
		if err := a.Start(); err != nil {
			return "The engine could not start: " + err.Error()
		}
		return ""
	}
	apply := cfg
	a.mu.Lock()
	if cfg.Port == old.Port && a.busyPort != 0 {
		apply.Port = st.Port // stay on the free port found at start
	} else {
		a.busyPort = 0
	}
	a.reachable = nil
	a.mu.Unlock()
	if err := a.engine.Apply(apply); err != nil {
		a.mu.Lock()
		a.busyPort = cfg.Port
		a.mu.Unlock()
		return fmt.Sprintf("Port %d is busy; the engine stays on %d.", cfg.Port, st.Port)
	}
	go a.CheckMainBackend(context.Background())
	return ""
}

// SyncLoginItem registers the current program to open at login when the
// settings say so (following a moved app) and removes it otherwise.
func (a *App) SyncLoginItem() error {
	if !a.Config().Launch.OpenAtLogin {
		return a.LoginItem.Disable()
	}
	exe, err := a.Exe()
	if err != nil {
		return err
	}
	return a.LoginItem.Enable(exe)
}

// Probe tests a provider (reachable, models, context windows, logprobs,
// vision, latency) and remembers the answer for the model pickers.
func (a *App) Probe(ctx context.Context, b config.Backend) core.Probe {
	p := a.engine.Probe(ctx, b)
	a.mu.Lock()
	a.probes[b.ID] = p
	a.mu.Unlock()
	return p
}

// Probes are the latest probe per provider id.
func (a *App) Probes() map[string]core.Probe {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]core.Probe, len(a.probes))
	for k, v := range a.probes {
		out[k] = v
	}
	return out
}

// RenameProbe follows a provider rename in the remembered probes.
func (a *App) RenameProbe(old, name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if p, ok := a.probes[old]; ok {
		delete(a.probes, old)
		a.probes[name] = p
	}
}

// CheckOCR and CheckSearch run the Documents and Web search Test buttons.
func (a *App) CheckOCR(ctx context.Context, url string) CheckResult {
	return CheckOCR(ctx, a.http, url)
}

func (a *App) CheckSearch(ctx context.Context, p config.SearchProvider, key string) CheckResult {
	return CheckSearch(ctx, a.http, p, key)
}

// LaunchContext is what a session is rendered with: the port actually
// listening, the access token, the --settings file, the permission mode and
// the session's models.
func (a *App) LaunchContext(o LaunchOptions) (launch.Context, error) {
	a.mu.Lock()
	cfg := a.cfg
	a.mu.Unlock()
	port := cfg.Port
	if st := a.engine.State(); st.Running {
		port = st.Port
	}
	settings, err := launch.EnsureClaudeSettings(a.Dir())
	if err != nil {
		return launch.Context{}, err
	}
	return launch.Context{Config: &cfg, Port: port, Token: cfg.AccessToken, ClaudeSettingsPath: settings,
		PermissionMode: cfg.Launch.PermissionMode, Model: o.Model, Heavy: o.Heavy, Background: o.Background}, nil
}

// Command is a tool's command in this platform's shell with the remembered
// choices, for "Copy command".
func (a *App) Command(t launch.Tool) (string, error) {
	cfg := a.Config()
	o := Remembered(&cfg, t, nil)
	if o.Model == "" {
		return "", ErrNotConfigured
	}
	c, err := a.LaunchContext(o)
	if err != nil {
		return "", err
	}
	return launch.Render(t, c, launch.NativeShell())
}

// ErrNotConfigured: there is no main model to launch with.
var ErrNotConfigured = errors.New("add a provider and pick a main model in Settings first")

// ErrBusy: a launch is already under way.
var ErrBusy = errors.New("a launch is already under way")

// MissingCLIError: the tool isn't installed (or not where it can be found).
type MissingCLIError struct{ Tool launch.Tool }

func (e *MissingCLIError) Error() string {
	return fmt.Sprintf("%s (%s) wasn't found on your PATH or in the usual install locations. Install %s, then launch again.",
		e.Tool.DisplayName(), e.Tool.Executable(), e.Tool.DisplayName())
}

// TerminalError: no terminal could be opened.
type TerminalError struct {
	Tool launch.Tool
	Err  error
}

func (e *TerminalError) Error() string {
	return fmt.Sprintf("Couldn't open a terminal: %v. Use “Copy %s command” and run it yourself.", e.Err, e.Tool.DisplayName())
}

func (e *TerminalError) Unwrap() error { return e.Err }

// Launch opens a tool in a terminal (docs/DESIGN.md §3) with the launch
// window's choices: it checks the model, finds the CLI, starts the engine
// if needed, writes the launch script and remembers the choices.
func (a *App) Launch(t launch.Tool, o LaunchOptions) error {
	a.mu.Lock()
	if a.launching {
		a.mu.Unlock()
		return ErrBusy
	}
	cfg := a.cfg
	a.launching = true
	a.mu.Unlock()
	defer func() { a.mu.Lock(); a.launching = false; a.mu.Unlock() }()

	if _, ok := cfg.Model(o.Model); !ok {
		if len(cfg.Models) == 0 {
			return ErrNotConfigured
		}
		return fmt.Errorf("the model %q isn't available any more; pick another", o.Model)
	}
	for _, m := range []string{o.Heavy, o.Background} {
		if _, ok := cfg.Model(m); m != "" && !ok {
			return fmt.Errorf("the model %q isn't available any more; pick another", m)
		}
	}
	if o.Folder == "" {
		return errors.New("choose a folder to start in")
	}
	if st, err := os.Stat(o.Folder); err != nil || !st.IsDir() {
		return fmt.Errorf("the folder %s doesn't exist any more", o.Folder)
	}
	found, ok := a.Find(t)
	if !ok {
		return &MissingCLIError{t}
	}
	if !a.Running() {
		a.Start()
	}
	if !a.Running() {
		msg := a.Status().Line
		return fmt.Errorf("the code-link engine isn't running (%s)", msg)
	}
	c, err := a.LaunchContext(o)
	if err != nil {
		return err
	}
	cmd, err := launch.Render(t, c, launch.NativeShell())
	if err != nil {
		return err
	}
	dir, err := launch.LaunchDir()
	if err != nil {
		return &TerminalError{t, err}
	}
	if err := a.Open(launch.Opener{Dir: dir, Terminal: cfg.Launch.Terminal}, cmd, o.Folder, found.PathDir); err != nil {
		return &TerminalError{t, err}
	}
	a.Remember(t, o)
	return nil
}

// saveSynced writes settings changed by a model sync and applies them.
func (a *App) saveSynced(cfg config.Config) {
	a.mu.Lock()
	old := a.cfg
	a.cfg = cfg
	config.Save(a.Path, cfg)
	a.mu.Unlock()
	a.applyEngine(old, cfg)
	a.changed()
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// RecentFolders are the launch folders that still exist, newest first.
func (a *App) RecentFolders() []string {
	return ExistingFolders(a.Config().Launch.RecentFolders, nil)
}

// AddRecentFolder remembers a launch folder (saved right away).
func (a *App) AddRecentFolder(p string) {
	a.mu.Lock()
	a.cfg.Launch.RecentFolders = AddRecent(a.cfg.Launch.RecentFolders, p)
	config.Save(a.Path, a.cfg)
	a.mu.Unlock()
	a.changed()
}

// ClearRecentFolders forgets them.
func (a *App) ClearRecentFolders() {
	a.mu.Lock()
	a.cfg.Launch.RecentFolders = nil
	config.Save(a.Path, a.cfg)
	a.mu.Unlock()
	a.changed()
}
