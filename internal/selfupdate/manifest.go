// Package selfupdate keeps code-link up to date: it checks a signed
// manifest at start and every six hours, offers the newer version in the
// tray, and on request downloads it, verifies it and swaps it in place of the
// running copy, then restarts.
//
// The feed is two files next to each other on codelink.eidonix.com:
//
//	update/manifest.json      the manifest (below), published as-is
//	update/manifest.json.sig  base64 of the Ed25519 signature over the exact
//	                          bytes of manifest.json, one line
//
//	{
//	  "version": "0.2.1",
//	  "notes": "What changed, in a sentence or two",
//	  "files": {
//	    "darwin-arm64":  {"url": "https://codelink.eidonix.com/files/code-link-0.2.1-macos-arm64.zip", "sha256": "<64 hex>", "size": 12345678},
//	    "windows-amd64": {"url": "https://codelink.eidonix.com/files/code-link-0.2.1-windows-amd64.exe", "sha256": "<64 hex>", "size": 23456789}
//	  }
//	}
//
// The macOS file is a zip of code-link.app (ditto, so the signature
// survives); the Windows file is the bare code-link.exe. The signature is
// checked before the manifest is parsed, and every download is checked
// against the manifest's size and SHA-256 before anything is replaced. The
// public key is embedded (update-public-key.txt, written by
// `go run ./cmd/updatekeys keygen`); the private key lives only on the
// publishing Mac (~/.code-link/update-signing.key). A build without a public
// key never offers updates.
package selfupdate

import (
	"crypto/ed25519"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// DefaultManifestURL is where the published manifest lives; the signature
// is the same URL plus ".sig".
const DefaultManifestURL = "https://codelink.eidonix.com/update/manifest.json"

// SignatureSuffix names the detached signature next to the manifest.
const SignatureSuffix = ".sig"

// maxArtifact bounds a published file (the builds are tens of MB).
const maxArtifact = 1 << 30

//go:embed update-public-key.txt
var embeddedPublicKey string

// File is one platform's download.
type File struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Manifest is the published feed.
type Manifest struct {
	Version string          `json:"version"`
	Notes   string          `json:"notes,omitempty"`
	Files   map[string]File `json:"files"`
}

// ErrBadSignature: the manifest isn't signed by the embedded key.
var ErrBadSignature = errors.New("the update information isn't signed by Eidonix")

// ErrNoPublicKey: this build has no update key, so it never updates.
var ErrNoPublicKey = errors.New("this build has no update key")

// PublicKey is the embedded update key.
func PublicKey() (ed25519.PublicKey, error) {
	if strings.TrimSpace(embeddedPublicKey) == "" {
		return nil, ErrNoPublicKey
	}
	return ParsePublicKey(embeddedPublicKey)
}

// ParsePublicKey reads a base64 Ed25519 public key.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("the update key isn't a base64 Ed25519 public key")
	}
	return ed25519.PublicKey(b), nil
}

// Sign returns the detached signature file's content for a manifest.
func Sign(manifest []byte, priv ed25519.PrivateKey) []byte {
	return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, manifest)) + "\n")
}

// Verify checks the signature over the exact manifest bytes, then parses
// and checks the manifest. Nothing unsigned is ever parsed.
func Verify(manifest, sig []byte, pub ed25519.PublicKey) (*Manifest, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, ErrNoPublicKey
	}
	s, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || len(s) != ed25519.SignatureSize || !ed25519.Verify(pub, manifest, s) {
		return nil, ErrBadSignature
	}
	var m Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		return nil, fmt.Errorf("the update information can't be read: %w", err)
	}
	if err := m.Check(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Check is the manifest's shape: a plain x.y.z version and, per platform,
// a URL, a SHA-256 and a size.
func (m *Manifest) Check() error {
	if _, ok := ParseVersion(m.Version); !ok {
		return fmt.Errorf("the update information has no usable version (%q)", m.Version)
	}
	for key, f := range m.Files {
		if f.URL == "" {
			return fmt.Errorf("the %s download has no address", key)
		}
		if b, err := hex.DecodeString(f.SHA256); err != nil || len(b) != 32 {
			return fmt.Errorf("the %s download has no usable SHA-256", key)
		}
		if f.Size <= 0 || f.Size > maxArtifact {
			return fmt.Errorf("the %s download has no usable size", key)
		}
	}
	return nil
}

// ParseVersion reads "x.y.z" (a leading "v" and missing trailing parts are
// fine; anything else, like "dev" or "0.2.0-beta", is not a version).
func ParseVersion(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	parts := strings.Split(v, ".")
	if v == "" || len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		if p == "" || len(p) > 9 || strings.Trim(p, "0123456789") != "" {
			return out, false
		}
		out[i], _ = strconv.Atoi(p)
	}
	return out, true
}

// Compare is -1, 0 or 1 for a < b, a == b, a > b. An unreadable version
// sorts below every real one.
func Compare(a, b string) int {
	va, oka := ParseVersion(a)
	vb, okb := ParseVersion(b)
	switch {
	case !oka && !okb:
		return 0
	case !oka:
		return -1
	case !okb:
		return 1
	}
	for i := range va {
		if va[i] != vb[i] {
			if va[i] < vb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// Newer: latest is a real version above current, which is a real version
// too (a development build is never offered an update).
func Newer(latest, current string) bool {
	_, okl := ParseVersion(latest)
	_, okc := ParseVersion(current)
	return okl && okc && Compare(latest, current) > 0
}
