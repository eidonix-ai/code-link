package selfupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func keys(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func manifestBytes(t *testing.T, m Manifest) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// --- Manifest ---

func TestVerifyAcceptsSignedManifest(t *testing.T) {
	pub, priv := keys(t)
	body := manifestBytes(t, Manifest{Version: "0.2.1", Notes: "n", Files: map[string]File{
		"windows-amd64": {URL: "https://x/a.exe", SHA256: sum([]byte("a")), Size: 1}}})
	m, err := Verify(body, Sign(body, priv), pub)
	if err != nil || m.Version != "0.2.1" || m.Files["windows-amd64"].Size != 1 {
		t.Fatalf("got %+v, %v", m, err)
	}
}

func TestVerifyRefusesBadSignatures(t *testing.T) {
	pub, priv := keys(t)
	other, _ := keys(t)
	body := manifestBytes(t, Manifest{Version: "0.2.1"})
	sig := Sign(body, priv)
	tampered := append([]byte{}, body...)
	tampered[len(tampered)-2] = ' '
	cases := map[string]struct {
		body, sig []byte
		pub       ed25519.PublicKey
	}{
		"other key":       {body, sig, other},
		"changed bytes":   {tampered, sig, pub},
		"not base64":      {body, []byte("%%%"), pub},
		"short":           {body, []byte("AAAA"), pub},
		"empty":           {body, nil, pub},
		"signature of {}": {body, Sign([]byte("{}"), priv), pub},
	}
	for name, c := range cases {
		if _, err := Verify(c.body, c.sig, c.pub); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: got %v, want ErrBadSignature", name, err)
		}
	}
	if _, err := Verify(body, sig, nil); !errors.Is(err, ErrNoPublicKey) {
		t.Errorf("no key: %v", err)
	}
}

func TestVerifyRefusesUnusableManifests(t *testing.T) {
	pub, priv := keys(t)
	good := File{URL: "https://x/a", SHA256: sum(nil), Size: 10}
	for name, m := range map[string]Manifest{
		"no version":  {Files: map[string]File{"darwin-arm64": good}},
		"dev version": {Version: "dev"},
		"no url":      {Version: "1.0.0", Files: map[string]File{"darwin-arm64": {SHA256: good.SHA256, Size: 1}}},
		"short sha":   {Version: "1.0.0", Files: map[string]File{"darwin-arm64": {URL: "u", SHA256: "abc", Size: 1}}},
		"zero size":   {Version: "1.0.0", Files: map[string]File{"darwin-arm64": {URL: "u", SHA256: good.SHA256}}},
		"huge size":   {Version: "1.0.0", Files: map[string]File{"darwin-arm64": {URL: "u", SHA256: good.SHA256, Size: 2 << 30}}},
	} {
		body := manifestBytes(t, m)
		if _, err := Verify(body, Sign(body, priv), pub); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Verify([]byte("not json"), Sign([]byte("not json"), priv), pub); err == nil {
		t.Error("signed garbage accepted")
	}
}

func TestEmbeddedKeyIsEmptyOrValid(t *testing.T) {
	if _, err := PublicKey(); err != nil && !errors.Is(err, ErrNoPublicKey) {
		t.Fatalf("update-public-key.txt is not a valid key: %v", err)
	}
}

func TestVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"0.2.0", "0.2.0", 0}, {"0.2.1", "0.2.0", 1}, {"0.2.0", "0.10.0", -1}, {"1.0.0", "0.99.99", 1},
		{"v0.3.0", "0.3.0", 0}, {"0.3", "0.3.0", 0}, {"0.3.1", "0.3", 1}, {"dev", "0.0.1", -1}, {"0.2.0", "", 1},
	} {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	for _, bad := range []string{"", "dev", "0.2.0-beta", "1.2.3.4", "1..2", "a.b.c", " ", "-1.0.0", "0.2.x"} {
		if _, ok := ParseVersion(bad); ok {
			t.Errorf("ParseVersion(%q) ok", bad)
		}
	}
	if !Newer("0.2.1", "0.2.0") || Newer("0.2.0", "0.2.0") || Newer("0.1.9", "0.2.0") || Newer("0.3.0", "dev") || Newer("x", "0.1.0") {
		t.Error("Newer")
	}
}

// --- Feed and checks ---

