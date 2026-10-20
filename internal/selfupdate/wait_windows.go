//go:build windows

package selfupdate

import (
	"errors"
	"time"

	"golang.org/x/sys/windows"
)

// platformWaitForExit waits for pid to exit, at most timeout. If the
// process can't be opened for another reason than being gone, it waits a
// short beat instead.
func platformWaitForExit(pid int, timeout time.Duration) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return true // no such process: already gone
		}
		time.Sleep(time.Second)
		return false
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, uint32(timeout.Milliseconds()))
	return err == nil && ev == windows.WAIT_OBJECT_0
}
