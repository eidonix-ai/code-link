package launch

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"codelink/internal/config"
)

func ip(n int) *int { return &n }

// cfg mirrors the Swift tests' config(main:opus:small:codex:window:compact:).
func cfg(main, opus, small, codex string, window, compact *int) *config.Config {
	c := config.Default()
	c.Backends = []config.Backend{{ID: "cn3", Kind: config.OpenAICompatible, BaseURL: "http://h:8000/v1"}}
	c.Models = []config.Model{{ID: main, BackendID: "cn3", Upstream: main, ContextWindow: window, AutoCompactAt: compact}}
	c.Roles = config.Roles{Main: main, Opus: opus, Small: small, Codex: codex}
	for _, id := range []string{opus, small, codex} {
		if _, ok := c.Model(id); id != "" && !ok {
			c.Models = append(c.Models, config.Model{ID: id, BackendID: "cn3", Upstream: id, ContextWindow: ip(262_144)})
		}
	}
	return &c
}

func def() *config.Config { return cfg("GLM-5.3", "", "", "", ip(1_048_576), nil) }

const macSettings = "/Users/d/Library/Application Support/Eidonix/code-link/claude-settings.json"

func ctx(c *config.Config, mode string) Context {
	return Context{Config: c, Port: 18765, Token: "tok_en-1", ClaudeSettingsPath: macSettings, PermissionMode: mode}
}

