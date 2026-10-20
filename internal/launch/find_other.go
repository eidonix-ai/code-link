//go:build !darwin && !windows

package launch

import "os"

// Find looks for a tool on PATH and in the usual install locations.
func Find(t Tool) (Found, bool) {
	home, _ := os.UserHomeDir()
	return Search(t, SearchEnv{GOOS: "linux", Paths: []string{os.Getenv("PATH")}, Home: home, Exists: regularFile})
}

// FreshPath is this process's PATH (the login shell's is looked up per command).
func FreshPath() string { return os.Getenv("PATH") }