type feed struct {
	t        *testing.T
	priv     ed25519.PrivateKey
	pub      ed25519.PublicKey
	srv      *httptest.Server
	mu       sync.Mutex
	manifest []byte
	sig      []byte
	files    map[string][]byte
	// lie overrides what the file endpoint sends.
	lie     func(w http.ResponseWriter, r *http.Request) bool
	fetches atomic.Int32
}

func newFeed(t *testing.T) *feed {
	f := &feed{t: t, files: map[string][]byte{}}
	f.pub, f.priv = keys(t)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/update/manifest.json":
			f.fetches.Add(1)
			if f.manifest == nil {
				http.NotFound(w, r)
				return
			}
			w.Write(f.manifest)
		case r.URL.Path == "/update/manifest.json.sig":
			w.Write(f.sig)
		case strings.HasPrefix(r.URL.Path, "/files/"):
			if f.lie != nil && f.lie(w, r) {
				return
			}
			b, ok := f.files[strings.TrimPrefix(r.URL.Path, "/files/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Write(b)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// publish signs a manifest offering content as version for platform.
func (f *feed) publish(version, platform, name string, content []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[name] = content
	body := manifestBytes(f.t, Manifest{Version: version, Notes: "Faster.", Files: map[string]File{
		platform: {URL: "/files/" + name, SHA256: sum(content), Size: int64(len(content))}}})
	f.manifest, f.sig = body, Sign(body, f.priv)
}

func (f *feed) url() string { return f.srv.URL + "/update/manifest.json" }

func TestCheckOffersNewerVersionOnly(t *testing.T) {
	f := newFeed(t)
	f.publish("0.2.1", "windows-amd64", "a.exe", []byte("new"))
	changes := 0
	u := New(Options{Current: "0.2.0", ManifestURL: f.url(), PublicKey: f.pub, GOOS: "windows", Platform: "windows-amd64",
		OnChange: func() { changes++ }})
	s := u.Check(context.Background())
	if !s.Available || s.Latest != "0.2.1" || s.Notes != "Faster." || s.Error != "" || s.CheckedAt == "" || changes != 2 {
		t.Fatalf("state %+v, changes %d", s, changes)
	}
	if MenuLabel(s) != "Update code-link to 0.2.1…" {
		t.Errorf("menu %q", MenuLabel(s))
	}

	same := New(Options{Current: "0.2.1", ManifestURL: f.url(), PublicKey: f.pub, GOOS: "windows", Platform: "windows-amd64"})
	if s := same.Check(context.Background()); s.Available || MenuLabel(s) != "" {
		t.Errorf("same version offered: %+v", s)
	}
	other := New(Options{Current: "0.2.0", ManifestURL: f.url(), PublicKey: f.pub, GOOS: "darwin", Platform: "darwin-arm64"})
	if s := other.Check(context.Background()); s.Available {
		t.Errorf("offered without a file for the platform: %+v", s)
	}
}

func TestCheckFailuresOfferNothing(t *testing.T) {
	f := newFeed(t)
	f.publish("0.9.0", "windows-amd64", "a.exe", []byte("new"))
	_, wrong := keys(t)
	f.sig = Sign(f.manifest, wrong)
	u := New(Options{Current: "0.2.0", ManifestURL: f.url(), PublicKey: f.pub, GOOS: "windows", Platform: "windows-amd64"})
	s := u.Check(context.Background())
	if s.Available || !strings.Contains(s.Error, "isn't signed") {
		t.Errorf("bad signature: %+v", s)
	}
	gone := New(Options{Current: "0.2.0", ManifestURL: f.srv.URL + "/missing.json", PublicKey: f.pub, GOOS: "windows", Platform: "windows-amd64"})
	if s := gone.Check(context.Background()); s.Available || !strings.Contains(s.Error, "internet connection") {
		t.Errorf("404: %+v", s)
	}
	// A good check after a failed one clears the error.
	f.publish("0.9.0", "windows-amd64", "a.exe", []byte("new"))
	if s := u.Check(context.Background()); !s.Available || s.Error != "" {
		t.Errorf("recovered: %+v", s)
	}
}

func TestDisabledBuildsNeverCheck(t *testing.T) {
	f := newFeed(t)
	f.publish("0.9.0", "windows-amd64", "a.exe", []byte("new"))
	for name, o := range map[string]Options{
		"dev":   {Current: "dev", PublicKey: f.pub, GOOS: "windows"},
		"empty": {Current: "", PublicKey: f.pub, GOOS: "windows"},
		"linux": {Current: "0.2.0", PublicKey: f.pub, GOOS: "linux"},
	} {
		o.ManifestURL = f.url()
		u := New(o)
		s := u.Check(context.Background())
		if s.Disabled == "" || s.Available || f.fetches.Load() != 0 {
			t.Errorf("%s: %+v (fetches %d)", name, s, f.fetches.Load())
		}
	}
	if s := New(Options{Current: ""}).State(); s.Current != "dev" {
		t.Errorf("current %q", s.Current)
	}
	if strings.TrimSpace(embeddedPublicKey) == "" {
		if s := New(Options{Current: "0.2.0", GOOS: "darwin"}).State(); s.Disabled == "" {
			t.Error("no embedded key, yet updates are on")
		}
	}
}

func TestWatchChecksAtStartAndEveryHalfHour(t *testing.T) {
	if CheckInterval != 30*time.Minute {
		t.Fatalf("CheckInterval = %v", CheckInterval)
	}
	f := newFeed(t)
	f.publish("0.2.1", "windows-amd64", "a.exe", []byte("new"))
	ticks := make(chan time.Time)
	var asked []time.Duration
	var mu sync.Mutex
	u := New(Options{Current: "0.2.0", ManifestURL: f.url(), PublicKey: f.pub, GOOS: "windows", Platform: "windows-amd64",
		After: func(d time.Duration) <-chan time.Time {
			mu.Lock()
			asked = append(asked, d)
			mu.Unlock()
			return ticks
		}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { u.Watch(ctx); close(done) }()
	waitFor(t, func() bool { return f.fetches.Load() == 1 })
	ticks <- time.Now()
	waitFor(t, func() bool { return f.fetches.Load() == 2 })
	ticks <- time.Now()
	waitFor(t, func() bool { return f.fetches.Load() == 3 })
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	for _, d := range asked {
		if d != CheckInterval {
			t.Errorf("waited %v between checks", d)
		}
	}
	if len(asked) != 3 {
		t.Errorf("waits %d", len(asked))
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestMenuLabels(t *testing.T) {
	for _, c := range []struct {
		s    State
		want string
	}{
		{State{}, ""},
		{State{Checking: true}, ""},
		{State{Latest: "0.2.0", Current: "0.2.0"}, ""},
		{State{Available: true, Latest: "0.3.0"}, "Update code-link to 0.3.0…"},
		{State{Available: true, Latest: "0.3.0", Job: Job{Running: true, Version: "0.3.0"}}, "Updating code-link to 0.3.0…"},
		{State{Available: true, Latest: "0.3.0", Job: Job{Phase: PhaseFailed, Version: "0.3.0"}}, "Update code-link to 0.3.0…"},
		{State{RestartNeeded: true, Latest: "0.3.0"}, "Quit and reopen code-link to finish updating"},
		{State{Disabled: "dev", Error: "x"}, ""},
	} {
		if got := MenuLabel(c.s); got != c.want {
			t.Errorf("%+v: %q, want %q", c.s, got, c.want)
		}
	}
}

// --- Plans ---

func TestWindowsPlan(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "code-link.exe")
	p, err := PlanFor("windows", exe)
	if err != nil {
		t.Fatal(err)
	}
	if p.Target != exe || p.Dir != dir || p.Old != filepath.Join(dir, "code-link.old.exe") || p.Download != filepath.Join(dir, "code-link.new.exe") {
		t.Errorf("%+v", p)
	}
	name, args := p.Relaunch(4242)
	if name != exe || len(args) != 1 || args[0] != "--update-relaunch-wait=4242" {
		t.Errorf("relaunch %s %v", name, args)
	}
}

func TestDarwinPlan(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("macOS paths")
	}
	p, err := PlanFor("darwin", "/Applications/code-link.app/Contents/MacOS/code-link")
	if err != nil {
		t.Fatal(err)
	}
	want := Plan{OS: "darwin", Target: "/Applications/code-link.app", Dir: "/Applications",
		Staging: "/Applications/.code-link-update", Download: "/Applications/.code-link-update/update.zip",
		Old: "/Applications/.code-link-update/old/code-link.app"}
	if p != want {
		t.Errorf("got %+v\nwant %+v", p, want)
	}
	name, args := p.Relaunch(7)
	if name != "/usr/bin/open" || strings.Join(args, " ") != "-n /Applications/code-link.app --args --update-relaunch-wait=7" {
		t.Errorf("relaunch %s %v", name, args)
	}
	if _, err := PlanFor("darwin", "/Users/d/go/bin/code-link"); err == nil || !strings.Contains(err.Error(), "development build") {
		t.Errorf("not a bundle: %v", err)
	}
	if _, err := PlanFor("darwin", "/private/var/folders/x/AppTranslocation/ABC/d/code-link.app/Contents/MacOS/code-link"); err == nil ||
		!strings.Contains(err.Error(), "Applications") {
		t.Errorf("translocated: %v", err)
	}
	if _, err := PlanFor("linux", "/usr/bin/code-link"); err == nil {
		t.Error("linux planned")
	}
}

func readOnlyDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a POSIX non-root user")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	return dir
}

func TestNotWritableSaysSoInPlainWords(t *testing.T) {
	dir := readOnlyDir(t)
	err := windowsPlan(filepath.Join(dir, "code-link.exe")).Prepare()
	var ue *UserError
	if !errors.As(err, &ue) || !strings.Contains(ue.Message, "isn't writable") || !strings.Contains(ue.Message, "Documents") ||
		!strings.Contains(ue.Message, dir) {
		t.Errorf("windows: %v", err)
	}
	p, _ := darwinPlan(filepath.Join(dir, "code-link.app", "Contents", "MacOS", "code-link"))
	err = p.Prepare()
	if !errors.As(err, &ue) || !strings.Contains(ue.Message, "isn't writable") || !strings.Contains(ue.Message, "Applications") {
		t.Errorf("darwin: %v", err)
	}
	dmg := Plan{OS: "darwin", Dir: "/Volumes/code-link", Staging: "/Volumes/code-link/.code-link-update"}
	if err := dmg.notWritable(os.ErrPermission); !strings.Contains(err.Error(), "disk image") {
		t.Errorf("dmg: %v", err)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

func TestWindowsInstallSwapsAndCleanup(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "code-link.exe")
	write(t, exe, "old")
	write(t, filepath.Join(dir, "code-link.json"), "{settings}")
	write(t, filepath.Join(dir, "code-link.old.exe"), "older leftover")
	p := windowsPlan(exe)
	if err := p.Prepare(); err != nil {
		t.Fatal(err)
	}
	write(t, p.Download, "new")
	if err := p.Install(nil); err != nil {
		t.Fatal(err)
	}
	if read(t, exe) != "new" || read(t, p.Old) != "old" || exists(p.Download) || read(t, filepath.Join(dir, "code-link.json")) != "{settings}" {
		t.Errorf("after install: exe %q old %q", read(t, exe), read(t, p.Old))
	}
	if err := p.Cleanup(); err != nil || exists(p.Old) || !exists(exe) {
		t.Errorf("cleanup: %v", err)
	}
}

func TestWindowsInstallPutsItselfBack(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "code-link.exe")
	write(t, exe, "old")
	p := windowsPlan(exe)
	// No download in place: the second rename fails.
	retryDelay = time.Millisecond
	t.Cleanup(func() { retryDelay = 250 * time.Millisecond })
	err := p.Install(nil)
	var ue *UserError
	if !errors.As(err, &ue) || read(t, exe) != "old" || exists(p.Old) {
		t.Errorf("err %v, exe present %v", err, exists(exe))
	}
}

// fakeBundle makes dir/name.app with an executable saying content.
func fakeBundle(t *testing.T, dir, name, content string) string {
	b := filepath.Join(dir, name)
	write(t, filepath.Join(b, "Contents", "MacOS", "code-link"), content)
	write(t, filepath.Join(b, "Contents", "Info.plist"), "plist "+content)
	return b
}

// fakeExtract stands in for ditto: the "zip" holds the version text, and
// extraction makes a code-link.app saying it.
func fakeExtract(t *testing.T) func(zip, dest string) error {
	return func(zip, dest string) error {
		b, err := os.ReadFile(zip)
		if err != nil {
			return err
		}
		fakeBundle(t, dest, "code-link.app", string(b))
		return nil
	}
}

func TestDarwinInstallSwapsBundlesAndCleanup(t *testing.T) {
	dir := t.TempDir()
	bundle := fakeBundle(t, dir, "code-link.app", "old")
	write(t, filepath.Join(bundle, "Contents", "MacOS", "code-link.json"), "{portable}")
	p, err := darwinPlan(filepath.Join(bundle, "Contents", "MacOS", "code-link"))
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Staging, "junk"), "from an earlier try")
	if err := p.Prepare(); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(p.Staging, "junk")) {
		t.Error("leftovers kept")
	}
	write(t, p.Download, "new")
	if err := p.Install(fakeExtract(t)); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(bundle, "Contents", "MacOS", "code-link")
	if read(t, exe) != "new" || read(t, filepath.Join(p.Old, "Contents", "MacOS", "code-link")) != "old" {
		t.Errorf("not swapped")
	}
	if read(t, filepath.Join(bundle, "Contents", "MacOS", "code-link.json")) != "{portable}" {
		t.Error("portable settings not carried over")
	}
	if exists(p.Download) || exists(filepath.Join(p.Staging, "new")) {
		t.Error("download or unpacked copy left")
	}
	if err := p.Cleanup(); err != nil || exists(p.Staging) || !exists(exe) {
		t.Errorf("cleanup: %v", err)
	}
}

func TestDarwinInstallKeepsOldOnBadZip(t *testing.T) {
	dir := t.TempDir()
	bundle := fakeBundle(t, dir, "code-link.app", "old")
	p, _ := darwinPlan(filepath.Join(bundle, "Contents", "MacOS", "code-link"))
	if err := p.Prepare(); err != nil {
		t.Fatal(err)
	}
	write(t, p.Download, "x")
	for name, extract := range map[string]func(string, string) error{
		"ditto fails": func(string, string) error { return errors.New("ditto: bad zip") },
		"no app":      func(_, dest string) error { write(t, filepath.Join(dest, "readme"), "x"); return nil },
		"two apps": func(_, dest string) error {
			fakeBundle(t, dest, "a.app", "1")
			fakeBundle(t, dest, "b.app", "2")
			return nil
		},
	} {
		if err := p.Install(extract); err == nil {
			t.Errorf("%s: installed", name)
		}
		if read(t, filepath.Join(bundle, "Contents", "MacOS", "code-link")) != "old" {
			t.Fatalf("%s: running bundle touched", name)
		}
	}
}

func TestRelaunchFlag(t *testing.T) {
	rest, pid, found := StripRelaunch([]string{"--update-relaunch-wait=123", "serve", "x.json"})
	if !found || pid != 123 || strings.Join(rest, " ") != "serve x.json" {
		t.Errorf("%v %d %v", rest, pid, found)
	}
	if rest, pid, found := StripRelaunch([]string{"--update-relaunch-wait=abc"}); !found || pid != 0 || len(rest) != 0 {
		t.Errorf("malformed: %v %d %v", rest, pid, found)
	}
	if _, _, found := StripRelaunch([]string{"serve"}); found {
		t.Error("found in normal args")
	}

	var waited []int
	waitForExit = func(pid int, d time.Duration) bool { waited = append(waited, pid); return true }
	cleaned := 0
	cleanupRunning = func() { cleaned++ }
	t.Cleanup(func() { waitForExit, cleanupRunning = platformWaitForExit, CleanupRunning })
	if got := AfterRelaunch([]string{"serve"}); strings.Join(got, " ") != "serve" || len(waited) != 0 {
		t.Errorf("normal start waited: %v", waited)
	}
	if cleaned != 0 {
		t.Error("normal start cleaned up")
	}
	if got := AfterRelaunch([]string{"--update-relaunch-wait=99"}); len(got) != 0 || len(waited) != 1 || waited[0] != 99 || cleaned != 1 {
		t.Errorf("relaunch: %v %v, cleaned %d", got, waited, cleaned)
	}
}

func TestWaitForExitOfGoneProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX wait")
	}
	start := time.Now()
	// A pid far above the usual range: no such process.
	if !platformWaitForExit(99999999, time.Second) || time.Since(start) > 500*time.Millisecond {
		t.Error("waited for a process that doesn't exist")
	}
	if platformWaitForExit(os.Getpid(), 150*time.Millisecond) {
		t.Error("own process reported gone")
	}
}

