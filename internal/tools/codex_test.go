package tools

import (
	"archive/tar"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCodexTargets(t *testing.T) {
	for _, c := range []struct{ goos, goarch, want string }{
		{"darwin", "arm64", "aarch64-apple-darwin"},
		{"darwin", "amd64", "x86_64-apple-darwin"},
		{"windows", "amd64", "x86_64-pc-windows-msvc"},
		{"windows", "arm64", "aarch64-pc-windows-msvc"},
		{"linux", "amd64", "x86_64-unknown-linux-musl"},
		{"linux", "arm64", "aarch64-unknown-linux-musl"},
	} {
		if got, err := codexTarget(c.goos, c.goarch); err != nil || got != c.want {
			t.Errorf("%s/%s: %q %v", c.goos, c.goarch, got, err)
		}
	}
	if _, err := codexTarget("windows", "386"); err == nil {
		t.Error("no 32-bit build")
	}
}

func TestCodexAssetSelection(t *testing.T) {
	f := newFeeds(t)
	targets := []string{"aarch64-apple-darwin", "x86_64-apple-darwin", "x86_64-pc-windows-msvc", "aarch64-pc-windows-msvc"}
	f.publish(t, "0.163.0", targets, nil)
	rel := f.release(t)
	for _, target := range targets {
		pkg, sums, err := codexAssets(rel, target)
		if err != nil || pkg.Name != "codex-package-"+target+".tar.gz" || sums.Name != sumsAsset {
			t.Errorf("%s: %q %q %v", target, pkg.Name, sums.Name, err)
		}
	}
	if _, _, err := codexAssets(rel, "x86_64-unknown-linux-musl"); err == nil {
		t.Error("no package for the target is an error")
	}
}

func TestChecksumFiles(t *testing.T) {
	sums := []byte("aaaa  other.tar.gz\n" + strings.Repeat("ab", 32) + "  codex-package-x.tar.gz\n" + strings.Repeat("CD", 32) + " *codex-package-y.tar.gz\n")
	if got := sumFor(sums, "codex-package-x.tar.gz"); got != strings.Repeat("ab", 32) {
		t.Error(got)
	}
	if got := sumFor(sums, "codex-package-y.tar.gz"); got != strings.Repeat("cd", 32) {
		t.Error(got)
	}
	if sumFor(sums, "codex-package-z.tar.gz") != "" || sumFor(sums, "other.tar.gz") != "" {
		t.Error("only well-formed lines for the name")
	}
	if sha256Of("sha256:"+strings.Repeat("0f", 32)) == "" || sha256Of("sha1:abc") != "" || sha256Of("") != "" {
		t.Error("digest parsing")
	}
}

func target(t *testing.T, m *Manager) string {
	tg, err := codexTarget(m.Env.GOOS, m.Env.GOARCH)
	if err != nil {
		t.Skip(err)
	}
	return tg
}

func TestInstallsAndUpdatesCodexFromGitHub(t *testing.T) {
	needsUnix(t)
	f := newFeeds(t)
	m, _ := testManager(t, f)
	tg := target(t, m)
	f.publish(t, "0.163.0", []string{tg, "x86_64-pc-windows-msvc"}, nil)
	ctx := context.Background()

	progress, lines := collect()
	if err := m.Install(ctx, Codex, progress); err != nil {
		t.Fatal(err)
	}
	home := m.Env.Home
	cmd := filepath.Join(home, ".local", "bin", "codex")
	if target, err := os.Readlink(cmd); err != nil || target != filepath.Join(m.codexDir(), "current", "bin", "codex") {
		t.Errorf("~/.local/bin/codex → %q %v", target, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "codex-code-mode-host")); err != nil {
		t.Error("codex-code-mode-host is linked next to it")
	}
	for _, want := range []string{"Looking up", "Downloading Codex 0.163.0 for " + tg, "Checking the download", "Unpacking", "Codex 0.163.0 is installed"} {
		if !hasLine(lines(), want) {
			t.Errorf("no %q in %q", want, lines())
		}
	}
	s := m.Status(ctx, Codex)
	if !s.Installed || s.Path != cmd || s.Method != GitHub || s.Version != "0.163.0" || s.Latest != "0.163.0" || s.UpdateAvailable || !s.CanUpdate {
		t.Errorf("after install: %+v", s)
	}
	for _, p := range []string{"codex-resources", "codex-path/rg", "codex-package.json"} {
		if _, err := os.Stat(filepath.Join(m.codexDir(), "versions", "0.163.0", p)); err != nil {
			t.Errorf("the whole package is installed: %v", err)
		}
	}

	// Up to date: nothing is downloaded.
	pkgPath := "/dl/rust-v0.163.0/codex-package-" + tg + ".tar.gz"
	progress, lines = collect()
	if err := m.Update(ctx, Codex, progress); err != nil || f.count(pkgPath) != 1 || !hasLine(lines(), "up to date (0.163.0)") {
		t.Errorf("up to date: %v, %d downloads, %q", err, f.count(pkgPath), lines())
	}

	// A new release: switched to, the previous one kept.
	f.publish(t, "0.164.0", []string{tg}, nil)
	m.cache = nil
	if s := m.Status(ctx, Codex); !s.UpdateAvailable || s.Latest != "0.164.0" {
		t.Errorf("outdated: %+v", s)
	}
	if err := m.Update(ctx, Codex, nil); err != nil {
		t.Fatal(err)
	}
	if v, _ := readVersion(ctx, cmd); v != "0.164.0" {
		t.Errorf("runs %q", v)
	}
	f.publish(t, "0.165.0", []string{tg}, nil)
	if err := m.Update(ctx, Codex, nil); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(m.codexDir(), "versions"))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !reflect.DeepEqual(names, []string{"0.164.0", "0.165.0"}) {
		t.Errorf("versions kept: %q", names)
	}
}

