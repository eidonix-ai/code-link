//go:build !windows

package tools

import (
	"strconv"
	"syscall"
)

func processAlive(pid string) bool {
	n, err := strconv.Atoi(pid)
	if err != nil {
		return false
	}
	return syscall.Kill(n, 0) == nil
}