// --- Whole updates ---

type started struct {
	mu   sync.Mutex
	name string
	args []string
	err  error
}

func (s *started) start(name string, args []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.name, s.args = name, args
	return s.err
}

func windowsUpdater(t *testing.T, f *feed, st *started) (*Updater, string) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "code-link.exe")
	write(t, exe, "old")
	u := New(Options{Current: "0.2.0", ManifestURL: f.url(), PublicKey: f.pub, GOOS: "windows", Platform: "windows-amd64",
		Exe: func() (string, error) { return exe, nil }, Start: st.start, Pid: func() int { return 555 }})
	return u, exe
}

func runUpdate(t *testing.T, u *Updater) (State, bool) {
	t.Helper()
	restarted := make(chan struct{}, 1)
	u.Start(func() { restarted <- struct{}{} })
	waitFor(t, func() bool { return !u.State().Job.Running })
	select {
	case <-restarted:
		return u.State(), true
	default:
		return u.State(), false
	}
}

func TestWindowsUpdateEndToEnd(t *testing.T) {
	f := newFeed(t)
	f.publish("0.2.1", "windows-amd64", "code-link-0.2.1-windows-amd64.exe", []byte("new exe"))
	st := &started{}
	u, exe := windowsUpdater(t, f, st)
	u.Check(context.Background())
	s, restarted := runUpdate(t, u)
	if !restarted || s.Job.Phase != PhaseRestart || s.Job.Message != "" || s.Job.Received != int64(len("new exe")) {
		t.Fatalf("job %+v restarted %v", s.Job, restarted)
	}
	if read(t, exe) != "new exe" || read(t, filepath.Join(filepath.Dir(exe), "code-link.old.exe")) != "old" {
		t.Error("not swapped")
	}
	if st.name != exe || strings.Join(st.args, " ") != "--update-relaunch-wait=555" {
		t.Errorf("started %s %v", st.name, st.args)
	}
}

