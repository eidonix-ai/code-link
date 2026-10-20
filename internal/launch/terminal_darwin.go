//go:build darwin

package launch

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func (o Opener) open(s Session, folder, pathDir string) error {
	shell := LoginShell(os.Getenv, fileExists)
	script, err := WriteScript(o.Dir, ".sh", []byte(POSIXScript(shell, s, folder, pathDir)))
	if err != nil {
		return fmt.Errorf("could not write the launch script: %w", err)
	}
	var last error
	for _, p := range MacPlans(o.Terminal, ITermInstalled(), script) {
		if last = osascript(p.AppleScript, p.Argument); last == nil {
			return nil
		}
	}
	os.Remove(script)
	if last == nil {
		last = errors.New("no terminal could be opened")
	}
	return last
}

// ITermInstalled: iTerm in /Applications or ~/Applications, or anywhere
// Spotlight knows of.
func ITermInstalled() bool {
	paths := []string{"/Applications/iTerm.app"}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, "Applications", "iTerm.app"))
	}
	for _, p := range paths {
		if fileExists(p) {
			return true
		}
	}
	out, err := exec.Command("/usr/bin/mdfind", "kMDItemCFBundleIdentifier == 'com.googlecode.iterm2'").Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

// osascript runs an AppleScript with one argument. It may wait on the
// Automation permission prompt the first time.
func osascript(script, arg string) error {
	out, err := exec.Command("/usr/bin/osascript", "-e", script, arg).CombinedOutput()
	if err != nil {
		if detail := strings.TrimSpace(string(out)); detail != "" {
			return errors.New(detail)
		}
		return err
	}
	return nil
}
