package tools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"codelink/internal/launch"
)

// feeds is a fake of every official source: Anthropic's release feed, the
// npm registry, GitHub's API and its downloads.
type feeds struct {
	srv *httptest.Server
	mu  sync.Mutex
	// routes are path → reply body (status 200); a missing path is a 404.
	routes map[string][]byte
	hits   map[string]int
	// slow, when set, is a path whose reply stalls after half the body
	// until the request is cancelled.
	slow string
}

func newFeeds(t *testing.T) *feeds {
	f := &feeds{routes: map[string][]byte{}, hits: map[string]int{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		body, ok := f.routes[r.URL.Path]
		f.hits[r.URL.Path]++
		slow := f.slow == r.URL.Path
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		if slow {
			w.Write(body[:len(body)/2])
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		w.Write(body)
	}))
	t.Cleanup(f.srv.Close)
	f.set("/claude/latest", "2.1.304\n")
	f.set("/claude/stable", "2.1.290\n")
	f.set("/npm/@anthropic-ai/claude-code/latest", `{"name":"@anthropic-ai/claude-code","version":"2.1.304"}`)
	f.set("/npm/@anthropic-ai/claude-code/stable", `{"version":"2.1.290"}`)
	f.set("/npm/@openai/codex/latest", `{"version":"0.163.0"}`)
	return f
}

func (f *feeds) set(path, body string) { f.setBytes(path, []byte(body)) }

func (f *feeds) setBytes(path string, body []byte) {
	f.mu.Lock()
	f.routes[path] = body
	f.mu.Unlock()
}

func (f *feeds) remove(path string) {
	f.mu.Lock()
	delete(f.routes, path)
	f.mu.Unlock()
}

func (f *feeds) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[path]
}

func (f *feeds) sources() Sources {
	return Sources{
		ClaudeReleases:   f.srv.URL + "/claude",
		ClaudeInstallSh:  "https://claude.ai/install.sh",
		ClaudeInstallPS1: "https://claude.ai/install.ps1",
		NPMRegistry:      f.srv.URL + "/npm",
		GitHubAPI:        f.srv.URL + "/gh",
		CodexRepo:        "openai/codex",
		CodexInstallSh:   "https://chatgpt.com/codex/install.sh",
		CodexInstallPS1:  "https://chatgpt.com/codex/install.ps1",
	}
}

const releasePath = "/gh/repos/openai/codex/releases/latest"

// publish makes version the latest Codex release, with packages for the
// given targets (and decoys: the bazel build, the bare binary, the zst).
// The checksum file lists sums; nil = the real ones.
func (f *feeds) publish(t *testing.T, version string, targets []string, sums map[string]string) {
	t.Helper()
	tag := "rust-v" + version
	var assets []map[string]any
	add := func(name string, body []byte) {
		f.setBytes("/dl/"+tag+"/"+name, body)
		assets = append(assets, map[string]any{"name": name, "size": len(body),
			"browser_download_url": f.srv.URL + "/dl/" + tag + "/" + name, "digest": "sha256:" + hexSum(body)})
	}
	var lines []string
	for _, target := range targets {
		pkg := codexPackage(t, version, target)
		name := "codex-package-" + target + ".tar.gz"
		add(name, pkg)
		sum := hexSum(pkg)
		if s, ok := sums[name]; ok {
			sum = s
		}
		lines = append(lines, sum+"  "+name)
		add("codex-package-"+target+"-bazel.tar.gz", []byte("decoy"))
		add("codex-"+target+".tar.gz", []byte("decoy"))
		add("codex-package-"+target+".tar.zst", []byte("decoy"))
	}
	add(sumsAsset, []byte(strings.Join(lines, "\n")+"\n"))
	b, _ := json.Marshal(map[string]any{"tag_name": tag, "name": version, "draft": false, "prerelease": false, "assets": assets})
	f.setBytes(releasePath, b)
}