func TestDarwinUpdateEndToEnd(t *testing.T) {
	f := newFeed(t)
	f.publish("0.2.1", "darwin-arm64", "code-link-0.2.1-macos-arm64.zip", []byte("new app"))
	dir := t.TempDir()
	bundle := fakeBundle(t, dir, "code-link.app", "old")
	st := &started{}
	u := New(Options{Current: "0.2.0", ManifestURL: f.url(), PublicKey: f.pub, GOOS: "darwin", Platform: "darwin-arm64",
		Exe:     func() (string, error) { return filepath.Join(bundle, "Contents", "MacOS", "code-link"), nil },
		Extract: fakeExtract(t), Start: st.start, Pid: func() int { return 9 }})
	u.Check(context.Background())
	s, restarted := runUpdate(t, u)
	if !restarted || s.Job.Phase != PhaseRestart {
		t.Fatalf("job %+v", s.Job)
	}
	if read(t, filepath.Join(bundle, "Contents", "MacOS", "code-link")) != "new app" {
		t.Error("not swapped")
	}
	if st.name != "/usr/bin/open" || strings.Join(st.args, " ") != "-n "+bundle+" --args --update-relaunch-wait=9" {
		t.Errorf("started %s %v", st.name, st.args)
	}
}

func TestDownloadsThatDontMatchAreRefused(t *testing.T) {
	cases := map[string]func(f *feed){
		"wrong sha": func(f *feed) {
			f.lie = func(w http.ResponseWriter, r *http.Request) bool { w.Write([]byte("evil!!!")); return true }
		},
		"too short": func(f *feed) {
			f.lie = func(w http.ResponseWriter, r *http.Request) bool { w.Write([]byte("new")); return true }
		},
		"too long, no length": func(f *feed) {
			f.lie = func(w http.ResponseWriter, r *http.Request) bool {
				w.Header().Set("Transfer-Encoding", "chunked")
				w.Write([]byte("new exe and more"))
				w.(http.Flusher).Flush()
				return true
			}
		},
		"manifest size wrong": func(f *feed) {
			var m Manifest
			json.Unmarshal(f.manifest, &m)
			file := m.Files["windows-amd64"]
			file.Size++
			m.Files["windows-amd64"] = file
			f.manifest = manifestBytes(f.t, m)
			f.sig = Sign(f.manifest, f.priv)
		},
	}
	for name, lie := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFeed(t)
			f.publish("0.2.1", "windows-amd64", "n.exe", []byte("new exe"))
			lie(f)
			st := &started{}
			u, exe := windowsUpdater(t, f, st)
			u.Check(context.Background())
			s, restarted := runUpdate(t, u)
			if restarted || s.Job.Phase != PhaseFailed || !strings.Contains(s.Job.Message, "didn't match the published update") {
				t.Fatalf("job %+v", s.Job)
			}
			if read(t, exe) != "old" || exists(filepath.Join(filepath.Dir(exe), "code-link.new.exe")) || st.name != "" {
				t.Error("something was replaced, left or started")
			}
			if !s.Available || MenuLabel(s) != "Update code-link to 0.2.1…" {
				t.Errorf("update no longer offered after a failure: %+v", s)
			}
		})
	}
}

