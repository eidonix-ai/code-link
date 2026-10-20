package tools

import (
	"context"
	"fmt"

	"codelink/internal/launch"
)

// installClaude runs Anthropic's native installer, which installs for this
// user only (~/.local/bin/claude, %UserProfile%\.local\bin\claude.exe):
// on macOS `curl -fsSL https://claude.ai/install.sh | bash` in the login
// shell, on Windows `irm https://claude.ai/install.ps1 | iex` in Windows
// PowerShell. Both verify the binary's SHA-256 against Anthropic's release
// manifest before running it.
func (m *Manager) installClaude(ctx context.Context, progress func(string)) error {
	progress("Running Anthropic's Claude Code installer…")
	var c Cmd
	if m.Env.windows() {
		c = m.powerShellCmd("$ErrorActionPreference = 'Stop'; irm " + m.Sources.ClaudeInstallPS1 + " | iex")
	} else {
		c = m.shellCmd("set -o pipefail; curl -fsSL " + m.Sources.ClaudeInstallSh + " | bash")
	}
	tail, err := m.Run(ctx, c, progress)
	if err != nil {
		return failed("The Claude Code installer failed", tail, err)
	}
	return m.confirm(ctx, ClaudeCode, "The Claude Code installer finished, but claude can't be found", tail, progress)
}

// updateClaude updates the way it was installed: `claude update` for the
// native installer, npm, Homebrew or WinGet for theirs.
func (m *Manager) updateClaude(ctx context.Context, in install, progress func(string)) error {
	switch in.method {
	case Native:
		progress("Running claude update…")
		tail, err := m.Run(ctx, Cmd{Path: in.path, Args: []string{"update"}}, progress)
		if err != nil {
			return failed("`claude update` failed", tail, err)
		}
	case NPM, Brew, WinGet:
		if err := m.updateWithPackageManager(ctx, ClaudeCode, in, progress); err != nil {
			return err
		}
	default:
		return cantUpdate(ClaudeCode, in.path)
	}
	return m.confirm(ctx, ClaudeCode, "", nil, progress)
}

func cantUpdate(t Tool, path string) error {
	return &Error{Message: fmt.Sprintf("code-link doesn't know how %s at %s was installed, so it can't update it. Update it the way you installed it.", t.DisplayName(), path)}
}

// updateWithPackageManager runs `npm install -g <pkg>@latest` (not
// `npm update -g`, which keeps to the original semver range),
// `brew upgrade <name>` or `winget upgrade --id <id>`.
func (m *Manager) updateWithPackageManager(ctx context.Context, t Tool, in install, progress func(string)) error {
	var c Cmd
	var what string
	switch in.method {
	case NPM:
		what = fmt.Sprintf("Updating %s with npm failed", t.DisplayName())
		script := "npm install -g " + npmPackage(t) + "@latest"
		dir := m.Env.dir(in.path) // npm sits next to the tool's shim
		if m.Env.windows() {
			c = m.powerShellCmd(script)
			c.Env = []string{"Path=" + dir + ";" + m.freshPath()}
		} else {
			c = m.shellCmd("export PATH=" + launch.Quote(dir) + `:"$PATH"; ` + script)
		}
	case Brew:
		what = fmt.Sprintf("Homebrew couldn't update %s", in.brewName)
		c = Cmd{Path: in.brew, Args: []string{"upgrade", in.brewName},
			Env: []string{"HOMEBREW_NO_ENV_HINTS=1", "NONINTERACTIVE=1"}}
	case WinGet:
		what = fmt.Sprintf("WinGet couldn't update %s", t.DisplayName())
		winget := m.Env.join(m.Env.LocalAppData, "Microsoft", "WindowsApps", "winget.exe")
		if !m.Exists(winget) {
			winget = "winget.exe"
		}
		c = Cmd{Path: winget, Args: []string{"upgrade", "--id", wingetID(t), "--exact", "--silent",
			"--accept-source-agreements", "--accept-package-agreements", "--disable-interactivity"},
			Env: []string{"Path=" + m.freshPath()}}
	}
	progress(fmt.Sprintf("Updating %s with %s…", t.DisplayName(), in.method))
	tail, err := m.Run(ctx, c, progress)
	if err != nil {
		return failed(what, tail, err)
	}
	return nil
}

// confirm finds the tool after an install or update and reports its
// version; notFound is the error when it isn't found ("" = don't check).
func (m *Manager) confirm(ctx context.Context, t Tool, notFound string, tail []string, progress func(string)) error {
	f, ok := m.Find(t)
	if !ok {
		if notFound == "" {
			return nil
		}
		return &Error{Message: notFound + ".", Tail: tail}
	}
	if f.Path != "" {
		if v, err := m.Version(ctx, f.Path); err == nil {
			progress(fmt.Sprintf("%s %s is installed.", t.DisplayName(), v))
		}
	}
	return nil
}

// shellCmd runs a script in the user's login shell (so its PATH, nvm and
// Homebrew setup apply).
func (m *Manager) shellCmd(script string) Cmd {
	return Cmd{Path: m.Env.Shell, Args: []string{"-lc", script}}
}

// powerShellCmd runs a script in Windows PowerShell, without the user's
// profile, with scripts allowed for this process only, and failing instead
// of waiting at a prompt nobody sees.
func (m *Manager) powerShellCmd(script string) Cmd {
	return Cmd{Path: m.Env.PowerShell, Args: []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script}}
}

func (m *Manager) freshPath() string {
	if m.FreshPath != nil {
		return m.FreshPath()
	}
	return ""
}
