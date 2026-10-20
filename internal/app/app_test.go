package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"codelink/internal/config"
	"codelink/internal/launch"
	"codelink/internal/testkit"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return p
}

// stubConfig is a complete config on a stub backend, on a free port.
func stubConfig(t *testing.T) config.Config {
	stub := testkit.StartStub(t, func(testkit.Seen, int) testkit.Reply { return testkit.JSONR(200, testkit.Text("ok")) })
	c := testkit.DefaultConfig(stub)
	c.Port = freePort(t)
	return c
}

func perm(t *testing.T, p string) os.FileMode {
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

// --- Persistence (PersistenceTests.swift) ---

func TestConfigRoundTripsThroughDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "code-link.json")
	c := base()
	c.Port = 19000
	c.Backends[0].APIKey = "sk-1"
	c.Models = []config.Model{{ID: "GLM", BackendID: "a", Upstream: "glm", ContextWindow: ip(1000)}}
	c.Roles.Main = "GLM"
	if err := config.Save(path, c); err != nil {
		t.Fatal(err)
	}
	back, warn := LoadSettings(path)
	if warn != "" || !reflect.DeepEqual(back, c) {
		t.Errorf("round trip (%s):\n%+v\n%+v", warn, back, c)
	}
	if runtime.GOOS != "windows" && perm(t, path) != 0o600 {
		t.Errorf("mode %v", perm(t, path))
	}
}

func TestSaveWritesClaudeSettingsNextToConfig(t *testing.T) {
	a := newTestApp(t)
	if r := a.Save(base()); !r.OK {
		t.Fatalf("%+v", r)
	}
	b, err := os.ReadFile(filepath.Join(a.Dir(), "claude-settings.json"))
	if err != nil || string(b) != `{"attribution":{"commit":"","pr":""}}`+"\n" {
		t.Errorf("%q %v", b, err)
	}
}

func TestUnreadableConfigIsSetAsideNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "code-link.json")
	os.WriteFile(path, []byte("{nope"), 0o600)
	c, warn := LoadSettings(path)
	if warn == "" || !strings.Contains(warn, "code-link.unreadable.json") {
		t.Errorf("warning %q", warn)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "code-link.unreadable.json")); string(b) != "{nope" {
		t.Errorf("set aside: %q", b)
	}
	if c.Port != config.DefaultPort || len(c.Backends) != 1 || c.Backends[0].ID != config.DefaultGatewayID || c.AccessToken == "" {
		t.Errorf("defaults: %+v", c)
	}
}

func TestLaunchSettingsPersistAndDefaultOldFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "code-link.json")
	os.WriteFile(path, []byte(`{"accessToken":"t","launch":{"terminal":"iterm"}}`), 0o600)
	c, warn := LoadSettings(path)
	if warn != "" || c.Launch.Terminal != "iterm" || c.Launch.PermissionMode != "" || len(c.Launch.RecentFolders) != 0 || c.Launch.OpenAtLogin {
		t.Errorf("partial: %+v %s", c.Launch, warn)
	}
	// An empty permission mode launches in auto mode.
	cmd, _ := launch.Render(launch.ClaudeCode, launch.Context{Config: func() *config.Config { x := base(); x.Roles.Main = "m"; return &x }()}, launch.POSIX)
	if !strings.Contains(cmd, "--permission-mode auto") {
		t.Errorf("%s", cmd)
	}
	c.Launch = config.Launch{Terminal: "terminal", PermissionMode: "plan", RecentFolders: []string{"/tmp/a"}, OpenAtLogin: true}
	config.Save(path, c)
	if back, _ := LoadSettings(path); !reflect.DeepEqual(back.Launch, c.Launch) {
		t.Errorf("%+v", back.Launch)
	}
}

func TestRecentFoldersAreNewestFirstDedupedAndCapped(t *testing.T) {
	var r []string
	for i := 0; i < 12; i++ {
		r = AddRecent(r, "/tmp/f"+strconv.Itoa(i))
	}
	if len(r) != RecentLimit || r[0] != filepath.Clean("/tmp/f11") {
		t.Fatalf("%v", r)
	}
	r = AddRecent(r, "/tmp/f9/")
	n := 0
	for _, p := range r {
		if p == filepath.Clean("/tmp/f9") {
			n++
		}
	}
	if r[0] != filepath.Clean("/tmp/f9") || n != 1 {
		t.Errorf("%v", r)
	}
	if r2 := AddRecent(r, ""); !reflect.DeepEqual(r2, r) {
		t.Error("empty is ignored")
	}
	if got := ExistingFolders(r, func(p string) bool { return p == filepath.Clean("/tmp/f9") }); !reflect.DeepEqual(got, []string{filepath.Clean("/tmp/f9")}) {
		t.Errorf("existing %v", got)
	}
	sep := string(filepath.Separator)
	home := sep + filepath.Join("Users", "d")
	if got := FolderTitle(filepath.Join(home, "code", "app"), home); got != "app — ~"+sep+filepath.Join("code", "app") {
		t.Errorf("title %q", got)
	}
	if got := FolderTitle(home, home); got != "d — ~" {
		t.Errorf("home title %q", got)
	}
}

