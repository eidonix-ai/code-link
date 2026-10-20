//go:build windows

package launch

import (
	"os"

	"golang.org/x/sys/windows/registry"
)

// Find looks for a tool on PATH (this process's, then the user's and the
// machine's from the registry, fresh after an install), then in the usual
// install locations (the native installer's %UserProfile%\.local\bin, npm's
// %AppData%\npm shims, WinGet, Scoop, …). The folder it was found in goes
// first on PATH in the launch script.
func Find(t Tool) (Found, bool) {
	home, _ := os.UserHomeDir()
	paths := []string{os.Getenv("PATH"),
		registryPath(registry.CURRENT_USER, `Environment`),
		registryPath(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`)}
	return Search(t, SearchEnv{GOOS: "windows", Paths: paths, Home: home,
		AppData: os.Getenv("APPDATA"), LocalAppData: os.Getenv("LOCALAPPDATA"), Exists: regularFile})
}

func registryPath(root registry.Key, path string) string {
	k, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetStringValue("Path")
	if err != nil {
		return ""
	}
	if x, err := registry.ExpandString(v); err == nil {
		return x
	}
	return v
}
