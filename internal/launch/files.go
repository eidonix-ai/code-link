package launch

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// ClaudeSettings is the --settings file given to Claude Code: commit and
// pull-request attribution off. Passed by path (inline JSON loses its quotes
// on the way through Windows PowerShell).
const ClaudeSettings = `{"attribution":{"commit":"","pr":""}}` + "\n"

// ClaudeSettingsFile is the name of that file, next to code-link.json.
const ClaudeSettingsFile = "claude-settings.json"

// EnsureClaudeSettings writes dir/claude-settings.json (only when its
// content differs) and returns its path.
func EnsureClaudeSettings(dir string) (string, error) {
	p := filepath.Join(dir, ClaudeSettingsFile)
	if b, err := os.ReadFile(p); err == nil && string(b) == ClaudeSettings {
		return p, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, []byte(ClaudeSettings), 0o600); err != nil {
		return "", err
	}
	return p, nil
}

// LaunchDir is where launch scripts are written: the user's cache folder
// (~/Library/Caches/Eidonix/code-link/launch, %LocalAppData%\Eidonix\code-link\launch).
func LaunchDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "Eidonix", "code-link", "launch"), nil
}

// WriteScript writes content to dir/launch-<random><ext> (0700) and returns
// its path. The scripts delete themselves when they run.
func WriteScript(dir, ext string, content []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	p := filepath.Join(dir, "launch-"+hex.EncodeToString(b)+ext)
	if err := os.WriteFile(p, content, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(p, 0o700); err != nil {
		os.Remove(p)
		return "", err
	}
	return p, nil
}

// POSIXScript is the script handed to iTerm or Terminal. It runs in the
// user's interactive login shell (so ~/.zprofile and ~/.zshrc PATH entries
// apply — iTerm runs a session command without a shell), deletes itself,
// cds and runs the command. When the tool exits the window stays open in an
// interactive shell with the same environment (s.After says so), with
// s.Function defined there for zsh and bash. pathDir, when set, is put in
// front of PATH (a CLI found outside the login shell's PATH).
func POSIXScript(shell string, s Session, folder, pathDir string) string {
	var b strings.Builder
	b.WriteString("#!" + shell + " -li\n")
	b.WriteString(`rm -f -- "$0"` + "\n")
	if folder != "" {
		b.WriteString("cd -- " + Quote(folder) + " || exit 1\n")
	}
	if pathDir != "" {
		b.WriteString("export PATH=" + Quote(pathDir) + `:"$PATH"` + "\n")
	}
	b.WriteString(s.Command + "\n")
	// Not "status": zsh has a read-only parameter of that name.
	b.WriteString("code=$?\n")
	b.WriteString(`if [ "$code" -ne 0 ]; then printf '\n[exited with status %s]\n' "$code"; fi` + "\n")
	if s.After != "" {
		b.WriteString("printf '\\n%s\\n\\n' " + Quote(s.After) + "\n")
	}
	switch filepath.Base(shell) {
	case "zsh":
		// zsh reads its startup files from $ZDOTDIR: a one-off folder whose
		// files read the user's own, then add the function.
		b.WriteString(`rc=$(mktemp -d "${TMPDIR:-/tmp}/code-link-shell.XXXXXX") || exec ` + Quote(shell) + " -i\n")
		b.WriteString(`printf '%s\n' '[ -f "$HOME/.zshenv" ] && . "$HOME/.zshenv"' > "$rc/.zshenv"` + "\n")
		b.WriteString(`printf '%s\n' 'ZDOTDIR="$HOME"; [ -f "$HOME/.zshrc" ] && . "$HOME/.zshrc"' ` + Quote(s.Function) + ` "rm -rf -- '$rc'" > "$rc/.zshrc"` + "\n")
		b.WriteString(`ZDOTDIR="$rc" exec ` + Quote(shell) + " -i\n")
	case "bash":
		b.WriteString(`rc=$(mktemp "${TMPDIR:-/tmp}/code-link-shell.XXXXXX") || exec ` + Quote(shell) + " -i\n")
		b.WriteString(`printf '%s\n' '[ -f "$HOME/.bashrc" ] && . "$HOME/.bashrc"' ` + Quote(s.Function) + ` "rm -f -- '$rc'" > "$rc"` + "\n")
		b.WriteString("exec " + Quote(shell) + ` --rcfile "$rc" -i` + "\n")
	default:
		b.WriteString("exec " + Quote(shell) + " -i\n")
	}
	return b.String()
}

// LoginShell is the user's login shell when it is a POSIX shell the script
// can run in, else zsh.
func LoginShell(env func(string) string, exists func(string) bool) string {
	shell := env("SHELL")
	switch filepath.Base(shell) {
	case "zsh", "bash", "ksh", "sh":
		if exists(shell) {
			return shell
		}
	}
	return "/bin/zsh"
}

// PowerShellScript is the .ps1 file Windows Terminal or PowerShell runs
// (with -NoExit, so the window stays, keeping the $env: settings). It
// deletes itself, changes to the folder, puts pathDir (the folder the CLI
// was found in) first on PATH and runs the command; then says how to resume
// and defines s.Function for the rest of the session. UTF-8 with a
// byte-order mark: Windows PowerShell 5.1 reads a .ps1 without one in the
// ANSI code page.
func PowerShellScript(s Session, folder, pathDir string) []byte {
	var b bytes.Buffer
	b.WriteString("\ufeff")
	b.WriteString("Remove-Item -LiteralPath $PSCommandPath -Force -ErrorAction SilentlyContinue\r\n")
	if folder != "" {
		b.WriteString("Set-Location -LiteralPath " + PSQuote(folder) + "\r\n")
	}
	if pathDir != "" {
		b.WriteString("$env:Path = " + PSQuote(pathDir+";") + " + $env:Path\r\n")
	}
	b.WriteString(strings.ReplaceAll(s.Command, "\n", "\r\n") + "\r\n")
	if s.After != "" {
		b.WriteString("Write-Host ''\r\nWrite-Host " + PSQuote(s.After) + "\r\nWrite-Host ''\r\n")
	}
	if s.Function != "" {
		b.WriteString(s.Function + "\r\n")
	}
	return b.Bytes()
}