func render(t *testing.T, tool Tool, c Context, sh Shell) string {
	t.Helper()
	s, err := Render(tool, c, sh)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestClaudeCommandMatchesDesign(t *testing.T) {
	got := render(t, ClaudeCode, ctx(cfg("GLM-5.3", "Big", "Tiny", "", ip(1_048_576), nil), "auto"), POSIX)
	want := `export ANTHROPIC_BASE_URL=http://127.0.0.1:18765
export ANTHROPIC_AUTH_TOKEN=tok_en-1
export ANTHROPIC_MODEL=GLM-5.3
export ANTHROPIC_DEFAULT_SONNET_MODEL=GLM-5.3
export ANTHROPIC_DEFAULT_SONNET_MODEL_NAME=GLM-5.3
export ANTHROPIC_DEFAULT_OPUS_MODEL=Big
export ANTHROPIC_DEFAULT_OPUS_MODEL_NAME=Big
export ANTHROPIC_DEFAULT_FABLE_MODEL=Big
export ANTHROPIC_DEFAULT_FABLE_MODEL_NAME=Big
export ANTHROPIC_DEFAULT_HAIKU_MODEL=Tiny
export ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME=Tiny
export ANTHROPIC_SMALL_FAST_MODEL=Tiny
export CLAUDE_CODE_AUTO_MODE_SERVER=0
export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
export API_TIMEOUT_MS=900000
export CLAUDE_CODE_MAX_CONTEXT_TOKENS=1048576
export CLAUDE_CODE_AUTO_COMPACT_WINDOW=891289
claude --permission-mode auto --settings '/Users/d/Library/Application Support/Eidonix/code-link/claude-settings.json'`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestClaudeAliasesDefaultToMainAndNeverSetClaudeOnlyFlags(t *testing.T) {
	for _, sh := range []Shell{POSIX, PowerShell} {
		cmd := render(t, ClaudeCode, ctx(def(), ""), sh)
		for _, name := range []string{"ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_SMALL_FAST_MODEL",
			"ANTHROPIC_DEFAULT_OPUS_MODEL_NAME", "ANTHROPIC_DEFAULT_FABLE_MODEL_NAME", "ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME"} {
			want := "export " + name + "=GLM-5.3\n"
			if sh == PowerShell {
				want = "$env:" + name + " = 'GLM-5.3'"
			}
			if !strings.Contains(cmd, want) {
				t.Errorf("%v: missing %q in\n%s", sh, want, cmd)
			}
		}
		for _, unwanted := range []string{"DISABLE_PROMPT_CACHING", "DISABLE_1M_CONTEXT"} {
			if strings.Contains(cmd, unwanted) {
				t.Errorf("%s only applies to Claude models and makes Claude Code warn", unwanted)
			}
		}
		if !strings.Contains(cmd, "--permission-mode auto ") {
			t.Errorf("an empty permission mode is auto:\n%s", cmd)
		}
	}
}

func TestClaudeOmitsWindowVariablesWhenUnknownAndHonoursAutoCompactAt(t *testing.T) {
	unknown := render(t, ClaudeCode, ctx(cfg("GLM-5.3", "", "", "", nil, nil), "auto"), POSIX)
	if strings.Contains(unknown, "CLAUDE_CODE_MAX_CONTEXT_TOKENS") || strings.Contains(unknown, "CLAUDE_CODE_AUTO_COMPACT_WINDOW") {
		t.Errorf("unknown window must not be passed:\n%s", unknown)
	}
	explicit := render(t, ClaudeCode, ctx(cfg("GLM-5.3", "", "", "", ip(200_000), ip(150_000)), "auto"), POSIX)
	if !strings.Contains(explicit, "CLAUDE_CODE_MAX_CONTEXT_TOKENS=200000\n") || !strings.Contains(explicit, "CLAUDE_CODE_AUTO_COMPACT_WINDOW=150000\n") {
		t.Errorf("explicit window and compact point:\n%s", explicit)
	}
}

func TestClaudeUsesPermissionModeAndQuotesAwkwardModelNames(t *testing.T) {
	cmd := render(t, ClaudeCode, ctx(cfg("my model's [1m]", "", "", "", ip(1_048_576), nil), "acceptEdits"), POSIX)
	if !strings.Contains(cmd, `export ANTHROPIC_MODEL='my model'"'"'s [1m]'`+"\n") {
		t.Errorf("model not quoted:\n%s", cmd)
	}
	if !strings.HasSuffix(cmd, "claude --permission-mode acceptEdits --settings '"+macSettings+"'") {
		t.Errorf("tail:\n%s", cmd)
	}
}

func TestClaudePowerShellCommand(t *testing.T) {
	c := ctx(cfg("model'o", "", "", "", nil, nil), "plan")
	c.ClaudeSettingsPath = `C:\Users\A B\AppData\Roaming\Eidonix\code-link\claude-settings.json`
	cmd := render(t, ClaudeCode, c, PowerShell)
	lines := strings.Split(cmd, "\n")
	if lines[0] != "$env:ANTHROPIC_BASE_URL = 'http://127.0.0.1:18765'" || lines[1] != "$env:ANTHROPIC_AUTH_TOKEN = 'tok_en-1'" {
		t.Errorf("head:\n%s", cmd)
	}
	for _, want := range []string{"$env:ANTHROPIC_MODEL = 'model''o'", "$env:ANTHROPIC_SMALL_FAST_MODEL = 'model''o'",
		"$env:CLAUDE_CODE_AUTO_MODE_SERVER = '0'", "$env:CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC = '1'", "$env:API_TIMEOUT_MS = '900000'"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("missing %q:\n%s", want, cmd)
		}
	}
	if want := `claude --permission-mode plan --settings 'C:\Users\A B\AppData\Roaming\Eidonix\code-link\claude-settings.json'`; lines[len(lines)-1] != want {
		t.Errorf("last line %q, want %q", lines[len(lines)-1], want)
	}
	for _, l := range lines[:len(lines)-1] {
		if !strings.HasPrefix(l, "$env:") {
			t.Errorf("not an assignment: %q", l)
		}
	}
}

func TestCommandsNeedAMainModel(t *testing.T) {
	empty := config.Default()
	for _, tool := range Tools {
		for _, sh := range []Shell{POSIX, PowerShell} {
			if _, err := Render(tool, ctx(&empty, ""), sh); !errors.Is(err, ErrNoMainModel) {
				t.Errorf("%s: err = %v", tool, err)
			}
		}
	}
}

func TestCodexCommandMatchesDesign(t *testing.T) {
	got := render(t, Codex, ctx(cfg("GLM-5.3", "", "", "Coder", ip(1_048_576), nil), "auto"), POSIX)
	want := `export CODE_LINK_TOKEN=tok_en-1
codex --approve-for-me \
  -c 'model="Coder"' \
  -c 'model_provider="code-link"' \
  -c 'model_context_window=262144' \
  -c 'model_providers.code-link.name="code-link"' \
  -c 'model_providers.code-link.base_url="http://127.0.0.1:18765/v1"' \
  -c 'model_providers.code-link.wire_api="responses"' \
  -c 'model_providers.code-link.env_key="CODE_LINK_TOKEN"'`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestCodexPowerShellCommand(t *testing.T) {
	got := render(t, Codex, ctx(cfg("model'o", "", "", "", ip(128_000), nil), "auto"), PowerShell)
	want := "$env:CODE_LINK_TOKEN = 'tok_en-1'\n" +
		"codex --approve-for-me `\n" +
		"  -c 'model=\"model''o\"' `\n" +
		"  -c 'model_provider=\"code-link\"' `\n" +
		"  -c 'model_context_window=128000' `\n" +
		"  -c 'model_providers.code-link.name=\"code-link\"' `\n" +
		"  -c 'model_providers.code-link.base_url=\"http://127.0.0.1:18765/v1\"' `\n" +
		"  -c 'model_providers.code-link.wire_api=\"responses\"' `\n" +
		"  -c 'model_providers.code-link.env_key=\"CODE_LINK_TOKEN\"'"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestCodexDefaultsToMainAndSkipsUnknownWindow(t *testing.T) {
	cmd := render(t, Codex, ctx(cfg("GLM-5.3", "", "", "", nil, nil), "auto"), POSIX)
	if !strings.Contains(cmd, `-c 'model="GLM-5.3"'`) || strings.Contains(cmd, "model_context_window") {
		t.Errorf("got:\n%s", cmd)
	}
}

func TestBuildGivesTheEnvironmentAndArguments(t *testing.T) {
	inv, err := Build(Codex, ctx(def(), ""))
	if err != nil {
		t.Fatal(err)
	}
	if inv.Name != "codex" || len(inv.Env) != 1 || inv.Env[0] != (Var{"CODE_LINK_TOKEN", "tok_en-1"}) || inv.Args[0] != "--approve-for-me" || inv.Args[1] != "-c" {
		t.Errorf("%+v", inv)
	}
	inv, _ = Build(ClaudeCode, ctx(def(), "plan"))
	if strings.Join(inv.Args, " ") != "--permission-mode plan --settings "+macSettings {
		t.Errorf("%v", inv.Args)
	}
}

func TestShellFor(t *testing.T) {
	if ShellFor("windows") != PowerShell || ShellFor("darwin") != POSIX || PowerShell.Language() != "powershell" || POSIX.Language() != "bash" {
		t.Error("shells")
	}
	want := POSIX
	if runtime.GOOS == "windows" {
		want = PowerShell
	}
	if NativeShell() != want {
		t.Error("native shell")
	}
}

func TestShellQuoting(t *testing.T) {
	cases := [][2]string{
		{Quote("it's"), `'it'"'"'s'`},
		{QuoteIfNeeded("GLM-5.3"), "GLM-5.3"},
		{QuoteIfNeeded("a b"), "'a b'"},
		{QuoteIfNeeded(""), "''"},
		{TOMLString(`a"b\c`), `"a\"b\\c"`},
		{PSQuote("it's"), "'it''s'"},
		{psQuoteIfNeeded("acceptEdits"), "acceptEdits"},
		{psQuoteIfNeeded("a b"), "'a b'"},
	}
	for _, c := range cases {
		if c[0] != c[1] {
			t.Errorf("got %s want %s", c[0], c[1])
		}
	}
}

func TestLaunchScriptText(t *testing.T) {
	got := POSIXScript("/bin/zsh", Session{Command: "claude --version", After: "Claude Code has ended."}, "/Users/d/my project", "")
	want := `#!/bin/zsh -li
rm -f -- "$0"
cd -- '/Users/d/my project' || exit 1
claude --version
code=$?
if [ "$code" -ne 0 ]; then printf '\n[exited with status %s]\n' "$code"; fi
printf '\n%s\n\n' 'Claude Code has ended.'
rc=$(mktemp -d "${TMPDIR:-/tmp}/code-link-shell.XXXXXX") || exec '/bin/zsh' -i
printf '%s\n' '[ -f "$HOME/.zshenv" ] && . "$HOME/.zshenv"' > "$rc/.zshenv"
printf '%s\n' 'ZDOTDIR="$HOME"; [ -f "$HOME/.zshrc" ] && . "$HOME/.zshrc"' '' "rm -rf -- '$rc'" > "$rc/.zshrc"
ZDOTDIR="$rc" exec '/bin/zsh' -i
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(POSIXScript("/bin/zsh", Session{Command: "x"}, "", ""), "cd --") {
		t.Error("no folder, no cd")
	}
	if s := POSIXScript("/bin/zsh", Session{Command: "x"}, "/a", "/Users/d/.local/bin"); !strings.Contains(s, "cd -- '/a' || exit 1\nexport PATH='/Users/d/.local/bin':\"$PATH\"\nx\n") {
		t.Errorf("PATH line:\n%s", s)
	}
	if s := POSIXScript("/bin/bash", Session{Command: "x", Function: "codex() { command codex -c 'a=1' \"$@\"; }"}, "", ""); !strings.Contains(s, `exec '/bin/bash' --rcfile "$rc" -i`) || !strings.Contains(s, `'codex() { command codex -c '"'"'a=1'"'"' "$@"; }'`) {
		t.Errorf("bash:\n%s", s)
	}
}

// The window is left in an interactive shell that keeps the session's
// environment and, for Codex, the function that keeps code-link's
// overrides. Runs the real script in a temporary home.
func TestLeftoverShellKeepsTheEnvironmentAndFunction(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shells")
	}
	for _, shell := range []string{"/bin/zsh", "/bin/bash"} {
		if !fileExists(shell) {
			continue
		}
		home := t.TempDir()
		os.WriteFile(filepath.Join(home, ".zshrc"), []byte("export FROM_RC=yes\n"), 0o600)
		os.WriteFile(filepath.Join(home, ".bashrc"), []byte("export FROM_RC=yes\n"), 0o600)
		s := Session{Command: "export CODE_LINK_TOKEN=tok\ntrue", After: "ended", Function: `codex() { echo "wrapped $*"; }`}
		file := filepath.Join(t.TempDir(), "launch.sh")
		os.WriteFile(file, []byte(POSIXScript(shell, s, "", "")), 0o700)
		cmd := exec.Command(shell, file) // as the terminal runs it, minus -li (no login profile here)
		cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "TMPDIR=" + t.TempDir()}
		cmd.Stdin = strings.NewReader("echo token=$CODE_LINK_TOKEN rc=$FROM_RC\ncodex resume --last\nexit\n")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", shell, err, out)
		}
		for _, w := range []string{"ended", "token=tok rc=yes", "wrapped resume --last"} {
			if !strings.Contains(string(out), w) {
				t.Errorf("%s: missing %q in:\n%s", shell, w, out)
			}
		}
	}
}

func TestPowerShellScriptText(t *testing.T) {
	got := string(PowerShellScript(Session{Command: "$env:A = 'x'\nclaude", After: "Claude Code has ended.", Function: "function global:codex { }"}, `C:\Users\A B\code`, `C:\Users\A B\.local\bin`))
	want := "\ufeffRemove-Item -LiteralPath $PSCommandPath -Force -ErrorAction SilentlyContinue\r\n" +
		"Set-Location -LiteralPath 'C:\\Users\\A B\\code'\r\n" +
		"$env:Path = 'C:\\Users\\A B\\.local\\bin;' + $env:Path\r\n" +
		"$env:A = 'x'\r\nclaude\r\n" +
		"Write-Host ''\r\nWrite-Host 'Claude Code has ended.'\r\nWrite-Host ''\r\n" +
		"function global:codex { }\r\n"
	if got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
	if s := string(PowerShellScript(Session{Command: "x"}, "", "")); strings.Contains(s, "Set-Location") || strings.Contains(s, "$env:Path") || strings.Contains(s, "Write-Host") {
		t.Errorf("no folder, no PATH, no note: %q", s)
	}
}

func TestSessionsSayHowToResume(t *testing.T) {
	c, _ := NewSession(ClaudeCode, ctx(def(), ""), POSIX)
	if !strings.Contains(c.After, "claude --continue") || c.Function != "" {
		t.Errorf("claude: %+v", c)
	}
	x, _ := NewSession(Codex, ctx(def(), ""), POSIX)
	if !strings.Contains(x.After, "codex resume --last") || !strings.HasPrefix(x.Function, "codex() { command codex --approve-for-me -c ") || !strings.Contains(x.Function, "model_provider=") {
		t.Errorf("codex: %+v", x)
	}
	w, _ := NewSession(Codex, ctx(def(), ""), PowerShell)
	if !strings.HasPrefix(w.Function, "function global:codex {") || !strings.Contains(w.Function, "@args }") {
		t.Errorf("powershell: %+v", w)
	}
}

func TestLoginShellFallsBackToZsh(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	yes := func(string) bool { return true }
	no := func(string) bool { return false }
	cases := []struct {
		env    map[string]string
		exists func(string) bool
		want   string
	}{
		{map[string]string{"SHELL": "/bin/bash"}, yes, "/bin/bash"},
		{map[string]string{"SHELL": "/opt/homebrew/bin/fish"}, yes, "/bin/zsh"},
		{map[string]string{"SHELL": "/bin/zsh"}, no, "/bin/zsh"},
		{map[string]string{}, yes, "/bin/zsh"},
	}
	for _, c := range cases {
		if got := LoginShell(env(c.env), c.exists); got != c.want {
			t.Errorf("%v: %s, want %s", c.env, got, c.want)
		}
	}
}

func TestITermIsUsedOnlyWhenInstalledAndThePathIsPlain(t *testing.T) {
	cases := []struct {
		pref      string
		installed bool
		path      string
		want      bool
	}{
		{TerminalAuto, true, "/Users/d/Library/Caches/Eidonix/code-link/launch/launch-1.sh", true},
		{TerminalAuto, false, "/a/b.sh", false},
		{TerminalApple, true, "/a/b.sh", false},
		{TerminalITerm, true, "/a/b.sh", true},
		{TerminalAuto, true, "/Users/d/My Mac/b.sh", false},
		{TerminalAuto, true, "/Users/d/it's/b.sh", false},
	}
	for _, c := range cases {
		if got := UseITerm(c.pref, c.installed, c.path); got != c.want {
			t.Errorf("%+v: %v", c, got)
		}
	}
}

func apps(ps []MacPlan) []MacApp {
	var out []MacApp
	for _, p := range ps {
		out = append(out, p.App)
	}
	return out
}

func eqApps(a []MacApp, b ...MacApp) bool {
	return strings.Join(strs(a), ",") == strings.Join(strs(b), ",")
}

func strs(a []MacApp) []string {
	var out []string
	for _, x := range a {
		out = append(out, map[MacApp]string{ITerm: "iterm", AppleTerminal: "terminal"}[x])
	}
	return out
}

func TestTerminalPlansPickTheScriptAndPathHandling(t *testing.T) {
	plain := "/Users/d/Library/Caches/Eidonix/code-link/launch/launch-ab.sh"
	auto := MacPlans(TerminalAuto, true, plain)
	if !eqApps(apps(auto), ITerm, AppleTerminal) {
		t.Fatalf("iTerm first, Terminal as the fallback: %v", apps(auto))
	}
	if auto[0].Argument != plain || auto[1].Argument != "'"+plain+"'" {
		t.Errorf("iTerm gets the bare path, Terminal the quoted one: %+v", auto)
	}
	if !strings.Contains(auto[0].AppleScript, `tell application "iTerm2"`) ||
		!strings.Contains(auto[1].AppleScript, `tell application "Terminal"`) || !strings.Contains(auto[1].AppleScript, "do script") {
		t.Error("scripts")
	}
	if !eqApps(apps(MacPlans(TerminalApple, true, plain)), AppleTerminal) ||
		!eqApps(apps(MacPlans(TerminalAuto, false, plain)), AppleTerminal) ||
		!eqApps(apps(MacPlans(TerminalITerm, false, plain)), AppleTerminal) {
		t.Error("Terminal only")
	}
	spaced := "/Users/d/My Mac/launch.sh"
	if f := MacPlans(TerminalITerm, true, spaced); !eqApps(apps(f), AppleTerminal) || f[0].Argument != "'"+spaced+"'" {
		t.Errorf("spaced: %+v", f)
	}
	if q := MacPlans(TerminalAuto, true, "/Users/o'brien/l.sh"); !eqApps(apps(q), AppleTerminal) || q[0].Argument != `'/Users/o'"'"'brien/l.sh'` {
		t.Errorf("quoted: %+v", q)
	}
}

func TestITermOpensATabInTheCurrentWindowElseANewWindow(t *testing.T) {
	for _, want := range []string{"if (count of windows) = 0 then", "create window with default profile command (item 1 of argv)",
		"tell current window to create tab with default profile command (item 1 of argv)"} {
		if !strings.Contains(ITermScript, want) {
			t.Errorf("missing %q", want)
		}
	}
	if !strings.Contains(TerminalScript, "do script (item 1 of argv)") {
		t.Error("Terminal script")
	}
}

func TestWindowsPlans(t *testing.T) {
	ps := `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`
	script := `C:\Users\A B\AppData\Local\Eidonix\code-link\launch\launch-1.ps1`
	plans := WinPlans(TerminalAuto, `C:\wt.exe`, ps, script, `C:\code\app`)
	if len(plans) != 2 || plans[0].Program != `C:\wt.exe` || plans[1].Program != ps {
		t.Fatalf("Windows Terminal first, PowerShell as the fallback: %+v", plans)
	}
	wantWT := []string{"-w", "0", "new-tab", "-d", `C:\code\app`, ps, "-NoLogo", "-NoExit", "-ExecutionPolicy", "Bypass", "-File", script}
	if strings.Join(plans[0].Args, "|") != strings.Join(wantWT, "|") || plans[0].NewConsole {
		t.Errorf("wt args: %q", plans[0].Args)
	}
	if strings.Join(plans[1].Args, "|") != "-NoLogo|-NoExit|-ExecutionPolicy|Bypass|-File|"+script || !plans[1].NewConsole || plans[1].Dir != `C:\code\app` {
		t.Errorf("powershell plan: %+v", plans[1])
	}
	if p := WinPlans(TerminalAuto, "", ps, script, `C:\x`); len(p) != 1 || p[0].Program != ps {
		t.Errorf("no wt: %+v", p)
	}
	if p := WinPlans(TerminalPowerShell, `C:\wt.exe`, ps, script, `C:\x`); len(p) != 1 || p[0].Program != ps {
		t.Errorf("PowerShell chosen: %+v", p)
	}
}

func TestTerminalChoices(t *testing.T) {
	if c := TerminalChoices("darwin"); len(c) != 3 || c[1].Value != TerminalITerm || c[2].Value != TerminalApple {
		t.Errorf("%v", c)
	}
	if c := TerminalChoices("windows"); len(c) != 3 || c[1].Value != TerminalWindows || c[2].Value != TerminalPowerShell {
		t.Errorf("%v", c)
	}
	if PermissionModes[0].Value != "auto" {
		t.Error("auto first")
	}
}

func TestWritesAnExecutableScriptInTheLaunchDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "launch")
	p, err := WriteScript(dir, ".sh", []byte(POSIXScript("/bin/zsh", Session{Command: "echo hi"}, "/tmp", "")))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p, filepath.Join(dir, "launch-")) || !strings.HasSuffix(p, ".sh") || strings.Contains(filepath.Base(p), " ") {
		t.Errorf("path %s", p)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o700 {
		t.Errorf("mode %v", st.Mode().Perm())
	}
	b, _ := os.ReadFile(p)
	if !strings.HasPrefix(string(b), "#!/bin/zsh -li\n") {
		t.Errorf("content %q", b)
	}
	q, _ := WriteScript(dir, ".sh", nil)
	if q == p {
		t.Error("each script gets its own name")
	}
}

func TestClaudeSettingsFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "code-link")
	p, err := EnsureClaudeSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != `{"attribution":{"commit":"","pr":""}}`+"\n" || filepath.Dir(p) != dir {
		t.Errorf("%s: %q", p, b)
	}
	os.WriteFile(p, []byte("{}"), 0o600)
	EnsureClaudeSettings(dir)
	if b, _ := os.ReadFile(p); string(b) != ClaudeSettings {
		t.Error("rewritten when it differs")
	}
}

func set(paths ...string) func(string) bool {
	m := map[string]bool{}
	for _, p := range paths {
		m[p] = true
	}
	return func(p string) bool { return m[p] }
}

func TestFindsWindowsCLIsOnPathThenInInstallLocations(t *testing.T) {
	env := SearchEnv{GOOS: "windows", Home: `C:\Users\A B`, AppData: `C:\Users\A B\AppData\Roaming`, LocalAppData: `C:\Users\A B\AppData\Local`,
		Paths: []string{`C:\Windows;C:\tools\`, `C:\Users\A B\AppData\Roaming\npm;c:\windows`}}
	// The native installer's claude.exe.
	env.Exists = set(`C:\Users\A B\.local\bin\claude.exe`)
	if f, ok := Search(ClaudeCode, env); !ok || f.Path != `C:\Users\A B\.local\bin\claude.exe` || f.PathDir != `C:\Users\A B\.local\bin` {
		t.Errorf("native: %+v %v", f, ok)
	}
	// npm's shims: the .cmd on PATH (from the registry's user PATH) before the install locations.
	env.Exists = set(`C:\Users\A B\AppData\Roaming\npm\codex.cmd`, `C:\Users\A B\AppData\Roaming\npm\codex.ps1`, `C:\Users\A B\.cargo\bin\codex.exe`)
	if f, ok := Search(Codex, env); !ok || f.Path != `C:\Users\A B\AppData\Roaming\npm\codex.cmd` {
		t.Errorf("npm: %+v", f)
	}
	// Only a .ps1 shim.
	env.Exists = set(`C:\tools\claude.ps1`)
	if f, ok := Search(ClaudeCode, env); !ok || f.Path != `C:\tools\claude.ps1` || f.PathDir != `C:\tools` {
		t.Errorf("ps1: %+v", f)
	}
	// WinGet.
	env.Exists = set(`C:\Users\A B\AppData\Local\Microsoft\WinGet\Links\codex.exe`)
	if f, ok := Search(Codex, env); !ok || f.PathDir != `C:\Users\A B\AppData\Local\Microsoft\WinGet\Links` {
		t.Errorf("winget: %+v", f)
	}
	env.Exists = set()
	if _, ok := Search(ClaudeCode, env); ok {
		t.Error("nothing installed")
	}
	// Duplicate PATH entries (any case) are searched once.
	n := 0
	for _, c := range Candidates(ClaudeCode, env) {
		if strings.EqualFold(c, `C:\Windows\claude.exe`) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("C:\\Windows searched %d times", n)
	}
}

func TestFindsMacCLIsInInstallLocations(t *testing.T) {
	env := SearchEnv{GOOS: "darwin", Home: "/Users/d", Paths: []string{"/usr/bin:/bin"},
		Glob: func(p string) []string {
			if p == "/Users/d/.nvm/versions/node/*/bin/codex" {
				return []string{"/Users/d/.nvm/versions/node/v20.1.0/bin/codex", "/Users/d/.nvm/versions/node/v22.3.0/bin/codex"}
			}
			return nil
		}}
	env.Exists = set("/Users/d/.local/bin/claude", "/opt/homebrew/bin/claude")
	if f, ok := Search(ClaudeCode, env); !ok || f.Path != "/Users/d/.local/bin/claude" || f.PathDir != "/Users/d/.local/bin" {
		t.Errorf("native installer first: %+v", f)
	}
	env.Exists = set("/Users/d/.claude/local/claude")
	if f, ok := Search(ClaudeCode, env); !ok || f.PathDir != "/Users/d/.claude/local" {
		t.Errorf("local install: %+v", f)
	}
	env.Exists = set("/Users/d/.nvm/versions/node/v20.1.0/bin/codex", "/Users/d/.nvm/versions/node/v22.3.0/bin/codex")
	if f, ok := Search(Codex, env); !ok || f.Path != "/Users/d/.nvm/versions/node/v22.3.0/bin/codex" {
		t.Errorf("nvm: %+v", f)
	}
	env.Exists = set("/usr/bin/codex")
	if f, ok := Search(Codex, env); !ok || f.Path != "/usr/bin/codex" {
		t.Errorf("PATH: %+v", f)
	}
	for _, c := range Candidates(Codex, env) {
		if strings.Contains(c, ".claude/local") {
			t.Error("Codex isn't in Claude's folder")
		}
	}
}

func TestToolNames(t *testing.T) {
	if ClaudeCode.DisplayName() != "Claude Code" || Codex.DisplayName() != "Codex" || ClaudeCode.Executable() != "claude" || Codex.Executable() != "codex" {
		t.Error("names")
	}
	if ClaudeCode.InstallURL() != "https://docs.anthropic.com/en/docs/claude-code/setup" || Codex.InstallURL() != "https://github.com/openai/codex" {
		t.Error("install links")
	}
}

func TestFindsCodexWhereCodeLinkAndOpenAIInstallIt(t *testing.T) {
	win := SearchEnv{GOOS: "windows", Home: `C:\Users\A B`, LocalAppData: `C:\Users\A B\AppData\Local`}
	if d := CodexDir(win); d != `C:\Users\A B\AppData\Local\Programs\code-link\codex` {
		t.Errorf("windows dir %q", d)
	}
	ours := `C:\Users\A B\AppData\Local\Programs\code-link\codex\current\bin\codex.exe`
	openai := `C:\Users\A B\AppData\Local\Programs\OpenAI\Codex\bin\codex.exe`
	win.Exists = set(ours, openai)
	if f, ok := Search(Codex, win); !ok || f.Path != ours || f.PathDir != `C:\Users\A B\AppData\Local\Programs\code-link\codex\current\bin` {
		t.Errorf("code-link's: %+v", f)
	}
	win.Exists = set(openai)
	if f, ok := Search(Codex, win); !ok || f.Path != openai {
		t.Errorf("OpenAI's: %+v", f)
	}
	for _, c := range Candidates(ClaudeCode, win) {
		if strings.Contains(c, "codex") {
			t.Errorf("Claude Code isn't in Codex's folders: %s", c)
		}
	}
	mac := SearchEnv{GOOS: "darwin", Home: "/Users/d"}
	if d := CodexDir(mac); d != "/Users/d/.local/share/code-link/codex" {
		t.Errorf("mac dir %q", d)
	}
	// On macOS the command is linked from ~/.local/bin, already searched.
	mac.Exists = set("/Users/d/.local/bin/codex")
	if f, ok := Search(Codex, mac); !ok || f.Path != "/Users/d/.local/bin/codex" {
		t.Errorf("mac: %+v", f)
	}
}

func TestASessionsModelsComeFromTheLaunchWindow(t *testing.T) {
	c := cfg("GLM-5.3", "Big", "Tiny", "Coder", ip(1_048_576), nil)
	c.Models = append(c.Models, config.Model{ID: "Qwen", BackendID: "cn3", Upstream: "qwen", ContextWindow: ip(131_072), AutoCompactAt: ip(100_000)})
	x := ctx(c, "auto")
	x.Model, x.Heavy, x.Background = "Qwen", "Big", ""
	cmd := render(t, ClaudeCode, x, POSIX)
	for _, want := range []string{"ANTHROPIC_MODEL=Qwen\n", "ANTHROPIC_DEFAULT_SONNET_MODEL=Qwen\n", "ANTHROPIC_DEFAULT_SONNET_MODEL_NAME=Qwen\n",
		"ANTHROPIC_DEFAULT_OPUS_MODEL=Big\n", "ANTHROPIC_DEFAULT_OPUS_MODEL_NAME=Big\n", "ANTHROPIC_DEFAULT_FABLE_MODEL=Big\n", "ANTHROPIC_DEFAULT_FABLE_MODEL_NAME=Big\n",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL=Qwen\n", "ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME=Qwen\n", "ANTHROPIC_SMALL_FAST_MODEL=Qwen\n",
		"CLAUDE_CODE_MAX_CONTEXT_TOKENS=131072\n", "CLAUDE_CODE_AUTO_COMPACT_WINDOW=100000\n"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("missing %q:\n%s", want, cmd)
		}
	}
	// The configured roles don't leak into a session with its own model.
	if strings.Contains(cmd, "Tiny") || strings.Contains(cmd, "GLM-5.3") {
		t.Errorf("roles leaked:\n%s", cmd)
	}
	x.Background = "Tiny"
	if cmd := render(t, ClaudeCode, x, PowerShell); !strings.Contains(cmd, "$env:ANTHROPIC_SMALL_FAST_MODEL = 'Tiny'") || !strings.Contains(cmd, "$env:ANTHROPIC_DEFAULT_FABLE_MODEL = 'Big'") {
		t.Errorf("%s", cmd)
	}
	cx := render(t, Codex, x, POSIX)
	if !strings.Contains(cx, `-c 'model="Qwen"'`) || !strings.Contains(cx, "model_context_window=131072") {
		t.Errorf("codex:\n%s", cx)
	}
	// A session model works even before roles exist.
	empty := config.Default()
	empty.Models = []config.Model{{ID: "m", BackendID: "llm.eidonix.com", Upstream: "m"}}
	if _, err := Render(ClaudeCode, Context{Config: &empty, Model: "m"}, POSIX); err != nil {
		t.Error(err)
	}
}
