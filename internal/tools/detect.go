package tools

import (
	"path/filepath"
	"runtime"
	"strings"

	"codelink/internal/launch"
)

// install is a found install: how it was made and what updating it needs.
type install struct {
	method   Method
	path     string // the command found
	brewName string // the cask or formula (Brew)
	brew     string // the brew command (Brew)
	channel  string // Claude Code's Homebrew cask channel: "stable" or "latest"
}

// npmPackage is the tool's npm package.
func npmPackage(t Tool) string {
	if t == Codex {
		return "@openai/codex"
	}
	return "@anthropic-ai/claude-code"
}

// wingetID is the tool's WinGet package.
func wingetID(t Tool) string {
	if t == Codex {
		return "OpenAI.Codex"
	}
	return "Anthropic.ClaudeCode"
}

// norm is p for comparing: forward slashes, lower case on Windows.
func (e Env) norm(p string) string {
	if e.windows() {
		return strings.ToLower(strings.ReplaceAll(p, `\`, "/"))
	}
	return p
}

// under: p is dir or inside it.
func (e Env) under(p, dir string) bool {
	if dir == "" || p == "" {
		return false
	}
	p, dir = strings.TrimRight(e.norm(p), "/"), strings.TrimRight(e.norm(dir), "/")
	return p == dir || strings.HasPrefix(p, dir+"/")
}

func (e Env) join(parts ...string) string {
	if e.windows() {
		return strings.Join(parts, `\`)
	}
	return filepath.Join(parts...)
}

func (e Env) dir(p string) string {
	if e.windows() {
		if i := strings.LastIndexAny(p, `\/`); i >= 0 {
			return p[:i]
		}
		return ""
	}
	return filepath.Dir(p)
}

// resolve follows symlinks (on this machine only; tests with another GOOS
// see paths as given).
func (m *Manager) resolve(p string) string {
	if m.Env.GOOS != runtime.GOOS {
		return p
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// codexDir is where code-link installs Codex.
func (m *Manager) codexDir() string { return launch.CodexDir(m.Env.search()) }

// detect works out how the install at path was made, from where it lives
// (after following symlinks).
func (m *Manager) detect(t Tool, path string) install {
	e := m.Env
	in := install{method: Unknown, path: path}
	resolved := m.resolve(path)
	both := func(dir string) bool {
		return e.under(path, dir) || e.under(resolved, dir) || e.under(resolved, m.resolve(dir))
	}
	rn := e.norm(resolved)
	shim := strings.HasSuffix(e.norm(path), ".cmd") || strings.HasSuffix(e.norm(path), ".ps1")

	switch {
	case t == Codex && both(m.codexDir()):
		in.method = GitHub
	case t == Codex && both(e.join(e.Home, ".codex", "packages")):
		in.method = Standalone
	case t == Codex && e.windows() && both(e.join(e.LocalAppData, "Programs", "OpenAI", "Codex")):
		in.method = Standalone
	case t == ClaudeCode && both(e.join(e.Home, ".claude", "local")):
		// The old local installer's npm copy; `claude update` handles it.
		in.method = Native
	case strings.Contains(rn, "/node_modules/"+npmPackage(t)+"/"):
		in.method = NPM
	case e.windows() && shim && m.Exists(e.join(e.dir(path), "node_modules", strings.ReplaceAll(npmPackage(t), "/", `\`))):
		in.method = NPM
	case !e.windows() && (strings.Contains(rn, "/Caskroom/") || strings.Contains(rn, "/Cellar/")):
		in.method = Brew
		in.brew, in.brewName = brewOf(resolved)
		in.channel = "stable"
		if strings.HasSuffix(in.brewName, "@latest") {
			in.channel = "latest"
		}
	case e.windows() && both(e.join(e.LocalAppData, "Microsoft", "WinGet")):
		in.method = WinGet
	case t == ClaudeCode && (both(e.join(e.Home, ".local", "share", "claude")) ||
		e.norm(path) == e.norm(e.join(e.Home, ".local", "bin", "claude")) ||
		e.norm(path) == e.norm(e.join(e.Home, ".local", "bin", "claude.exe"))):
		in.method = Native
	}
	return in
}

// brewOf is the brew command and package name of a Homebrew install:
// /opt/homebrew/Caskroom/claude-code/2.1.3/claude → /opt/homebrew/bin/brew, claude-code.
func brewOf(resolved string) (brew, name string) {
	for _, kind := range []string{"/Caskroom/", "/Cellar/"} {
		if i := strings.Index(resolved, kind); i >= 0 {
			name, _, _ = strings.Cut(resolved[i+len(kind):], "/")
			return resolved[:i] + "/bin/brew", name
		}
	}
	return "brew", ""
}
