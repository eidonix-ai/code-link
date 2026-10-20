//go:build !windows

package tools

import (
	"os/exec"
	"syscall"
)

// prepare runs cmd in its own process group; cancelling kills the group
// (the login shell, curl, bash and the installer they started).
func prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}

func powerShell() string { return "powershell.exe" }

// linkDir points the symlink link at the folder target.
func linkDir(target, link string) error { return replaceSymlink(target, link) }

func userPathRegistry() PathRegistry { return nil }