func TestCodexChecksumMismatchIsRefused(t *testing.T) {
	needsUnix(t)
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, f *feeds, tg string)
		want  string
	}{
		{"package doesn't match the sums", func(t *testing.T, f *feeds, tg string) {
			f.publish(t, "0.163.0", []string{tg}, map[string]string{"codex-package-" + tg + ".tar.gz": strings.Repeat("0", 64)})
			// GitHub's digest for the package agrees with the bad sum, so the download is what fails.
			r := f.release(t)
			for i := range r.Assets {
				if r.Assets[i].Name == "codex-package-"+tg+".tar.gz" {
					r.Assets[i].Digest = "sha256:" + strings.Repeat("0", 64)
				}
			}
			f.setRelease(r)
		}, "didn't match the checksum OpenAI published"},
		{"sums and GitHub disagree", func(t *testing.T, f *feeds, tg string) {
			f.publish(t, "0.163.0", []string{tg}, map[string]string{"codex-package-" + tg + ".tar.gz": strings.Repeat("0", 64)})
		}, "checksums disagree"},
		{"sums file tampered", func(t *testing.T, f *feeds, tg string) {
			f.publish(t, "0.163.0", []string{tg}, nil)
			f.set("/dl/rust-v0.163.0/"+sumsAsset, strings.Repeat("1", 64)+"  codex-package-"+tg+".tar.gz\n")
		}, "checksum file didn't match its GitHub digest"},
		{"no checksum at all", func(t *testing.T, f *feeds, tg string) {
			f.publish(t, "0.163.0", []string{tg}, nil)
			r := f.release(t)
			var keep []asset
			for _, a := range r.Assets {
				if a.Name != sumsAsset {
					a.Digest = ""
					keep = append(keep, a)
				}
			}
			r.Assets = keep
			f.setRelease(r)
		}, "publishes no checksum"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFeeds(t)
			m, _ := testManager(t, f)
			c.setup(t, f, target(t, m))
			err := m.Install(context.Background(), Codex, nil)
			var te *Error
			if !errors.As(err, &te) || !strings.Contains(te.Message, c.want) || !errors.Is(err, errChecksum) {
				t.Fatalf("%v", err)
			}
			if _, err := os.Lstat(filepath.Join(m.Env.Home, ".local", "bin", "codex")); err == nil {
				t.Error("nothing is installed")
			}
			if entries, _ := os.ReadDir(filepath.Join(m.codexDir(), "versions")); len(entries) != 0 {
				t.Errorf("left %v", entries)
			}
			if s := m.Status(context.Background(), Codex); s.Installed {
				t.Error("still not installed")
			}
		})
	}
}