// release is the published release JSON, for tests that edit it.
func (f *feeds) release(t *testing.T) release {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var r release
	if err := json.Unmarshal(f.routes[releasePath], &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *feeds) setRelease(r release) {
	b, _ := json.Marshal(r)
	f.setBytes(releasePath, b)
}

// codexPackage is a codex-package tar.gz whose bin/codex is a script
// printing "codex-cli <version>".
func codexPackage(t *testing.T, version, target string) []byte {
	t.Helper()
	windows := strings.Contains(target, "windows")
	exe := ""
	if windows {
		exe = ".exe"
	}
	meta, _ := json.Marshal(map[string]any{"layoutVersion": 1, "version": version, "target": target,
		"variant": "codex", "entrypoint": "bin/codex" + exe, "resourcesDir": "codex-resources", "pathDir": "codex-path"})
	entries := []tarEntry{
		{name: "bin", dir: true},
		{name: "bin/codex" + exe, body: "#!/bin/sh\necho \"codex-cli " + version + "\"\n", mode: 0o755},
		{name: "bin/codex-code-mode-host" + exe, body: "#!/bin/sh\n", mode: 0o755},
		{name: "codex-path", dir: true},
		{name: "codex-path/rg" + exe, body: "rg", mode: 0o755},
		{name: "codex-resources", dir: true},
		{name: "codex-package.json", body: string(meta), mode: 0o644},
	}
	if windows {
		entries = append(entries, tarEntry{name: "codex-resources/codex-command-runner.exe", body: "x", mode: 0o755},
			tarEntry{name: "codex-resources/codex-windows-sandbox-setup.exe", body: "x", mode: 0o755})
	}
	return tarGz(t, entries)
}

type tarEntry struct {
	name, body, link string
	dir              bool
	mode             int64
	typ              byte
}

func tarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: e.mode, Size: int64(len(e.body)), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)}
		switch {
		case e.typ != 0:
			h.Typeflag, h.Linkname, h.Size = e.typ, e.link, 0
		case e.dir:
			h.Typeflag, h.Mode, h.Size = tar.TypeDir, 0o755, 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// testManager is a Manager on this machine with a temp HOME, the fake
// feeds, detection that looks only inside the temp folder, and the real
// process runner and version reader (which run the fake scripts).
func testManager(t *testing.T, f *feeds) (*Manager, string) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	os.MkdirAll(home, 0o755)
	env := Env{GOOS: runtime.GOOS, GOARCH: "arm64", Home: home, Shell: "/bin/sh", PowerShell: "powershell.exe"}
	if runtime.GOOS == "windows" {
		env.LocalAppData = filepath.Join(home, "AppData", "Local")
		env.AppData = filepath.Join(home, "AppData", "Roaming")
	}
	m := &Manager{Env: env, HTTP: f.srv.Client(), Sources: f.sources(), Run: runCmd, Version: readVersion,
		Exists: exists, Link: linkDir, FreshPath: func() string { return "" }}
	m.HTTP.CheckRedirect = httpsOnlyRedirects
	m.Find = func(tool Tool) (launch.Found, bool) {
		se := env.search()
		se.Exists = func(p string) bool {
			if !strings.HasPrefix(p, root) {
				return false // never the real installs
			}
			st, err := os.Stat(p)
			return err == nil && !st.IsDir()
		}
		return launch.Search(tool, se)
	}
	return m, root
}

// script writes an executable shell script.
func script(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(link), 0o755)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func needsUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runs shell scripts")
	}
}

// recorder is a fake Run that records commands.
type recorder struct {
	mu   sync.Mutex
	cmds []Cmd
	// do runs for each command (simulating its effect); nil = succeed.
	do func(Cmd) ([]string, error)
}

func (r *recorder) run(ctx context.Context, c Cmd, progress func(string)) ([]string, error) {
	r.mu.Lock()
	r.cmds = append(r.cmds, c)
	r.mu.Unlock()
	if r.do != nil {
		return r.do(c)
	}
	return nil, nil
}

type exitErr int

func (e exitErr) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitErr) ExitCode() int { return int(e) }

func collect() (func(string), func() []string) {
	var mu sync.Mutex
	var lines []string
	return func(s string) {
			mu.Lock()
			lines = append(lines, s)
			mu.Unlock()
		}, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), lines...)
		}
}

func hasLine(lines []string, sub string) bool {
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}
