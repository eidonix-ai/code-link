//go:build windows

package launch

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

const createNewConsole = 0x00000010

func (o Opener) open(s Session, folder, pathDir string) error {
	script, err := WriteScript(o.Dir, ".ps1", PowerShellScript(s, folder, pathDir))
	if err != nil {
		return fmt.Errorf("could not write the launch script: %w", err)
	}
	var last error
	for _, p := range WinPlans(o.Terminal, WindowsTerminalPath(), powerShellPath(), script, folder) {
		cmd := exec.Command(p.Program, p.Args...)
		cmd.Dir = p.Dir
		if p.NewConsole {
			cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewConsole}
		}
		if last = cmd.Start(); last == nil {
			go cmd.Wait()
			return nil
		}
	}
	os.Remove(script)
	if last == nil {
		last = errors.New("no terminal could be opened")
	}
	return last
}

// WindowsTerminalPath is wt.exe (an app execution alias in
// %LocalAppData%\Microsoft\WindowsApps), or "" when it isn't installed.
func WindowsTerminalPath() string {
	if p, err := exec.LookPath("wt.exe"); err == nil {
		return p
	}
	if d := os.Getenv("LOCALAPPDATA"); d != "" {
		p := filepath.Join(d, "Microsoft", "WindowsApps", "wt.exe")
		if fileExists(p) {
			return p
		}
	}
	return ""
}

// powerShellPath is Windows PowerShell, which every Windows has.
func powerShellPath() string {
	if root := os.Getenv("SystemRoot"); root != "" {
		p := filepath.Join(root, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
		if fileExists(p) {
			return p
		}
	}
	return "powershell.exe"
}
