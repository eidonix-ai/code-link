//go:build !windows

package selfupdate

import (
	"errors"
	"syscall"
	"time"
)

// platformWaitForExit polls until pid is gone, at most timeout.
func platformWaitForExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}
