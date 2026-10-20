//go:build windows

package loginitem

import (
	"os"

	"golang.org/x/sys/windows/registry"
)

var executable = os.Executable

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

// Enabled: the Run value exists.
func Enabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(RunValueName)
	return err == nil
}

// Enable sets the Run value to exe (only when it differs).
func Enable(exe string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if v, _, err := k.GetStringValue(RunValueName); err == nil && v == RunValue(exe) {
		return nil
	}
	return k.SetStringValue(RunValueName, RunValue(exe))
}

// Disable removes the Run value.
func Disable() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return nil
	}
	defer k.Close()
	if err := k.DeleteValue(RunValueName); err != nil && err != registry.ErrNotExist {
		return err
	}
	return nil
}
