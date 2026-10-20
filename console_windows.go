//go:build windows

package main

import (
	"os"
	"syscall"
)

// attachConsole connects stdout and stderr to the console code-link was
// started from: the program is built as a GUI app (no console window of its
// own), so `code-link serve` would otherwise print nothing.
func attachConsole() {
	const attachParentProcess = ^uintptr(0) // (DWORD)-1
	k := syscall.NewLazyDLL("kernel32.dll")
	if r, _, _ := k.NewProc("AttachConsole").Call(attachParentProcess); r == 0 {
		return
	}
	if f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
		os.Stdout, os.Stderr = f, f
	}
}
