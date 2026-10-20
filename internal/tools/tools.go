// Package tools installs and updates Claude Code and Codex for the user,
// without admin rights, from their official sources:
//
//   - Claude Code: Anthropic's native installer (claude.ai/install.sh or
//     install.ps1); updates with `claude update`, or through npm, Homebrew
//     or WinGet when it was installed with one of them.
//   - Codex: the codex-package bundle of OpenAI's GitHub release, checked
//     against the release's SHA-256 sums and unpacked under
//     launch.CodexDir; updates through npm, Homebrew, WinGet or OpenAI's own
//     installer when one of them put it there.
//
// It has no UI: the Settings window calls Status, Install and Update.
// Detection is internal/launch's, so what Status finds is what a launch
// runs.
package tools

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"codelink/internal/launch"
)

// Tool is launch's: ClaudeCode or Codex.
type Tool = launch.Tool

const (
	ClaudeCode = launch.ClaudeCode
	Codex      = launch.Codex
)

// Method is how an install was made, which decides how it is updated.
type Method string

const (
	Native     Method = "native"     // Claude Code's native installer
	NPM        Method = "npm"        // npm install -g
	Brew       Method = "brew"       // Homebrew cask or formula
	WinGet     Method = "winget"     // WinGet package
	GitHub     Method = "github"     // code-link's install of Codex from GitHub releases
	Standalone Method = "standalone" // OpenAI's Codex installer (chatgpt.com/codex/install.sh)
	Unknown    Method = "unknown"    // pnpm, bun, Volta, Cargo, a copy of its own…
)

// Info is what Status reports.
type Info struct {
	Tool      Tool
	Installed bool
	// Path is the command found; "" when not installed (or a shell alias).
	Path string
	// Version is `--version`'s, "" when it couldn't be read.
	Version string
	// Latest is the newest release for this install's channel; "" when it
	// couldn't be looked up (offline): then no update is offered.
	Latest string
	// UpdateAvailable: Latest is newer than Version.
	UpdateAvailable bool
	// CanUpdate: Update knows how to update this install (Method isn't
	// Unknown). Show an Update button when both are true.
	CanUpdate bool
	// Method is how it was installed; when not installed, how Install will
	// install it (Native for Claude Code, GitHub for Codex).
	Method Method
	// Error is a plain-words problem reading the install (never a failed
	// latest-version lookup).
	Error string
}

// Error is an install or update failure: Message in plain words, Tail the
// last lines of the installer's output.
type Error struct {
	Message string
	Tail    []string
	Err     error
}

