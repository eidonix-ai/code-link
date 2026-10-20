package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"codelink/internal/launch"
)

func TestParseVersion(t *testing.T) {
	for in, want := range map[string]string{
		"2.1.296 (Claude Code)":           "2.1.296",
		"codex-cli 0.163.0":               "0.163.0",
		"codex-cli 0.164.0-alpha.7\n":     "0.164.0-alpha.7",
		"rust-v0.163.0":                   "0.163.0",
		"2.1.304\n":                       "2.1.304",
		"warning: x\n2.0.1 (Claude Code)": "2.0.1",
		"Claude Code":                     "",
		"":                                "",
	} {
		if got := ParseVersion(in); got != want {
			t.Errorf("ParseVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"2.1.304", "2.1.296", 1},
		{"2.1.296", "2.1.304", -1},
		{"0.163.0", "0.163.0", 0},
		{"0.163.0", "0.99.9", 1}, // numeric, not alphabetical
		{"1.10.0", "1.9.12", 1},  // numeric, not alphabetical
		{"1.2", "1.2.0", 0},      // missing parts are 0
		{"1.2.1", "1.2", 1},      //
		{"v2.0.0", "2.0.0", 0},   // a leading v
		{"0.164.0-alpha.7", "0.164.0", -1},
		{"0.164.0", "0.164.0-alpha.7", 1},
		{"0.164.0-alpha.10", "0.164.0-alpha.9", 1},
		{"0.164.0-alpha.1", "0.164.0-beta", -1},
		{"0.164.0-alpha", "0.164.0-alpha.1", -1},
	} {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestStatusWhenMissing(t *testing.T) {
	f := newFeeds(t)
	f.publish(t, "0.163.0", nil, nil)
	m, _ := testManager(t, f)
	ctx := context.Background()
	if s := m.Status(ctx, ClaudeCode); s.Installed || s.Method != Native || s.Latest != "2.1.304" || s.UpdateAvailable || s.Error != "" {
		t.Errorf("claude: %+v", s)
	}
	if s := m.Status(ctx, Codex); s.Installed || s.Method != GitHub || s.Latest != "0.163.0" || s.UpdateAvailable || s.Error != "" {
		t.Errorf("codex: %+v", s)
	}
}

// claudeNative fakes the native installer's layout: ~/.local/bin/claude →
// ~/.local/share/claude/versions/<v>.
func claudeNative(t *testing.T, home, version string) string {
	target := filepath.Join(home, ".local", "share", "claude", "versions", version)
	script(t, target, `echo "`+version+` (Claude Code)"`)
	link := filepath.Join(home, ".local", "bin", "claude")
	os.Remove(link)
	symlink(t, target, link)
	return link
}

func TestStatusClaudeNativeOutdatedThenCurrent(t *testing.T) {
	needsUnix(t)
	f := newFeeds(t)
	m, _ := testManager(t, f)
	home := m.Env.Home
	link := claudeNative(t, home, "2.1.296")
	s := m.Status(context.Background(), ClaudeCode)
	if !s.Installed || s.Path != link || s.Method != Native || s.Version != "2.1.296" || s.Latest != "2.1.304" || !s.UpdateAvailable || !s.CanUpdate || s.Error != "" {
		t.Errorf("outdated: %+v", s)
	}
	claudeNative(t, home, "2.1.304")
	s = m.Status(context.Background(), ClaudeCode)
	if s.Version != "2.1.304" || s.UpdateAvailable {
		t.Errorf("current: %+v", s)
	}
}

func TestStatusClaudeFollowsTheStableChannel(t *testing.T) {
	needsUnix(t)
	f := newFeeds(t)
	m, _ := testManager(t, f)
	claudeNative(t, m.Env.Home, "2.1.290")
	os.MkdirAll(filepath.Join(m.Env.Home, ".claude"), 0o700)
	os.WriteFile(filepath.Join(m.Env.Home, ".claude", "settings.json"), []byte(`{"autoUpdatesChannel": "stable"}`), 0o600)
	s := m.Status(context.Background(), ClaudeCode)
	if s.Latest != "2.1.290" || s.UpdateAvailable {
		t.Errorf("stable: %+v", s)
	}
	if f.count("/claude/latest") != 0 || f.count("/claude/stable") != 1 {
		t.Errorf("feeds asked: latest %d, stable %d", f.count("/claude/latest"), f.count("/claude/stable"))
	}
}

func TestStatusClaudeFallsBackToNPMWhenTheFeedIsDown(t *testing.T) {
	f := newFeeds(t)
	f.remove("/claude/latest")
	m, _ := testManager(t, f)
	if s := m.Status(context.Background(), ClaudeCode); s.Latest != "2.1.304" {
		t.Errorf("%+v", s)
	}
}

func TestStatusNPMInstalls(t *testing.T) {
	needsUnix(t)
	f := newFeeds(t)
	f.publish(t, "0.164.0", nil, nil) // npm says 0.163.0: npm installs follow npm
	m, _ := testManager(t, f)
	home := m.Env.Home
	pkg := filepath.Join(home, ".npm-global", "lib", "node_modules", "@anthropic-ai", "claude-code", "bin", "claude.exe")
	script(t, pkg, `echo "2.1.304 (Claude Code)"`)
	symlink(t, pkg, filepath.Join(home, ".npm-global", "bin", "claude"))
	s := m.Status(context.Background(), ClaudeCode)
	if s.Method != NPM || s.Version != "2.1.304" || s.UpdateAvailable || !s.CanUpdate {
		t.Errorf("claude: %+v", s)
	}
	if f.count("/npm/@anthropic-ai/claude-code/latest") != 1 || f.count("/claude/latest") != 0 {
		t.Error("npm installs ask the npm registry")
	}

	cpkg := filepath.Join(home, ".npm-global", "lib", "node_modules", "@openai", "codex", "bin", "codex.js")
	script(t, cpkg, `echo "codex-cli 0.162.0"`)
	symlink(t, cpkg, filepath.Join(home, ".npm-global", "bin", "codex"))
	s = m.Status(context.Background(), Codex)
	if s.Method != NPM || s.Version != "0.162.0" || s.Latest != "0.163.0" || !s.UpdateAvailable {
		t.Errorf("codex: %+v", s)
	}
}

func TestStatusBrewInstalls(t *testing.T) {
	needsUnix(t)
	f := newFeeds(t)
	f.publish(t, "0.163.0", nil, nil)
	m, root := testManager(t, f)
	prefix := filepath.Join(root, "opt", "homebrew")
	cask := filepath.Join(prefix, "Caskroom", "codex", "0.160.0", "codex-aarch64-apple-darwin")
	script(t, cask, `echo "codex-cli 0.160.0"`)
	link := filepath.Join(prefix, "bin", "codex")
	symlink(t, cask, link)
	m.Find = func(Tool) (launch.Found, bool) { return launch.Found{Path: link}, true }
	s := m.Status(context.Background(), Codex)
	if s.Method != Brew || s.Version != "0.160.0" || s.Latest != "0.163.0" || !s.UpdateAvailable || !s.CanUpdate {
		t.Errorf("%+v", s)
	}
	in := m.detect(Codex, link)
	if in.brewName != "codex" || !strings.HasSuffix(in.brew, "/opt/homebrew/bin/brew") {
		t.Errorf("brew: %+v", in)
	}

	// Claude Code's casks: claude-code is the stable channel, claude-code@latest the latest.
	ccask := filepath.Join(prefix, "Caskroom", "claude-code", "2.1.290", "claude")
	script(t, ccask, `echo "2.1.290 (Claude Code)"`)
	clink := filepath.Join(prefix, "bin", "claude")
	symlink(t, ccask, clink)
	m.Find = func(Tool) (launch.Found, bool) { return launch.Found{Path: clink}, true }
	if s := m.Status(context.Background(), ClaudeCode); s.Method != Brew || s.Latest != "2.1.290" || s.UpdateAvailable {
		t.Errorf("stable cask: %+v", s)
	}
	if in := m.detect(ClaudeCode, "/opt/homebrew/Caskroom/claude-code@latest/2.1.304/claude"); in.channel != "latest" || in.brewName != "claude-code@latest" {
		t.Errorf("latest cask: %+v", in)
	}
}

func TestStatusUnknownInstallsOfferNoUpdate(t *testing.T) {
	needsUnix(t)
	f := newFeeds(t)
	m, _ := testManager(t, f)
	script(t, filepath.Join(m.Env.Home, ".bun", "bin", "claude"), `echo "2.1.200 (Claude Code)"`)
	s := m.Status(context.Background(), ClaudeCode)
	if !s.Installed || s.Method != Unknown || s.CanUpdate || s.Version != "2.1.200" || !s.UpdateAvailable {
		t.Errorf("%+v", s)
	}
	err := m.Update(context.Background(), ClaudeCode, nil)
	var te *Error
	if !errors.As(err, &te) || !strings.Contains(te.Message, "doesn't know how") {
		t.Errorf("update: %v", err)
	}
}

func TestStatusReportsAnUnreadableVersion(t *testing.T) {
	needsUnix(t)
	f := newFeeds(t)
	m, _ := testManager(t, f)
	script(t, filepath.Join(m.Env.Home, ".local", "bin", "claude"), `echo "something else"; exit 2`)
	s := m.Status(context.Background(), ClaudeCode)
	if !s.Installed || s.Version != "" || s.UpdateAvailable || !strings.Contains(s.Error, "Couldn't read the version of Claude Code") {
		t.Errorf("%+v", s)
	}
}

func TestOfflineLeavesLatestUnknownAndIsCachedBriefly(t *testing.T) {
	needsUnix(t)
	f := newFeeds(t)
	m, _ := testManager(t, f)
	claudeNative(t, m.Env.Home, "2.1.296")
	f.remove("/claude/latest")
	f.remove("/npm/@anthropic-ai/claude-code/latest")
	now := time.Unix(1_800_000_000, 0)
	m.Now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		s := m.Status(context.Background(), ClaudeCode)
		if s.Latest != "" || s.UpdateAvailable || s.Error != "" || s.Version != "2.1.296" {
			t.Fatalf("offline: %+v", s)
		}
	}
	if n := f.count("/claude/latest"); n != 1 {
		t.Errorf("failed lookups are cached: asked %d times", n)
	}
	now = now.Add(11 * time.Minute)
	m.Status(context.Background(), ClaudeCode)
	if n := f.count("/claude/latest"); n != 2 {
		t.Errorf("then retried: asked %d times", n)
	}

	// A server that isn't there at all.
	f.srv.Close()
	m.cache = nil
	if s := m.Status(context.Background(), ClaudeCode); s.Latest != "" || s.UpdateAvailable || s.Error != "" {
		t.Errorf("unreachable: %+v", s)
	}
}

func TestLatestIsCachedForSixHours(t *testing.T) {
	f := newFeeds(t)
	f.publish(t, "0.163.0", nil, nil)
	m, _ := testManager(t, f)
	now := time.Unix(1_800_000_000, 0)
	m.Now = func() time.Time { return now }
	for i := 0; i < 5; i++ {
		m.Status(context.Background(), Codex)
	}
	if n := f.count(releasePath); n != 1 {
		t.Errorf("asked GitHub %d times", n)
	}
	now = now.Add(5 * time.Hour)
	m.Status(context.Background(), Codex)
	if n := f.count(releasePath); n != 1 {
		t.Errorf("within 6 h: asked %d times", n)
	}
	now = now.Add(2 * time.Hour)
	f.publish(t, "0.164.0", nil, nil)
	if s := m.Status(context.Background(), Codex); s.Latest != "0.164.0" || f.count(releasePath) != 2 {
		t.Errorf("after 6 h: %+v, asked %d times", s, f.count(releasePath))
	}
}

func TestCodexLatestFallsBackToNPMAndIgnoresOtherTags(t *testing.T) {
	f := newFeeds(t)
	m, _ := testManager(t, f)
	f.setBytes(releasePath, []byte(`{"tag_name":"desktop-v1.2.3","assets":[]}`))
	if s := m.Status(context.Background(), Codex); s.Latest != "0.163.0" {
		t.Errorf("%+v", s)
	}
}

func TestRefusesPlainHTTP(t *testing.T) {
	f := newFeeds(t)
	m, _ := testManager(t, f)
	m.Sources.ClaudeReleases = strings.Replace(m.Sources.ClaudeReleases, "https://", "http://", 1)
	m.Sources.NPMRegistry = strings.Replace(m.Sources.NPMRegistry, "https://", "http://", 1)
	if s := m.Status(context.Background(), ClaudeCode); s.Latest != "" {
		t.Errorf("%+v", s)
	}
	if f.count("/claude/latest") != 0 {
		t.Error("asked over http")
	}
}

func TestDetectsWindowsInstalls(t *testing.T) {
	e := Env{GOOS: "windows", Home: `C:\Users\A B`, AppData: `C:\Users\A B\AppData\Roaming`, LocalAppData: `C:\Users\A B\AppData\Local`}
	present := map[string]bool{
		`c:\users\a b\appdata\roaming\npm\node_modules\@openai\codex`:             true,
		`c:\users\a b\appdata\roaming\npm\node_modules\@anthropic-ai\claude-code`: true,
	}
	m := &Manager{Env: e, Exists: func(p string) bool { return present[strings.ToLower(p)] }}
	for _, c := range []struct {
		tool Tool
		path string
		want Method
	}{
		{ClaudeCode, `C:\Users\A B\.local\bin\claude.exe`, Native},
		{ClaudeCode, `C:\Users\A B\AppData\Roaming\npm\claude.cmd`, NPM},
		{ClaudeCode, `C:\Users\A B\AppData\Roaming\npm\claude.ps1`, NPM},
		{Codex, `c:\users\a b\appdata\roaming\npm\codex.cmd`, NPM},
		{ClaudeCode, `C:\Users\A B\AppData\Local\Microsoft\WinGet\Links\claude.exe`, WinGet},
		{Codex, `C:\Users\A B\AppData\Local\Microsoft\WinGet\Links\codex.exe`, WinGet},
		{Codex, `C:\Users\A B\AppData\Local\Programs\code-link\codex\current\bin\codex.exe`, GitHub},
		{Codex, `C:\Users\A B\AppData\Local\Programs\OpenAI\Codex\bin\codex.exe`, Standalone},
		{Codex, `C:\Users\A B\.codex\packages\standalone\current\bin\codex.exe`, Standalone},
		{Codex, `C:\Users\A B\AppData\Local\pnpm\codex.cmd`, Unknown},
		{Codex, `C:\Users\A B\scoop\shims\codex.exe`, Unknown},
		{Codex, `C:\Users\A B\.local\bin\codex.exe`, Unknown},
		{ClaudeCode, `C:\Users\A B\.cargo\bin\claude.exe`, Unknown},
	} {
		if got := m.detect(c.tool, c.path).method; got != c.want {
			t.Errorf("%s %s: %s, want %s", c.tool, c.path, got, c.want)
		}
	}
}

func TestDetectsMacInstalls(t *testing.T) {
	e := Env{GOOS: "darwin", Home: "/Users/d"}
	m := &Manager{Env: e, Exists: func(string) bool { return false }}
	for _, c := range []struct {
		tool Tool
		path string
		want Method
	}{
		{ClaudeCode, "/Users/d/.local/bin/claude", Native},
		{ClaudeCode, "/Users/d/.local/share/claude/versions/2.1.304", Native},
		{ClaudeCode, "/Users/d/.claude/local/claude", Native},
		{ClaudeCode, "/Users/d/.nvm/versions/node/v22.3.0/lib/node_modules/@anthropic-ai/claude-code/cli.js", NPM},
		{Codex, "/usr/local/lib/node_modules/@openai/codex/bin/codex.js", NPM},
		{Codex, "/opt/homebrew/Caskroom/codex/0.163.0/codex-aarch64-apple-darwin", Brew},
		{Codex, "/usr/local/Cellar/codex/0.163.0/bin/codex", Brew},
		{Codex, "/Users/d/.local/share/code-link/codex/versions/0.163.0/bin/codex", GitHub},
		{Codex, "/Users/d/.codex/packages/standalone/releases/0.163.0/bin/codex", Standalone},
		{Codex, "/Users/d/.cargo/bin/codex", Unknown},
		{Codex, "/Users/d/.local/bin/codex", Unknown}, // not a link into one of the above
		{ClaudeCode, "/Users/d/Library/pnpm/claude", Unknown},
	} {
		if got := m.detect(c.tool, c.path).method; got != c.want {
			t.Errorf("%s %s: %s, want %s", c.tool, c.path, got, c.want)
		}
	}
}

func TestClaudeInstallerCommandLines(t *testing.T) {
	for _, c := range []struct {
		env  Env
		want Cmd
	}{
		{Env{GOOS: "darwin", Home: "/Users/d", Shell: "/bin/zsh"},
			Cmd{Path: "/bin/zsh", Args: []string{"-lc", "set -o pipefail; curl -fsSL https://claude.ai/install.sh | bash"}}},
		{Env{GOOS: "windows", Home: `C:\Users\d`, PowerShell: `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`},
			Cmd{Path: `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, Args: []string{"-NoProfile", "-NonInteractive",
				"-ExecutionPolicy", "Bypass", "-Command", "$ErrorActionPreference = 'Stop'; irm https://claude.ai/install.ps1 | iex"}}},
	} {
		installed := false
		rec := &recorder{do: func(Cmd) ([]string, error) { installed = true; return nil, nil }}
		m := &Manager{Env: c.env, Sources: Official, Run: rec.run, Exists: func(string) bool { return false },
			Find: func(Tool) (launch.Found, bool) {
				return launch.Found{Path: "/x/claude"}, installed
			},
			Version: func(context.Context, string) (string, error) { return "2.1.304", nil }}
		progress, lines := collect()
		if err := m.Install(context.Background(), ClaudeCode, progress); err != nil {
			t.Fatalf("%s: %v", c.env.GOOS, err)
		}
		if len(rec.cmds) != 1 || !reflect.DeepEqual(rec.cmds[0], c.want) {
			t.Errorf("%s: ran %#v", c.env.GOOS, rec.cmds)
		}
		if !hasLine(lines(), "Claude Code 2.1.304 is installed.") {
			t.Errorf("%s: progress %q", c.env.GOOS, lines())
		}
	}
}

func TestClaudeInstallerFailureIsPlainWithTheTail(t *testing.T) {
	rec := &recorder{do: func(Cmd) ([]string, error) {
		return []string{"Setting up Claude Code...", "Checksum verification failed"}, exitErr(1)
	}}
	m := &Manager{Env: Env{GOOS: "darwin", Shell: "/bin/zsh"}, Sources: Official, Run: rec.run,
		Find: func(Tool) (launch.Found, bool) { return launch.Found{}, false }}
	err := m.Install(context.Background(), ClaudeCode, nil)
	var te *Error
	if !errors.As(err, &te) || te.Message != "The Claude Code installer failed (exit 1)." || !reflect.DeepEqual(te.Tail, []string{"Setting up Claude Code...", "Checksum verification failed"}) {
		t.Fatalf("%#v", err)
	}
	if !strings.HasSuffix(err.Error(), "\n\nSetting up Claude Code...\nChecksum verification failed") {
		t.Errorf("Error(): %q", err.Error())
	}

	// It exited 0 but there's no claude.
	rec.do = nil
	err = m.Install(context.Background(), ClaudeCode, nil)
	if !errors.As(err, &te) || !strings.Contains(te.Message, "claude can't be found") {
		t.Errorf("%v", err)
	}
}

func TestUpdateCommandLines(t *testing.T) {
	mac := Env{GOOS: "darwin", Home: "/Users/d", Shell: "/bin/zsh"}
	win := Env{GOOS: "windows", Home: `C:\Users\d`, AppData: `C:\Users\d\AppData\Roaming`, LocalAppData: `C:\Users\d\AppData\Local`, PowerShell: `PS.exe`}
	ps := func(script string) []string {
		return []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script}
	}
	for _, c := range []struct {
		name string
		env  Env
		tool Tool
		path string
		want Cmd
	}{
		{"claude native", mac, ClaudeCode, "/Users/d/.local/bin/claude",
			Cmd{Path: "/Users/d/.local/bin/claude", Args: []string{"update"}}},
		{"claude native windows", win, ClaudeCode, `C:\Users\d\.local\bin\claude.exe`,
			Cmd{Path: `C:\Users\d\.local\bin\claude.exe`, Args: []string{"update"}}},
		{"codex npm windows", win, Codex, `C:\Users\d\AppData\Roaming\npm\codex.cmd`,
			Cmd{Path: "PS.exe", Args: ps("npm install -g @openai/codex@latest"), Env: []string{`Path=C:\Users\d\AppData\Roaming\npm;C:\fresh`}}},
		{"claude brew latest cask", mac, ClaudeCode, "/opt/homebrew/Caskroom/claude-code@latest/2.1.3/claude",
			Cmd{Path: "/opt/homebrew/bin/brew", Args: []string{"upgrade", "claude-code@latest"}, Env: []string{"HOMEBREW_NO_ENV_HINTS=1", "NONINTERACTIVE=1"}}},
		{"codex brew", mac, Codex, "/usr/local/Caskroom/codex/0.160.0/codex-x86_64-apple-darwin",
			Cmd{Path: "/usr/local/bin/brew", Args: []string{"upgrade", "codex"}, Env: []string{"HOMEBREW_NO_ENV_HINTS=1", "NONINTERACTIVE=1"}}},
		{"claude winget", win, ClaudeCode, `C:\Users\d\AppData\Local\Microsoft\WinGet\Links\claude.exe`,
			Cmd{Path: "winget.exe", Args: []string{"upgrade", "--id", "Anthropic.ClaudeCode", "--exact", "--silent",
				"--accept-source-agreements", "--accept-package-agreements", "--disable-interactivity"}, Env: []string{`Path=C:\fresh`}}},
		{"codex winget", win, Codex, `C:\Users\d\AppData\Local\Microsoft\WinGet\Links\codex.exe`,
			Cmd{Path: "winget.exe", Args: []string{"upgrade", "--id", "OpenAI.Codex", "--exact", "--silent",
				"--accept-source-agreements", "--accept-package-agreements", "--disable-interactivity"}, Env: []string{`Path=C:\fresh`}}},
		{"codex standalone", mac, Codex, "/Users/d/.codex/packages/standalone/current/bin/codex",
			Cmd{Path: "/bin/zsh", Args: []string{"-lc", "set -o pipefail; curl -fsSL https://chatgpt.com/codex/install.sh | sh"}, Env: []string{"CODEX_NON_INTERACTIVE=1"}}},
		{"codex standalone windows", win, Codex, `C:\Users\d\AppData\Local\Programs\OpenAI\Codex\bin\codex.exe`,
			Cmd{Path: "PS.exe", Args: ps("$ErrorActionPreference = 'Stop'; irm https://chatgpt.com/codex/install.ps1 | iex"), Env: []string{"CODEX_NON_INTERACTIVE=1"}}},
	} {
		rec := &recorder{}
		m := &Manager{Env: c.env, Sources: Official, Run: rec.run,
			Exists:    func(p string) bool { return strings.HasSuffix(p, `npm\node_modules\@openai\codex`) },
			Find:      func(Tool) (launch.Found, bool) { return launch.Found{Path: c.path}, true },
			Version:   func(context.Context, string) (string, error) { return "9.9.9", nil },
			FreshPath: func() string { return `C:\fresh` }}
		if err := m.Update(context.Background(), c.tool, nil); err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if len(rec.cmds) != 1 || !reflect.DeepEqual(rec.cmds[0], c.want) {
			t.Errorf("%s: ran %#v\nwant %#v", c.name, rec.cmds, c.want)
		}
	}
}

// npm runs from the folder of the shim found (an nvm version's bin, where
// its npm is), in the login shell.
func TestNPMUpdateRunsTheShimsNPM(t *testing.T) {
	needsUnix(t)
	f := newFeeds(t)
	m, _ := testManager(t, f)
	bin := filepath.Join(m.Env.Home, ".nvm", "versions", "node", "v22.3.0", "bin")
	cli := filepath.Join(m.Env.Home, ".nvm", "versions", "node", "v22.3.0", "lib", "node_modules", "@anthropic-ai", "claude-code", "cli.js")
	script(t, cli, `echo "2.1.296 (Claude Code)"`)
	symlink(t, cli, filepath.Join(bin, "claude"))
	m.Find = func(Tool) (launch.Found, bool) { return launch.Found{Path: filepath.Join(bin, "claude")}, true }
	rec := &recorder{}
	m.Run = rec.run
	if err := m.Update(context.Background(), ClaudeCode, nil); err != nil {
		t.Fatal(err)
	}
	want := Cmd{Path: "/bin/sh", Args: []string{"-lc", "export PATH='" + bin + `':"$PATH"; npm install -g @anthropic-ai/claude-code@latest`}}
	if len(rec.cmds) != 1 || !reflect.DeepEqual(rec.cmds[0], want) {
		t.Errorf("ran %#v", rec.cmds)
	}
}

func TestUpdateFailuresArePlain(t *testing.T) {
	rec := &recorder{do: func(Cmd) ([]string, error) { return []string{"npm ERR! code EACCES"}, exitErr(243) }}
	m := &Manager{Env: Env{GOOS: "darwin", Home: "/Users/d", Shell: "/bin/zsh"}, Sources: Official, Run: rec.run,
		Exists: func(string) bool { return false },
		Find: func(Tool) (launch.Found, bool) {
			return launch.Found{Path: "/usr/local/lib/node_modules/@openai/codex/bin/codex.js"}, true
		}}
	err := m.Update(context.Background(), Codex, nil)
	var te *Error
	if !errors.As(err, &te) || te.Message != "Updating Codex with npm failed (exit 243)." || te.Tail[0] != "npm ERR! code EACCES" {
		t.Errorf("%#v", err)
	}
	m.Find = func(Tool) (launch.Found, bool) { return launch.Found{}, false }
	if err := m.Update(context.Background(), Codex, nil); err == nil || err.Error() != "Codex isn't installed." {
		t.Errorf("missing: %v", err)
	}
}

func TestOneInstallAtATime(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	rec := &recorder{do: func(Cmd) ([]string, error) { close(started); <-release; return nil, nil }}
	m := &Manager{Env: Env{GOOS: "darwin", Shell: "/bin/zsh"}, Sources: Official, Run: rec.run,
		Find: func(Tool) (launch.Found, bool) { return launch.Found{}, false }}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); m.Install(context.Background(), ClaudeCode, nil) }()
	<-started
	err := m.Install(context.Background(), ClaudeCode, nil)
	if err == nil || !strings.Contains(err.Error(), "already being installed") {
		t.Errorf("%v", err)
	}
	close(release)
	wg.Wait()
}

func TestPathRegistration(t *testing.T) {
	const dir = `C:\Users\A B\AppData\Local\Programs\code-link\codex\current\bin`
	vars := map[string]string{"LOCALAPPDATA": `C:\Users\A B\AppData\Local`, "USERPROFILE": `C:\Users\A B`}
	for _, c := range []struct {
		name, before string
		expand       bool
		after        string // "" = unchanged
	}{
		{"none yet", "", true, dir},
		{"appended", `C:\Users\A B\AppData\Local\Microsoft\WindowsApps`, true, `C:\Users\A B\AppData\Local\Microsoft\WindowsApps;` + dir},
		{"trailing semicolon", `C:\tools;`, false, `C:\tools;` + dir},
		{"already there", `C:\tools;` + dir, true, ""},
		{"other spelling", `C:\tools;"c:\users\a b\appdata\local\programs\code-link\codex\current\bin\"`, true, ""},
		{"as a variable", `%LOCALAPPDATA%\Programs\code-link\codex\current\bin;C:\tools`, true, ""},
		{"a different folder", `%LOCALAPPDATA%\Programs\code-link\codex\bin`, true, `%LOCALAPPDATA%\Programs\code-link\codex\bin;` + dir},
	} {
		r := &fakeRegistry{value: c.before, expand: c.expand, vars: vars}
		changed, err := addToUserPath(r, dir)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if c.after == "" {
			if changed || r.sets != 0 || r.broadcasts != 0 {
				t.Errorf("%s: changed it to %q", c.name, r.value)
			}
			continue
		}
		if !changed || r.value != c.after || r.sets != 1 || r.broadcasts != 1 || r.expand != c.expand {
			t.Errorf("%s: %q (expand %v, %d sets, %d broadcasts)", c.name, r.value, r.expand, r.sets, r.broadcasts)
		}
	}
	r := &fakeRegistry{failSet: true}
	if _, err := addToUserPath(r, dir); err == nil || r.broadcasts != 0 {
		t.Error("a failed write is an error, and nothing is broadcast")
	}
}

type fakeRegistry struct {
	value            string
	expand           bool
	vars             map[string]string
	sets, broadcasts int
	failSet          bool
}

func (r *fakeRegistry) UserPath() (string, bool, error) { return r.value, r.expand, nil }
func (r *fakeRegistry) SetUserPath(v string, expand bool) error {
	if r.failSet {
		return errors.New("access denied")
	}
	r.value, r.expand = v, expand
	r.sets++
	return nil
}
func (r *fakeRegistry) Expand(s string) string {
	return expandWindowsVars(s, func(k string) (string, bool) { v, ok := r.vars[strings.ToUpper(k)]; return v, ok })
}
func (r *fakeRegistry) Broadcast() { r.broadcasts++ }

func TestExpandWindowsVars(t *testing.T) {
	lookup := func(k string) (string, bool) {
		if strings.EqualFold(k, "LocalAppData") {
			return `C:\L`, true
		}
		return "", false
	}
	if got := expandWindowsVars(`%LOCALAPPDATA%\x;%NOPE%\y`, lookup); got != `C:\L\x;%NOPE%\y` {
		t.Error(got)
	}
}

func TestNewUsesTheOfficialSources(t *testing.T) {
	m := New()
	if m.Sources != Official || m.HTTP == nil || m.Find == nil || m.Run == nil || m.Version == nil || m.Link == nil {
		t.Errorf("%+v", m)
	}
	for _, u := range []string{Official.ClaudeReleases, Official.ClaudeInstallSh, Official.ClaudeInstallPS1, Official.NPMRegistry, Official.GitHubAPI, Official.CodexInstallSh, Official.CodexInstallPS1} {
		if !strings.HasPrefix(u, "https://") {
			t.Errorf("%s isn't https", u)
		}
	}
	if (runtime.GOOS == "windows") != (m.Registry != nil) {
		t.Error("the PATH registry is Windows only")
	}
}