func TestRecentFoldersEncodeAsAPlainArray(t *testing.T) {
	c := base()
	c.Launch.RecentFolders = []string{"/b", "/a"}
	path := filepath.Join(t.TempDir(), "code-link.json")
	config.Save(path, c)
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `"recentFolders": [`+"\n      \"/b\",\n      \"/a\"\n    ]") {
		t.Errorf("%s", b)
	}
}

func TestAccessTokenIsGeneratedOnceAsBase64URLInAPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "code-link", "code-link.json")
	c, _ := LoadSettings(path)
	if len(c.AccessToken) != 43 || strings.Trim(c.AccessToken, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_") != "" {
		t.Errorf("token %q", c.AccessToken)
	}
	again, _ := LoadSettings(path)
	if again.AccessToken != c.AccessToken {
		t.Error("generated once")
	}
	if runtime.GOOS != "windows" && perm(t, path) != 0o600 {
		t.Errorf("mode %v", perm(t, path))
	}
	if config.NewAccessToken() == config.NewAccessToken() {
		t.Error("random")
	}
}

// --- App ---

func TestSaveAppliesLiveAndKeepsTheTokenAndRecents(t *testing.T) {
	a := newTestApp(t)
	token := a.Config().AccessToken
	a.AddRecentFolder(t.TempDir())
	recents := a.Config().Launch.RecentFolders
	changes := 0
	a.OnChange = func() { changes++ }

	draft := stubConfig(t)
	draft.AccessToken = "from-the-page"
	draft.Launch.RecentFolders = nil
	r := a.Save(draft)
	if !r.OK || !a.Running() || r.Status.Kind != Running || r.Status.Port != draft.Port {
		t.Fatalf("%+v", r)
	}
	if a.Config().AccessToken != token || !reflect.DeepEqual(a.Config().Launch.RecentFolders, recents) || changes == 0 {
		t.Errorf("token %q recents %v", a.Config().AccessToken, a.Config().Launch.RecentFolders)
	}
	if back, _ := LoadSettings(a.Path); back.AccessToken != token || back.Roles.Main != "GLM" {
		t.Errorf("on disk: %+v", back)
	}
	// The engine answers with the saved token.
	req, _ := http.NewRequest("GET", "http://127.0.0.1:"+strconv.Itoa(draft.Port)+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("models: %v %v", resp, err)
	}
	resp.Body.Close()

	// A new port moves the listener.
	moved := draft
	moved.Port = freePort(t)
	if r := a.Save(moved); !r.OK || a.Engine().State().Port != moved.Port {
		t.Errorf("move: %+v %+v", r, a.Engine().State())
	}
	// Removing the main model stops the engine.
	off := moved
	off.Roles.Main = ""
	if r := a.Save(off); !r.OK || a.Running() || r.Status.Kind != NotConfigured {
		t.Errorf("stop: %+v", r)
	}
}

func TestProblemsBlockASave(t *testing.T) {
	a := newTestApp(t)
	bad := base()
	bad.Port = 80
	r := a.Save(bad)
	if r.OK || len(r.Problems) == 0 {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(filepath.Join(a.Dir(), "claude-settings.json")); err == nil {
		t.Error("nothing written")
	}
}

func TestBusyPortMovesToTheNextFreeOne(t *testing.T) {
	a := newTestApp(t)
	cfg := stubConfig(t)
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(cfg.Port))
	if err != nil {
		t.Skip("port taken meanwhile")
	}
	defer ln.Close()
	r := a.Save(cfg)
	st := a.Engine().State()
	if !r.OK || !st.Running || st.Port <= cfg.Port || st.Port > cfg.Port+PortRange {
		t.Fatalf("%+v %+v", r, st)
	}
	if s := a.Status(); !strings.Contains(s.Line, "(port "+strconv.Itoa(cfg.Port)+" was busy)") || s.Port != st.Port {
		t.Errorf("%+v", s)
	}
	found := false
	for _, e := range a.Activity.Entries() {
		found = found || strings.Contains(e.Text, "was busy")
	}
	if !found {
		t.Error("activity says so")
	}
	// Commands use the port actually running.
	cmd, err := a.Command(launch.Codex)
	if err != nil || !strings.Contains(cmd, "127.0.0.1:"+strconv.Itoa(st.Port)+"/v1") {
		t.Errorf("%s %v", cmd, err)
	}
	// Saving again with the same port stays on the free one.
	cfg.Roles.Opus = ""
	if r := a.Save(cfg); !r.OK || a.Engine().State().Port != st.Port {
		t.Errorf("stay: %+v %+v", r, a.Engine().State())
	}
}

func TestOpenAtLoginFollowsTheSetting(t *testing.T) {
	a := newTestApp(t)
	login := a.LoginItem.(*fakeLogin)
	c := base()
	c.Launch.OpenAtLogin = true
	if r := a.Save(c); !r.OK || !login.enabled || login.exe != "/Applications/code-link.app/Contents/MacOS/code-link" {
		t.Fatalf("%+v %+v", r, login)
	}
	c.Launch.OpenAtLogin = false
	a.Save(c)
	if login.enabled {
		t.Error("disabled")
	}
	c.Launch.OpenAtLogin = true
	a.Save(c)
	login.exe = ""
	if err := a.SyncLoginItem(); err != nil || login.exe == "" {
		t.Error("re-registered on start")
	}
}

