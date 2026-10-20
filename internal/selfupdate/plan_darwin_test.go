//go:build darwin

package selfupdate

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// The real ditto round trip: a bundle zipped the way scripts/build.sh does
// it is unpacked and swapped in.
func TestDarwinInstallWithDitto(t *testing.T) {
	src := t.TempDir()
	built := fakeBundle(t, src, "code-link.app", "new")
	zip := filepath.Join(t.TempDir(), "code-link-0.2.1-macos-arm64.zip")
	if out, err := exec.Command("/usr/bin/ditto", "-c", "-k", "--sequesterRsrc", "--keepParent", built, zip).CombinedOutput(); err != nil {
		t.Fatalf("ditto: %v %s", err, out)
	}

	dir := t.TempDir()
	bundle := fakeBundle(t, dir, "code-link.app", "old")
	p, err := darwinPlan(filepath.Join(bundle, "Contents", "MacOS", "code-link"))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Prepare(); err != nil {
		t.Fatal(err)
	}
	write(t, p.Download, read(t, zip))
	if err := p.Install(extractZip); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(bundle, "Contents", "MacOS", "code-link")) != "new" ||
		read(t, filepath.Join(p.Old, "Contents", "MacOS", "code-link")) != "old" {
		t.Error("not swapped")
	}
	if err := p.Cleanup(); err != nil || exists(p.Staging) {
		t.Errorf("cleanup: %v", err)
	}
}
