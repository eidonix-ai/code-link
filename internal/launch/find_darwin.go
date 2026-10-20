//go:build darwin

package launch

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Find looks for a tool the way the launch script will run it: on the
// user's login-shell PATH first (`$SHELL -lic 'command -v claude'`, 5 s),
// then in PATH and the usual install locations.
func Find(t Tool) (Found, bool) {
	shell := LoginShell(os.Getenv, fileExists)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, "-lic", "command -v "+t.Executable())
	cmd.Stdin = nil
	if out, err := cmd.Output(); err == nil {
		if line := lastLine(string(out)); line != "" {
			if strings.HasPrefix(line, "/") {
				return Found{Path: line}, true
			}
			return Found{}, true // an alias or function
		}
	}
	home, _ := os.UserHomeDir()
	return Search(t, SearchEnv{GOOS: "darwin", Paths: []string{os.Getenv("PATH")}, Home: home,
		Exists: regularFile, Glob: func(p string) []string { m, _ := filepath.Glob(p); return m }})
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// FreshPath is this process's PATH (the login shell's is looked up per command).
func FreshPath() string { return os.Getenv("PATH") }
