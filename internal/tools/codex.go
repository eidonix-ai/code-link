package tools

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Codex from GitHub. Each release (github.com/openai/codex, tag
// rust-v<version>) has, per target, a codex-package-<target>.tar.gz: the
// canonical standalone package OpenAI's own installers use —
//
//	codex-package.json
//	bin/codex[.exe], bin/codex-code-mode-host[.exe]
//	codex-resources/ (Windows: codex-command-runner.exe, codex-windows-sandbox-setup.exe)
//	codex-path/rg[.exe]
//
// (the bare codex-<target> binaries lack the helpers the Windows sandbox
// needs). Verification, as OpenAI's installer does it: the release's
// codex-package_SHA256SUMS lists each package's SHA-256; that file is
// itself checked against the SHA-256 digest GitHub computed when it was
// uploaded (the API's "digest"), and so is the package. A mismatch, or a
// release with no checksum at all, refuses the install. (Only Linux's bwrap
// helper has Sigstore bundles; the packages have none.)
//
// Layout: launch.CodexDir/versions/<version> is the unpacked package,
// launch.CodexDir/current points at the one in use (a symlink; a junction on
// Windows, which needs no admin rights). macOS: ~/.local/bin/codex and
// codex-code-mode-host link to current/bin, as OpenAI's installer links
// them. Windows: current\bin goes on the user's PATH (HKCU\Environment).
// Switching current is what makes an update take: a running codex keeps
// its version's files.

const sumsAsset = "codex-package_SHA256SUMS"

// codexTarget is the release target for an OS and native architecture.
func codexTarget(goos, goarch string) (string, error) {
	switch goos + "/" + goarch {
	case "darwin/arm64":
		return "aarch64-apple-darwin", nil
	case "darwin/amd64":
		return "x86_64-apple-darwin", nil
	case "windows/amd64":
		return "x86_64-pc-windows-msvc", nil
	case "windows/arm64":
		return "aarch64-pc-windows-msvc", nil
	case "linux/amd64":
		return "x86_64-unknown-linux-musl", nil
	case "linux/arm64":
		return "aarch64-unknown-linux-musl", nil
	}
	return "", fmt.Errorf("Codex has no build for %s/%s", goos, goarch)
}

// codexAssets picks the target's package and the checksum file (zero when
// the release has none).
func codexAssets(rel release, target string) (pkg, sums asset, err error) {
	want := "codex-package-" + target + ".tar.gz"
	for _, a := range rel.Assets {
		switch a.Name {
		case want:
			pkg = a
		case sumsAsset:
			sums = a
		}
	}
	if pkg.Name == "" {
		return pkg, sums, fmt.Errorf("the release %s has no %s", rel.TagName, want)
	}
	return pkg, sums, nil
}

// sha256Of is the hex of GitHub's "sha256:<hex>" digest, "" for none.
func sha256Of(digest string) string {
	h, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || len(h) != 64 {
		return ""
	}
	if _, err := hex.DecodeString(h); err != nil {
		return ""
	}
	return strings.ToLower(h)
}

// sumFor finds name's SHA-256 in a sha256sum-format file.
func sumFor(sums []byte, name string) string {
	sc := bufio.NewScanner(strings.NewReader(string(sums)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name && len(f[0]) == 64 {
			if _, err := hex.DecodeString(f[0]); err == nil {
				return strings.ToLower(f[0])
			}
		}
	}
	return ""
}

var errChecksum = errors.New("checksum mismatch")

// expectedDigest is the package's SHA-256: from the checksum file (which
// must match its GitHub digest), cross-checked with the package's GitHub
// digest.
func (m *Manager) expectedDigest(ctx context.Context, version string, pkg, sums asset) (string, error) {
	api := sha256Of(pkg.Digest)
	listed := ""
	if sums.URL != "" {
		b, err := m.get(ctx, sums.URL, "application/octet-stream")
		if err != nil {
			return "", &Error{Message: fmt.Sprintf("Couldn't download Codex's checksums from GitHub: %v.", err), Err: err}
		}
		if d := sha256Of(sums.Digest); d != "" && hexSum(b) != d {
			return "", &Error{Message: "Codex's checksum file didn't match its GitHub digest, so Codex wasn't installed.", Err: errChecksum}
		}
		listed = sumFor(b, pkg.Name)
	}
	switch {
	case listed != "" && api != "" && listed != api:
		return "", &Error{Message: fmt.Sprintf("Codex's published checksums disagree about %s, so Codex wasn't installed.", pkg.Name), Err: errChecksum}
	case listed != "":
		return listed, nil
	case api != "":
		return api, nil
	}
	return "", &Error{Message: fmt.Sprintf("Codex %s publishes no checksum for %s, so code-link won't install it.", version, pkg.Name), Err: errChecksum}
}

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// installCodex installs the latest release.
func (m *Manager) installCodex(ctx context.Context, progress func(string)) error {
	return m.installCodexRelease(ctx, "", progress)
}

// updateCodex updates the way Codex was installed.
func (m *Manager) updateCodex(ctx context.Context, in install, progress func(string)) error {
	switch in.method {
	case GitHub:
		current, _ := m.Version(ctx, in.path)
		return m.installCodexRelease(ctx, current, progress)
	case Standalone:
		progress("Running OpenAI's Codex installer…")
		var c Cmd
		if m.Env.windows() {
			c = m.powerShellCmd("$ErrorActionPreference = 'Stop'; irm " + m.Sources.CodexInstallPS1 + " | iex")
		} else {
			c = m.shellCmd("set -o pipefail; curl -fsSL " + m.Sources.CodexInstallSh + " | sh")
		}
		c.Env = []string{"CODEX_NON_INTERACTIVE=1"}
		tail, err := m.Run(ctx, c, progress)
		if err != nil {
			return failed("OpenAI's Codex installer failed", tail, err)
		}
	case NPM, Brew, WinGet:
		if err := m.updateWithPackageManager(ctx, Codex, in, progress); err != nil {
			return err
		}
	default:
		return cantUpdate(Codex, in.path)
	}
	return m.confirm(ctx, Codex, "", nil, progress)
}

// codexCommand is the command an install exposes: ~/.local/bin/codex, or
// current\bin\codex.exe on Windows (on PATH).
func (m *Manager) codexCommand() string {
	if m.Env.windows() {
		return m.Env.join(m.codexDir(), "current", "bin", "codex.exe")
	}
	return m.Env.join(m.Env.Home, ".local", "bin", "codex")
}

// installCodexRelease downloads, verifies and switches to the latest
// release; current is the installed version ("" = none, always install).
func (m *Manager) installCodexRelease(ctx context.Context, current string, progress func(string)) error {
	progress("Looking up the latest Codex release on GitHub…")
	rel, err := m.fetchRelease(ctx, m.githubLatestURL())
	if err != nil {
		return &Error{Message: fmt.Sprintf("Couldn't reach GitHub to find the latest Codex release: %v.", err), Err: err}
	}
	version, err := releaseVersion(rel)
	if err != nil {
		return &Error{Message: fmt.Sprintf("GitHub's latest Codex release looks wrong: %v.", err), Err: err}
	}
	if current != "" && CompareVersions(version, current) <= 0 {
		progress(fmt.Sprintf("Codex is up to date (%s).", current))
		return nil
	}
	target, err := codexTarget(m.Env.GOOS, m.Env.GOARCH)
	if err != nil {
		return &Error{Message: err.Error() + ".", Err: err}
	}
	pkg, sums, err := codexAssets(rel, target)
	if err != nil {
		return &Error{Message: fmt.Sprintf("Couldn't find Codex for this computer on GitHub: %v.", err), Err: err}
	}
	if err := m.checkCommandSlot(); err != nil {
		return err
	}
	want, err := m.expectedDigest(ctx, version, pkg, sums)
	if err != nil {
		return err
	}

	root := m.codexDir()
	versions := filepath.Join(root, "versions")
	if err := os.MkdirAll(versions, 0o755); err != nil {
		return installErr(root, err)
	}
	archive := filepath.Join(root, ".download-"+randomHex()+".tar.gz")
	defer os.Remove(archive)
	progress(fmt.Sprintf("Downloading Codex %s for %s (%d MB)…", version, target, (pkg.Size+1<<19)>>20))
	got, err := m.download(ctx, pkg, archive, progress)
	if err != nil {
		return &Error{Message: fmt.Sprintf("Couldn't download Codex from GitHub: %v.", err), Err: err}
	}
	progress("Checking the download against the SHA-256 OpenAI published…")
	if got != want {
		return &Error{Message: "The Codex download didn't match the checksum OpenAI published, so it wasn't installed.",
			Tail: []string{"expected " + want, "got      " + got}, Err: errChecksum}
	}

	progress("Unpacking…")
	staging := filepath.Join(versions, ".new-"+randomHex())
	defer os.RemoveAll(staging)
	if err := extractTarGz(archive, staging); err != nil {
		return &Error{Message: fmt.Sprintf("Couldn't unpack the Codex download: %v.", err), Err: err}
	}
	if err := checkPackage(staging, target, m.Env.windows()); err != nil {
		return &Error{Message: fmt.Sprintf("The Codex download isn't a usable package: %v.", err), Err: err}
	}
	dir := filepath.Join(versions, version)
	if m.Exists(dir) {
		// A reinstall of the same version: set the old copy aside.
		if err := os.Rename(dir, filepath.Join(versions, ".old-"+randomHex())); err != nil {
			return installErr(dir, err)
		}
	}
	if err := os.Rename(staging, dir); err != nil {
		return installErr(dir, err)
	}

	link := filepath.Join(root, "current")
	previous, _ := os.Readlink(link)
	if err := m.Link(dir, link); err != nil {
		return installErr(link, err)
	}
	if err := m.exposeCodex(root, progress); err != nil {
		m.restore(previous, link)
		return err
	}
	cmd := m.codexCommand()
	v, err := m.Version(ctx, cmd)
	if err != nil || v != version {
		m.restore(previous, link)
		if err == nil {
			err = fmt.Errorf("it reports version %s", v)
		}
		return &Error{Message: fmt.Sprintf("Codex %s was unpacked but doesn't run: %v.", version, err), Err: err}
	}
	m.cleanVersions(versions, version, filepath.Base(previous))
	progress(fmt.Sprintf("Codex %s is installed (%s).", version, cmd))
	return nil
}

func installErr(where string, err error) error {
	return &Error{Message: fmt.Sprintf("Couldn't install Codex in %s: %v.", where, err), Err: err}
}

func (m *Manager) restore(previous, link string) {
	if previous != "" && m.Exists(previous) {
		m.Link(previous, link)
	}
}

// checkCommandSlot refuses to replace a ~/.local/bin/codex that isn't
// code-link's (a file, or a working link elsewhere — another install that
// detection somehow missed). Nothing to check on Windows.
func (m *Manager) checkCommandSlot() error {
	if m.Env.windows() {
		return nil
	}
	p := m.codexCommand()
	if m.ourLink(p) {
		return nil
	}
	return &Error{Message: fmt.Sprintf("%s already exists and isn't code-link's, so code-link won't replace it.", p)}
}

// ourLink: nothing is at p, or a symlink pointing into code-link's Codex
// folder, or a dangling symlink.
func (m *Manager) ourLink(p string) bool {
	fi, err := os.Lstat(p)
	if err != nil {
		return true
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return false
	}
	target, err := os.Readlink(p)
	if err != nil {
		return false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(p), target)
	}
	if m.Env.under(target, m.codexDir()) {
		return true
	}
	_, err = os.Stat(p)
	return err != nil // dangling
}

// exposeCodex makes the installed version the one terminals run.
func (m *Manager) exposeCodex(root string, progress func(string)) error {
	if m.Env.windows() {
		bin := m.Env.join(root, "current", "bin")
		if m.Registry == nil {
			return nil
		}
		changed, err := addToUserPath(m.Registry, bin)
		if err != nil {
			return &Error{Message: fmt.Sprintf("Codex is installed, but couldn't be added to your PATH: %v.", err), Err: err}
		}
		if changed {
			progress(fmt.Sprintf("Added %s to your PATH (new terminals will find codex).", bin))
		}
		return nil
	}
	binDir := filepath.Join(m.Env.Home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return installErr(binDir, err)
	}
	for _, name := range []string{"codex", "codex-code-mode-host"} {
		target := filepath.Join(root, "current", "bin", name)
		link := filepath.Join(binDir, name)
		if !m.Exists(target) {
			if name == "codex" {
				return installErr(target, os.ErrNotExist)
			}
			continue
		}
		if !m.ourLink(link) {
			if name == "codex" {
				return m.checkCommandSlot()
			}
			continue // someone else's helper: leave it
		}
		if err := replaceSymlink(target, link); err != nil {
			return installErr(link, err)
		}
	}
	progress("Linked " + filepath.Join(binDir, "codex") + ".")
	return nil
}

// replaceSymlink points the symlink link at target, replacing what is there
// atomically (a new link renamed over the old one).
func replaceSymlink(target, link string) error {
	tmp := link + ".new-" + randomHex()
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// cleanVersions removes every version but keep and previous (a session
// started before the update may still be using it, and its helpers live in
// its folder), and leftovers of earlier runs. A folder is renamed before
// it is removed, so one Windows still locks (a running codex) stays whole
// until a later update.
func (m *Manager) cleanVersions(versions, keep, previous string) {
	entries, err := os.ReadDir(versions)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Name() == keep || e.Name() == previous {
			continue
		}
		p := filepath.Join(versions, e.Name())
		if !strings.HasPrefix(e.Name(), ".") {
			trash := filepath.Join(versions, ".trash-"+randomHex())
			if os.Rename(p, trash) != nil {
				continue
			}
			p = trash
		}
		os.RemoveAll(p)
	}
}

// download saves a release asset to dest, reporting progress every 10%,
// and returns its SHA-256.
func (m *Manager) download(ctx context.Context, a asset, dest string, progress func(string)) (string, error) {
	resp, err := m.open(ctx, a.URL, "application/octet-stream")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	size := a.Size
	if size <= 0 {
		size = resp.ContentLength
	}
	h := sha256.New()
	pw := &percentWriter{size: size, progress: progress}
	n, err := io.Copy(io.MultiWriter(f, h, pw), resp.Body)
	if err != nil {
		return "", err
	}
	if a.Size > 0 && n != a.Size {
		return "", fmt.Errorf("got %d of %d bytes", n, a.Size)
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type percentWriter struct {
	size, done int64
	last       int
	progress   func(string)
}

func (p *percentWriter) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	if p.size > 0 {
		pct := int(p.done * 100 / p.size)
		if pct/10 > p.last/10 && pct < 100 {
			p.progress(fmt.Sprintf("Downloaded %d%%", pct/10*10))
		}
		p.last = pct
	}
	return len(b), nil
}

const maxUnpacked = 4 << 30

// extractTarGz unpacks a .tar.gz into dest (created). Only folders and
// regular files are allowed, all inside dest: no links, no absolute or ..
// paths.
func extractTarGz(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	var total int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name, err := safeName(hdr.Name)
		if err != nil {
			return err
		}
		if name == "" {
			continue
		}
		target := filepath.Join(dest, filepath.FromSlash(name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			total += hdr.Size
			if total > maxUnpacked {
				return errors.New("the package is too large")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if hdr.Mode&0o111 != 0 {
				mode = 0o755
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, io.LimitReader(tr, hdr.Size))
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("unexpected entry %q in the package (type %c)", hdr.Name, hdr.Typeflag)
		}
	}
}

// safeName is a tar entry's path relative to the package, "" for the root;
// an error for anything that could land outside it.
func safeName(n string) (string, error) {
	if strings.ContainsAny(n, `\:`) || strings.HasPrefix(n, "/") {
		return "", fmt.Errorf("unsafe path %q in the package", n)
	}
	c := path.Clean(n)
	if c == "." {
		return "", nil
	}
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("unsafe path %q in the package", n)
	}
	return c, nil
}

// checkPackage checks the unpacked package is Codex for target.
func checkPackage(dir, target string, windows bool) error {
	b, err := os.ReadFile(filepath.Join(dir, "codex-package.json"))
	if err != nil {
		return errors.New("no codex-package.json")
	}
	var meta struct {
		Target     string `json:"target"`
		Entrypoint string `json:"entrypoint"`
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return fmt.Errorf("codex-package.json: %v", err)
	}
	if meta.Target != target {
		return fmt.Errorf("it is for %s, not %s", meta.Target, target)
	}
	exe := "bin/codex"
	if windows {
		exe += ".exe"
	}
	if meta.Entrypoint != exe {
		return fmt.Errorf("its entry point is %q", meta.Entrypoint)
	}
	fi, err := os.Stat(filepath.Join(dir, filepath.FromSlash(exe)))
	if err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("no %s", exe)
	}
	return nil
}

func randomHex() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}