func TestNetworkFailureSaysSo(t *testing.T) {
	f := newFeed(t)
	f.publish("0.2.1", "windows-amd64", "n.exe", []byte("new exe"))
	f.lie = func(w http.ResponseWriter, r *http.Request) bool { http.Error(w, "no", 502); return true }
	u, exe := windowsUpdater(t, f, &started{})
	u.Check(context.Background())
	s, _ := runUpdate(t, u)
	if s.Job.Phase != PhaseFailed || !strings.Contains(s.Job.Message, "internet connection") || !strings.Contains(s.Job.Detail, "502") {
		t.Errorf("job %+v", s.Job)
	}
	if read(t, exe) != "old" {
		t.Error("replaced")
	}
}

func TestCancelDuringDownload(t *testing.T) {
	f := newFeed(t)
	f.publish("0.2.1", "windows-amd64", "n.exe", []byte("new exe"))
	release := make(chan struct{})
	f.lie = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Length", "7")
		w.Write([]byte("new"))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
		return true
	}
	defer close(release)
	st := &started{}
	u, exe := windowsUpdater(t, f, st)
	u.Check(context.Background())
	u.Start(nil)
	waitFor(t, func() bool { return u.State().Job.Running })
	u.Cancel()
	waitFor(t, func() bool { return !u.State().Job.Running })
	s := u.State()
	if s.Job.Phase != PhaseCancelled || read(t, exe) != "old" || exists(filepath.Join(filepath.Dir(exe), "code-link.new.exe")) || st.name != "" {
		t.Errorf("job %+v", s.Job)
	}
}

