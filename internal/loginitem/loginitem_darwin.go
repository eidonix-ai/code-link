//go:build darwin

package loginitem

import (
	"os"
	"path/filepath"
)

var executable = os.Executable

// PlistPath is ~/Library/LaunchAgents/com.eidonix.code-link.plist.
func PlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist"), nil
}

// Enabled: the LaunchAgent exists.
func Enabled() bool {
	p, err := PlistPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// Enable writes the LaunchAgent for exe (only when it differs). launchd
// reads it at the next login; nothing is loaded now.
func Enable(exe string) error {
	p, err := PlistPath()
	if err != nil {
		return err
	}
	return writeIfChanged(p, Plist(exe))
}

// Disable removes the LaunchAgent.
func Disable() error {
	p, err := PlistPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func writeIfChanged(p, content string) error {
	if b, err := os.ReadFile(p); err == nil && string(b) == content {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(content), 0o644)
}
