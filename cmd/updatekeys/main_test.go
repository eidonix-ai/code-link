package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codelink/internal/selfupdate"
)

func TestKeygenManifestSignVerify(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "home", ".code-link", "update-signing.key")
	pubFile := filepath.Join(dir, "update-public-key.txt")
	os.WriteFile(pubFile, nil, 0o644) // the committed empty placeholder
	if err := keygen(key, pubFile, io.Discard); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(key); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file %v %v", fi, err)
	}
	if fi, _ := os.Stat(filepath.Dir(key)); fi.Mode().Perm() != 0o700 {
		t.Errorf("key dir mode %v", fi.Mode().Perm())
	}
	b, _ := os.ReadFile(pubFile)
	pub, err := selfupdate.ParsePublicKey(string(b))
	if err != nil {
		t.Fatal(err)
	}
	if err := keygen(key, filepath.Join(dir, "other.txt"), io.Discard); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("replaced the key: %v", err)
	}
	if err := keygen(filepath.Join(dir, "k2"), pubFile, io.Discard); err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Errorf("replaced the public key: %v", err)
	}

	zip := filepath.Join(dir, "code-link-0.2.1-macos-arm64.zip")
	exe := filepath.Join(dir, "code-link-0.2.1-windows-amd64.exe")
	os.WriteFile(zip, []byte("zip bytes"), 0o644)
	os.WriteFile(exe, []byte("exe"), 0o644)
	m := filepath.Join(dir, "update", "manifest.json")
	if err := manifest("0.2.1", "Notes.", "https://codelink.eidonix.com/files", m,
		[]string{"darwin-arm64=" + zip, "windows-amd64=" + exe}); err != nil {
		t.Fatal(err)
	}
	if err := sign(key, m, "", pub); err != nil {
		t.Fatal(err)
	}
	got, err := verify(m, "", pub)
	if err != nil {
		t.Fatal(err)
	}
	mac := got.Files["darwin-arm64"]
	if got.Version != "0.2.1" || got.Notes != "Notes." || mac.Size != 9 ||
		mac.URL != "https://codelink.eidonix.com/files/code-link-0.2.1-macos-arm64.zip" ||
		got.Files["windows-amd64"].URL != "https://codelink.eidonix.com/files/code-link-0.2.1-windows-amd64.exe" {
		t.Errorf("%+v", got)
	}

	// A different key is refused, and a changed manifest no longer verifies.
	other, _, _ := ed25519.GenerateKey(nil)
	if err := sign(key, m, filepath.Join(dir, "x.sig"), other); err == nil {
		t.Error("signed with a key the app doesn't trust")
	}
	os.WriteFile(m, []byte(strings.Replace(mustRead(t, m), "0.2.1", "0.2.2", 1)), 0o644)
	if _, err := verify(m, "", pub); err == nil {
		t.Error("tampered manifest verified")
	}
}

func TestManifestRefusesBadInput(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.exe")
	os.WriteFile(f, []byte("x"), 0o644)
	for name, c := range map[string]struct {
		version string
		files   []string
	}{
		"no version":  {"", []string{"windows-amd64=" + f}},
		"no files":    {"0.2.1", nil},
		"no platform": {"0.2.1", []string{f}},
		"missing":     {"0.2.1", []string{"windows-amd64=" + filepath.Join(dir, "nope")}},
		"empty file":  {"0.2.1", []string{"windows-amd64=" + os.DevNull}},
	} {
		if err := manifest(c.version, "", "https://x/", filepath.Join(dir, "m.json"), c.files); err == nil {
			t.Errorf("%s: written", name)
		}
	}
}

func TestReadKeyRefusesGarbage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k")
	os.WriteFile(p, []byte(base64.StdEncoding.EncodeToString([]byte("short"))), 0o600)
	if _, err := readKey(p); err == nil {
		t.Error("accepted")
	}
}

func mustRead(t *testing.T, p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