func TestRestartFailureKeepsTheInstall(t *testing.T) {
	f := newFeed(t)
	f.publish("0.2.1", "windows-amd64", "n.exe", []byte("new exe"))
	st := &started{err: errors.New("access denied")}
	u, exe := windowsUpdater(t, f, st)
	u.Check(context.Background())
	s, restarted := runUpdate(t, u)
	if restarted || s.Job.Phase != PhaseFailed || !s.RestartNeeded || s.Available || !strings.Contains(s.Job.Message, "Quit code-link and open it again") {
		t.Fatalf("state %+v", s)
	}
	if read(t, exe) != "new exe" {
		t.Error("install rolled back")
	}
	if MenuLabel(s) != "Quit and reopen code-link to finish updating" {
		t.Errorf("menu %q", MenuLabel(s))
	}
}

func TestStartWithoutUpdateOrInReadOnlyFolder(t *testing.T) {
	f := newFeed(t)
	u, _ := windowsUpdater(t, f, &started{})
	s, _ := runUpdate(t, u) // never checked
	if s.Job.Phase != PhaseFailed || !strings.Contains(s.Job.Message, "no newer code-link") {
		t.Errorf("job %+v", s.Job)
	}

	dir := readOnlyDir(t)
	f.publish("0.2.1", "windows-amd64", "n.exe", []byte("new exe"))
	ro := New(Options{Current: "0.2.0", ManifestURL: f.url(), PublicKey: f.pub, GOOS: "windows", Platform: "windows-amd64",
		Exe: func() (string, error) { return filepath.Join(dir, "code-link.exe"), nil }, Start: (&started{}).start})
	ro.Check(context.Background())
	s, _ = runUpdate(t, ro)
	if s.Job.Phase != PhaseFailed || !strings.Contains(s.Job.Message, "isn't writable") {
		t.Errorf("job %+v", s.Job)
	}
}

func TestResolveURL(t *testing.T) {
	base := "https://codelink.eidonix.com/update/manifest.json"
	for ref, want := range map[string]string{
		"/files/a.zip":                      "https://codelink.eidonix.com/files/a.zip",
		"../files/a.zip":                    "https://codelink.eidonix.com/files/a.zip",
		"https://other.example/files/a.zip": "https://other.example/files/a.zip",
	} {
		if got, err := resolveURL(base, ref); err != nil || got != want {
			t.Errorf("%s: %s %v", ref, got, err)
		}
	}
	if _, err := resolveURL(base, "file:///etc/passwd"); err == nil {
		t.Error("file URL allowed")
	}
}
