//go:build !darwin && !windows

package loginitem

import (
	"errors"
	"os"
)

var executable = os.Executable

func Enabled() bool { return false }

func Enable(string) error {
	return errors.New("open at login is supported on macOS and Windows")
}

func Disable() error { return nil }
