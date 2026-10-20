// updatekeys makes and uses the key that signs code-link's update feed
// (internal/selfupdate). Run it from the repository root:
//
//	go run ./cmd/updatekeys keygen [-key ~/.code-link/update-signing.key] [-pub internal/selfupdate/update-public-key.txt]
//	    Makes the Ed25519 key pair, once. The private key (a base64 seed) is
//	    written 0600 to -key and must never be committed or copied around;
//	    the public key goes to -pub, which is embedded in the app — commit
//	    that file. Refuses to replace either: a new key would strand every
//	    installed copy.
//
//	go run ./cmd/updatekeys manifest -version 0.2.1 [-notes "…"] [-base https://codelink.eidonix.com/files/] -out manifest.json darwin-arm64=bin/x.zip windows-amd64=bin/y.exe
//	    Writes the manifest: per platform the file's URL (-base + its name),
//	    SHA-256 and size.
//
//	go run ./cmd/updatekeys sign [-key …] -in manifest.json [-out manifest.json.sig]
//	    Signs the manifest's exact bytes. Refuses a key that isn't the one
//	    embedded in the app.
//
//	go run ./cmd/updatekeys verify -in manifest.json [-sig manifest.json.sig]
//	    Checks a manifest as installed apps will.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"codelink/internal/selfupdate"
)

const defaultPub = "internal/selfupdate/update-public-key.txt"

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "updatekeys:", err)
		os.Exit(1)
	}
}

func defaultKey() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".code-link", "update-signing.key")
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: updatekeys keygen|manifest|sign|verify [flags]")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	switch args[0] {
	case "keygen":
		key := fs.String("key", defaultKey(), "private key to write")
		pub := fs.String("pub", defaultPub, "public key file embedded in the app")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return keygen(*key, *pub, out)
	case "manifest":
		version := fs.String("version", "", "the release's version (x.y.z)")
		notes := fs.String("notes", "", "what changed")
		base := fs.String("base", "https://codelink.eidonix.com/files/", "URL the files are published under")
		dst := fs.String("out", "", "manifest to write")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return manifest(*version, *notes, *base, *dst, fs.Args())
	case "sign":
		key := fs.String("key", defaultKey(), "private key")
		in := fs.String("in", "", "manifest")
		dst := fs.String("out", "", "signature to write (default: manifest + .sig)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		pub, err := selfupdate.PublicKey()
		if err != nil {
			return fmt.Errorf("no public key is embedded (%v): run keygen and commit %s", err, defaultPub)
		}
		return sign(*key, *in, *dst, pub)
	case "verify":
		in := fs.String("in", "", "manifest")
		sig := fs.String("sig", "", "signature (default: manifest + .sig)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		pub, err := selfupdate.PublicKey()
		if err != nil {
			return fmt.Errorf("no public key is embedded: %v", err)
		}
		m, err := verify(*in, *sig, pub)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s: signed, code-link %s, %d files\n", *in, m.Version, len(m.Files))
		return nil
	}
	return fmt.Errorf("unknown command %q (want keygen, manifest, sign or verify)", args[0])
}

func keygen(keyPath, pubPath string, out io.Writer) error {
	if keyPath == "" {
		return errors.New("keygen: -key is required")
	}
	if _, err := os.Stat(keyPath); err == nil {
		return fmt.Errorf("keygen: %s already exists; refusing to replace a signing key", keyPath)
	}
	if b, err := os.ReadFile(pubPath); err == nil && strings.TrimSpace(string(b)) != "" {
		return fmt.Errorf("keygen: %s already holds a public key; refusing to replace it (installed copies trust only that key)", pubPath)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := os.Stat(filepath.Dir(pubPath)); err != nil {
		return fmt.Errorf("keygen: %s doesn't exist; run this from the code-link checkout", filepath.Dir(pubPath))
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(base64.StdEncoding.EncodeToString(priv.Seed()) + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.WriteFile(pubPath, []byte(base64.StdEncoding.EncodeToString(pub)+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "private key: %s (keep it there, back it up, never commit it)\npublic key:  %s (commit it)\n", keyPath, pubPath)
	return nil
}

func readKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the signing key: %w", err)
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s isn't a base64 Ed25519 seed", path)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func manifest(version, notes, base, dst string, files []string) error {
	if dst == "" || len(files) == 0 {
		return errors.New("manifest: -out and at least one platform=file are required")
	}
	m := selfupdate.Manifest{Version: version, Notes: notes, Files: map[string]selfupdate.File{}}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	for _, a := range files {
		platform, path, ok := strings.Cut(a, "=")
		if !ok || platform == "" || path == "" {
			return fmt.Errorf("manifest: %q isn't platform=file", a)
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.Copy(h, f)
		f.Close()
		if err != nil {
			return err
		}
		m.Files[platform] = selfupdate.File{URL: base + filepath.Base(path), SHA256: hex.EncodeToString(h.Sum(nil)), Size: n}
	}
	if err := m.Check(); err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, append(b, '\n'), 0o644)
}

func sign(keyPath, in, dst string, pub ed25519.PublicKey) error {
	if in == "" {
		return errors.New("sign: -in is required")
	}
	if dst == "" {
		dst = in + selfupdate.SignatureSuffix
	}
	priv, err := readKey(keyPath)
	if err != nil {
		return err
	}
	if !priv.Public().(ed25519.PublicKey).Equal(pub) {
		return fmt.Errorf("sign: %s is not the key the app trusts (%s)", keyPath, defaultPub)
	}
	body, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	sig := selfupdate.Sign(body, priv)
	if _, err := selfupdate.Verify(body, sig, pub); err != nil {
		return fmt.Errorf("sign: %w", err)
	}
	return os.WriteFile(dst, sig, 0o644)
}

func verify(in, sigPath string, pub ed25519.PublicKey) (*selfupdate.Manifest, error) {
	if in == "" {
		return nil, errors.New("verify: -in is required")
	}
	if sigPath == "" {
		sigPath = in + selfupdate.SignatureSuffix
	}
	body, err := os.ReadFile(in)
	if err != nil {
		return nil, err
	}
	sig, err := os.ReadFile(sigPath)
	if err != nil {
		return nil, err
	}
	return selfupdate.Verify(body, sig, pub)
}
