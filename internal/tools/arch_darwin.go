//go:build darwin

package tools

import (
	"runtime"

	"golang.org/x/sys/unix"
)

// nativeArch is the Mac's architecture: arm64 also when an Intel build of
// code-link runs under Rosetta (sysctl.proc_translated), so the arm64
// Codex is installed.
func nativeArch() string {
	if runtime.GOARCH == "amd64" {
		if v, err := unix.SysctlUint32("sysctl.proc_translated"); err == nil && v == 1 {
			return "arm64"
		}
	}
	return runtime.GOARCH
}