func TestLaunchFindsTheCLIStartsTheEngineAndRemembersTheFolder(t *testing.T) {
	a := newTestApp(t)
	if err := a.Launch(launch.ClaudeCode, "/x"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("not configured: %v", err)
	}
	cfg := stubConfig(t)
	// Saved without starting: the launch starts the engine.
	a.mu.Lock()
	a.cfg = cfg
	a.mu.Unlock()
	a.engine.Apply(cfg)

	a.Find = func(launch.Tool) (launch.Found, bool) { return launch.Found{}, false }
	var missing *MissingCLIError
	if err := a.Launch(launch.Codex, "/x"); !errors.As(err, &missing) || missing.Tool != launch.Codex || !strings.Contains(err.Error(), "Install Codex") {
		t.Fatalf("missing: %v", err)
	}

	a.Find = func(launch.Tool) (launch.Found, bool) {
		return launch.Found{Path: "/opt/bin/claude", PathDir: "/opt/bin"}, true
	}
	var gotCmd, gotFolder, gotDir string
	var gotOpener launch.Opener
	a.Open = func(o launch.Opener, command, folder, pathDir string) error {
		gotOpener, gotCmd, gotFolder, gotDir = o, command, folder, pathDir
		return nil
	}
	folder := t.TempDir()
	if err := a.Launch(launch.ClaudeCode, folder); err != nil {
		t.Fatal(err)
	}
	if !a.Running() || !strings.Contains(gotCmd, "ANTHROPIC_BASE_URL") || !strings.Contains(gotCmd, "127.0.0.1:"+strconv.Itoa(cfg.Port)) {
		t.Errorf("command:\n%s", gotCmd)
	}
	if gotFolder != folder || gotDir != "/opt/bin" || !strings.Contains(gotOpener.Dir, filepath.Join("Eidonix", "code-link", "launch")) {
		t.Errorf("folder %q dir %q opener %+v", gotFolder, gotDir, gotOpener)
	}
	if rs := a.RecentFolders(); len(rs) != 1 || rs[0] != folder {
		t.Errorf("recents %v", rs)
	}
	if back, _ := LoadSettings(a.Path); len(back.Launch.RecentFolders) != 1 {
		t.Error("recents saved")
	}

	a.Open = func(launch.Opener, string, string, string) error { return errors.New("osascript said no") }
	var term *TerminalError
	if err := a.Launch(launch.ClaudeCode, folder); !errors.As(err, &term) || !strings.Contains(err.Error(), "Copy Claude Code command") {
		t.Errorf("terminal: %v", err)
	}
	a.ClearRecentFolders()
	if len(a.RecentFolders()) != 0 {
		t.Error("clear")
	}
}

func TestProbeIsRememberedForThePickers(t *testing.T) {
	a := newTestApp(t)
	cfg := stubConfig(t)
	p := a.Probe(context.Background(), cfg.Backends[0])
	if !p.Reachable || len(p.Models) != 2 {
		t.Fatalf("%+v", p)
	}
	v := a.Describe(cfg)
	var titles []string
	for _, it := range v.Groups[0].Items {
		titles = append(titles, it.Title)
	}
	// Enabled models first, then the probe's.
	if strings.Join(titles, ",") != "GLM,Small,Big,glm-upstream,other" && strings.Join(titles, ",") != "GLM,Small,Big,other" {
		t.Errorf("%v", titles)
	}
}

func TestCheckMainBackendReportsUnreachable(t *testing.T) {
	a := newTestApp(t)
	cfg := stubConfig(t)
	if r := a.Save(cfg); !r.OK {
		t.Fatal(r)
	}
	a.CheckMainBackend(context.Background())
	if s := a.Status(); s.Kind != Running {
		t.Errorf("%+v", s)
	}
	cfg.Backends[0].BaseURL = "http://127.0.0.1:" + strconv.Itoa(freePort(t)) + "/v1"
	if r := a.Save(cfg); !r.OK {
		t.Fatal(r)
	}
	a.CheckMainBackend(context.Background())
	if s := a.Status(); s.Kind != BackendUnreachable || !strings.Contains(s.Line, "stub is unreachable") {
		t.Errorf("%+v", s)
	}
}

func TestStartOnFreePortGivesUpAfterTheRange(t *testing.T) {
	a := newTestApp(t)
	cfg := stubConfig(t)
	var lns []net.Listener
	for p := cfg.Port; p <= cfg.Port+PortRange; p++ {
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
		if err != nil {
			t.Skip("ports in use")
		}
		lns = append(lns, ln)
	}
	defer func() {
		for _, l := range lns {
			l.Close()
		}
	}()
	if _, err := StartOnFreePort(a.engine, cfg); err == nil || !strings.Contains(err.Error(), "all in use") {
		t.Errorf("%v", err)
	}
	if a.engine.Env().Config.Port != cfg.Port {
		t.Error("configured port restored")
	}
}