func TestCodexWontReplaceSomeoneElsesCommand(t *testing.T) {
	needsUnix(t)
	f := newFeeds(t)
	m, _ := testManager(t, f)
	f.publish(t, "0.163.0", []string{target(t, m)}, nil)
	mine := filepath.Join(m.Env.Home, ".local", "bin", "codex")
	script(t, mine, `echo "codex-cli 0.1.0"`)
	err := m.installCodex(context.Background(), func(string) {})
	if err == nil || !strings.Contains(err.Error(), "isn't code-link's") {
		t.Fatalf("%v", err)
	}
	if b, _ := os.ReadFile(mine); !strings.Contains(string(b), "0.1.0") {
		t.Error("it was replaced")
	}
	// A dangling link (an install that was deleted) is fine to replace.
	os.Remove(mine)
	symlink(t, filepath.Join(m.Env.Home, "gone", "codex"), mine)
	if err := m.installCodex(context.Background(), func(string) {}); err != nil {
		t.Fatal(err)
	}
}

func TestCodexDownloadCancellation(t *testing.T) {
	needsUnix(t)
	f := newFeeds(t)
	m, _ := testManager(t, f)
	tg := target(t, m)
	f.publish(t, "0.163.0", []string{tg}, nil)
	f.slow = "/dl/rust-v0.163.0/codex-package-" + tg + ".tar.gz"
	ctx, cancel := context.WithCancel(context.Background())
	progress, lines := collect()
	go func() {
		for !hasLine(lines(), "Downloading") {
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := m.Install(ctx, Codex, progress)
	if !errors.Is(err, context.Canceled) || err.Error() != "Installing Codex was cancelled." {
		t.Fatalf("%v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("slow to stop")
	}
	if entries, _ := os.ReadDir(m.codexDir()); len(entries) != 1 { // versions/ only, no download left
		t.Errorf("left %v", entries)
	}
}

func TestCodexUpdateFallsBackWhenTheNewVersionDoesntRun(t *testing.T) {
	needsUnix(t)
	f := newFeeds(t)
	m, _ := testManager(t, f)
	tg := target(t, m)
	f.publish(t, "0.163.0", []string{tg}, nil)
	if err := m.Install(context.Background(), Codex, nil); err != nil {
		t.Fatal(err)
	}
	f.publish(t, "0.164.0", []string{tg}, nil)
	real := m.Version
	m.Version = func(ctx context.Context, p string) (string, error) {
		v, err := real(ctx, p)
		if v == "0.164.0" {
			return "", errors.New("killed: 9")
		}
		return v, err
	}
	err := m.Update(context.Background(), Codex, nil)
	if err == nil || !strings.Contains(err.Error(), "was unpacked but doesn't run") {
		t.Fatalf("%v", err)
	}
	if v, _ := readVersion(context.Background(), filepath.Join(m.Env.Home, ".local", "bin", "codex")); v != "0.163.0" {
		t.Errorf("not switched back: %q", v)
	}
}

func TestExtractionRefusesUnsafeEntries(t *testing.T) {
	for _, c := range []struct {
		name  string
		entry tarEntry
	}{
		{"parent", tarEntry{name: "../evil", body: "x", mode: 0o644}},
		{"nested parent", tarEntry{name: "bin/../../evil", body: "x", mode: 0o644}},
		{"absolute", tarEntry{name: "/tmp/evil", body: "x", mode: 0o644}},
		{"backslash", tarEntry{name: `..\evil`, body: "x", mode: 0o644}},
		{"symlink", tarEntry{name: "bin/codex", typ: tar.TypeSymlink, link: "/bin/sh"}},
		{"hard link", tarEntry{name: "bin/codex", typ: tar.TypeLink, link: "../../x"}},
	} {
		dir := t.TempDir()
		archive := filepath.Join(dir, "p.tar.gz")
		os.WriteFile(archive, tarGz(t, []tarEntry{{name: "bin", dir: true}, c.entry}), 0o600)
		dest := filepath.Join(dir, "out", "pkg")
		if err := extractTarGz(archive, dest); err == nil {
			t.Errorf("%s: extracted", c.name)
		}
		if _, err := os.Stat(filepath.Join(dir, "out", "evil")); err == nil {
			t.Errorf("%s: wrote outside", c.name)
		}
	}
}

func TestPackageCheck(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "p.tar.gz")
	os.WriteFile(archive, codexPackage(t, "0.163.0", "x86_64-pc-windows-msvc"), 0o600)
	if err := extractTarGz(archive, filepath.Join(dir, "w")); err != nil {
		t.Fatal(err)
	}
	if err := checkPackage(filepath.Join(dir, "w"), "x86_64-pc-windows-msvc", true); err != nil {
		t.Error(err)
	}
	if err := checkPackage(filepath.Join(dir, "w"), "aarch64-pc-windows-msvc", true); err == nil {
		t.Error("another target's package")
	}
	if err := checkPackage(dir, "x86_64-pc-windows-msvc", true); err == nil {
		t.Error("not a package")
	}
}

func TestRunStreamsCleanLinesAndKeepsTheTail(t *testing.T) {
	needsUnix(t)
	dir := t.TempDir()
	sh := filepath.Join(dir, "s")
	script(t, sh, `printf 'one\r\033[32mtwo\033[0m\n\n  three  \n'; echo err >&2; i=0; while [ $i -lt 20 ]; do echo "line $i"; i=$((i+1)); done; exit 3`)
	progress, lines := collect()
	tail, err := runCmd(context.Background(), Cmd{Path: sh}, progress)
	got := lines()
	if !reflect.DeepEqual(got[:4], []string{"one", "two", "three", "err"}) || len(got) != 24 {
		t.Errorf("lines %q", got)
	}
	if len(tail) != tailLines || tail[len(tail)-1] != "line 19" {
		t.Errorf("tail %q", tail)
	}
	e := failed("The thing failed", tail, err)
	if e.(*Error).Message != "The thing failed (exit 3)." {
		t.Errorf("%v", e)
	}
	// Extra environment replaces inherited entries.
	script(t, sh, `echo "$CODEX_NON_INTERACTIVE $HOME"`)
	progress, lines = collect()
	if _, err := runCmd(context.Background(), Cmd{Path: sh, Env: []string{"CODEX_NON_INTERACTIVE=1", "HOME=/nowhere"}}, progress); err != nil || lines()[0] != "1 /nowhere" {
		t.Errorf("%v %q", err, lines())
	}
	if _, err := runCmd(context.Background(), Cmd{Path: filepath.Join(dir, "missing")}, nil); err == nil {
		t.Error("a missing command is an error")
	}
}

// Cancelling an install kills the installer and everything it started
// (here a "login shell" that starts a background child and waits).
func TestCancellingAnInstallKillsTheInstaller(t *testing.T) {
	needsUnix(t)
	f := newFeeds(t)
	m, root := testManager(t, f)
	pidFile := filepath.Join(root, "child.pid")
	shell := filepath.Join(root, "fakeshell")
	script(t, shell, `sleep 30 & echo $! > `+pidFile+`; echo started; wait`)
	m.Env.Shell = shell
	ctx, cancel := context.WithCancel(context.Background())
	progress, lines := collect()
	go func() {
		for !hasLine(lines(), "started") {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	start := time.Now()
	err := m.Install(ctx, ClaudeCode, progress)
	if !errors.Is(err, context.Canceled) || !strings.HasPrefix(err.Error(), "Installing Claude Code was cancelled.") {
		t.Fatalf("%v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("slow to stop")
	}
	b, _ := os.ReadFile(pidFile)
	pid := strings.TrimSpace(string(b))
	time.Sleep(100 * time.Millisecond)
	if pid == "" || processAlive(pid) {
		t.Errorf("child %q still running", pid)
	}
}

func TestReadVersion(t *testing.T) {
	needsUnix(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "claude")
	script(t, p, `echo "2.1.296 (Claude Code)"`)
	if v, err := readVersion(context.Background(), p); v != "2.1.296" || err != nil {
		t.Errorf("%q %v", v, err)
	}
	script(t, p, `echo nope`)
	if _, err := readVersion(context.Background(), p); err == nil {
		t.Error("no version is an error")
	}
	if _, err := readVersion(context.Background(), filepath.Join(dir, "missing")); err == nil {
		t.Error("missing")
	}
}