func (e *Error) Error() string {
	s := e.Message
	if len(e.Tail) > 0 {
		s += "\n\n" + strings.Join(e.Tail, "\n")
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

// Env is the machine: its OS, its native architecture (an Intel build
// under Rosetta or emulated x64 on Windows ARM still gets arm64), and the
// user's folders.
type Env struct {
	GOOS, GOARCH string
	Home         string
	AppData      string // %AppData%
	LocalAppData string // %LocalAppData%
	// Shell is the user's login shell (macOS, Linux).
	Shell string
	// PowerShell is Windows PowerShell's path.
	PowerShell string
}

func (e Env) search() launch.SearchEnv {
	return launch.SearchEnv{GOOS: e.GOOS, Home: e.Home, AppData: e.AppData, LocalAppData: e.LocalAppData}
}

func (e Env) windows() bool { return e.GOOS == "windows" }

// Sources are the official URLs (tests point them at fakes). All must be
// https.
type Sources struct {
	ClaudeReleases   string // Anthropic's release feed: <base>/latest, <base>/stable
	ClaudeInstallSh  string
	ClaudeInstallPS1 string
	NPMRegistry      string
	GitHubAPI        string
	CodexRepo        string // owner/name
	CodexInstallSh   string // OpenAI's installer, for installs it made
	CodexInstallPS1  string
}

// Official are the real sources.
var Official = Sources{
	ClaudeReleases:   "https://downloads.claude.ai/claude-code-releases",
	ClaudeInstallSh:  "https://claude.ai/install.sh",
	ClaudeInstallPS1: "https://claude.ai/install.ps1",
	NPMRegistry:      "https://registry.npmjs.org",
	GitHubAPI:        "https://api.github.com",
	CodexRepo:        "openai/codex",
	CodexInstallSh:   "https://chatgpt.com/codex/install.sh",
	CodexInstallPS1:  "https://chatgpt.com/codex/install.ps1",
}

// Cmd is a process to run.
type Cmd struct {
	Path string
	Args []string
	// Env are extra KEY=value entries (a PATH here replaces the inherited one).
	Env []string
}

// Timeouts.
const (
	installTimeout = 20 * time.Minute
	lookupTimeout  = 15 * time.Second
	versionTimeout = 20 * time.Second
	latestTTL      = 6 * time.Hour
	failedTTL      = 10 * time.Minute
)

// Manager does the work; New gives one for this machine, tests fill in the
// seams.
type Manager struct {
	Env     Env
	Sources Sources
	HTTP    *http.Client
	// Find finds a tool (launch.Find).
	Find func(Tool) (launch.Found, bool)
	// Run runs a process, sending each output line to progress, and returns
	// the last lines of its output.
	Run func(ctx context.Context, c Cmd, progress func(string)) ([]string, error)
	// Version runs `<path> --version` and returns the version in it.
	Version func(ctx context.Context, path string) (string, error)
	// Exists reports whether a file or folder exists.
	Exists func(string) bool
	// Link points link at the folder target: a symlink, a junction on Windows.
	Link func(target, link string) error
	// Registry is the user's PATH in the registry (Windows; nil elsewhere).
	Registry PathRegistry
	// FreshPath is PATH as a new terminal gets it.
	FreshPath func() string
	Now       func() time.Time

	mu    sync.Mutex
	cache map[string]cached
	busy  map[Tool]bool
}

type cached struct {
	version string
	at      time.Time
	ok      bool
}

var std = New()

// Status reports the tool's install and whether an update is available.
func Status(ctx context.Context, t Tool) Info { return std.Status(ctx, t) }

// Install installs the tool for this user (or updates it when it is
// already installed). Output lines go to progress.
func Install(ctx context.Context, t Tool, progress func(line string)) error {
	return std.Install(ctx, t, progress)
}

// Update updates the tool the way it was installed.
func Update(ctx context.Context, t Tool, progress func(line string)) error {
	return std.Update(ctx, t, progress)
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Status reports the tool's install and whether an update is available.
// It never fails: problems go in Info.Error, an unreachable release feed
// leaves Latest empty.
func (m *Manager) Status(ctx context.Context, t Tool) Info {
	info := Info{Tool: t}
	f, ok := m.Find(t)
	if !ok {
		info.Method = m.defaultMethod(t)
		info.Latest = m.latest(ctx, t, install{method: info.Method})
		return info
	}
	info.Installed = true
	info.Path = f.Path
	if f.Path == "" {
		info.Method = Unknown
		info.Error = t.DisplayName() + " was found as a shell alias or function, so code-link can't tell its version or update it."
		return info
	}
	in := m.detect(t, f.Path)
	info.Method = in.method
	info.CanUpdate = in.method != Unknown
	v, err := m.Version(ctx, f.Path)
	if err != nil {
		info.Error = fmt.Sprintf("Couldn't read the version of %s at %s: %v", t.DisplayName(), f.Path, err)
	}
	info.Version = v
	info.Latest = m.latest(ctx, t, in)
	info.UpdateAvailable = info.Version != "" && info.Latest != "" && CompareVersions(info.Latest, info.Version) > 0
	return info
}

func (m *Manager) defaultMethod(t Tool) Method {
	if t == Codex {
		return GitHub
	}
	return Native
}

// Install installs the tool, or updates it when it is already installed.
func (m *Manager) Install(ctx context.Context, t Tool, progress func(string)) error {
	if _, ok := m.Find(t); ok {
		return m.Update(ctx, t, progress)
	}
	return m.exclusive(ctx, t, "Installing", progress, func(ctx context.Context, progress func(string)) error {
		if t == Codex {
			return m.installCodex(ctx, progress)
		}
		return m.installClaude(ctx, progress)
	})
}

// Update updates the tool the way it was installed.
func (m *Manager) Update(ctx context.Context, t Tool, progress func(string)) error {
	f, ok := m.Find(t)
	if !ok {
		return &Error{Message: t.DisplayName() + " isn't installed."}
	}
	if f.Path == "" {
		return &Error{Message: t.DisplayName() + " was found as a shell alias or function; update it the way you installed it."}
	}
	in := m.detect(t, f.Path)
	return m.exclusive(ctx, t, "Updating", progress, func(ctx context.Context, progress func(string)) error {
		if t == Codex {
			return m.updateCodex(ctx, in, progress)
		}
		return m.updateClaude(ctx, in, progress)
	})
}

// exclusive runs one install or update of t at a time, with the install
// timeout, and turns cancellation into a plain error.
func (m *Manager) exclusive(ctx context.Context, t Tool, verb string, progress func(string), work func(context.Context, func(string)) error) error {
	if progress == nil {
		progress = func(string) {}
	}
	m.mu.Lock()
	if m.busy == nil {
		m.busy = map[Tool]bool{}
	}
	if m.busy[t] {
		m.mu.Unlock()
		return &Error{Message: t.DisplayName() + " is already being installed or updated."}
	}
	m.busy[t] = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.busy, t)
		m.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(ctx, installTimeout)
	defer cancel()
	err := work(ctx, progress)
	if err == nil {
		return nil
	}
	if ce := ctx.Err(); ce != nil {
		var tail []string
		var te *Error
		if errors.As(err, &te) {
			tail = te.Tail
		}
		if errors.Is(ce, context.DeadlineExceeded) {
			return &Error{Message: fmt.Sprintf("%s %s took too long and was stopped.", verb, t.DisplayName()), Tail: tail, Err: ce}
		}
		return &Error{Message: fmt.Sprintf("%s %s was cancelled.", verb, t.DisplayName()), Tail: tail, Err: ce}
	}
	return err
}

// New is a Manager for this machine.
func New() *Manager {
	home, _ := os.UserHomeDir()
	env := Env{GOOS: runtime.GOOS, GOARCH: nativeArch(), Home: home,
		AppData: os.Getenv("APPDATA"), LocalAppData: os.Getenv("LOCALAPPDATA"),
		Shell: launch.LoginShell(os.Getenv, exists), PowerShell: powerShell()}
	return &Manager{Env: env, Sources: Official, HTTP: newHTTPClient(),
		Find: launch.Find, Run: runCmd, Version: readVersion, Exists: exists,
		Link: linkDir, Registry: userPathRegistry(), FreshPath: launch.FreshPath}
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}
