//go:build windows

package tools

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const createNoWindow = 0x08000000

// prepare runs cmd without a console window, in its own process group;
// cancelling kills the whole tree (PowerShell → the installer it started).
func prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true,
		CreationFlags: createNoWindow | syscall.CREATE_NEW_PROCESS_GROUP}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		kill := exec.Command(filepath.Join(system32(), "taskkill.exe"), "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
		kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
		if kill.Run() != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}

func system32() string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, "System32")
}

func powerShell() string {
	p := filepath.Join(system32(), "WindowsPowerShell", "v1.0", "powershell.exe")
	if exists(p) {
		return p
	}
	return "powershell.exe"
}

// nativeArch is the machine's architecture, also when an x64 build runs
// emulated on Windows ARM.
func nativeArch() string {
	var process, native uint16
	if err := windows.IsWow64Process2(windows.CurrentProcess(), &process, &native); err == nil {
		switch native {
		case 0xAA64: // IMAGE_FILE_MACHINE_ARM64
			return "arm64"
		case 0x8664: // IMAGE_FILE_MACHINE_AMD64
			return "amd64"
		}
	}
	return runtime.GOARCH
}

// linkDir points the junction link at the folder target (a junction needs
// no admin rights or Developer Mode, unlike a symlink). An existing
// junction at link is replaced; a real folder there is an error.
func linkDir(target, link string) error {
	if fi, err := os.Lstat(link); err == nil {
		if fi.Mode()&os.ModeIrregular == 0 && fi.Mode()&os.ModeSymlink == 0 && fi.IsDir() {
			// os.Lstat reports a junction as a symlink or irregular; a plain
			// folder isn't ours to remove.
			return errors.New(link + " is a folder, not a link")
		}
		if err := os.Remove(link); err != nil {
			return err
		}
	}
	cmd := exec.Command(filepath.Join(system32(), "cmd.exe"), "/d", "/c", "mklink", "/J", link, target)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	if out, err := cmd.CombinedOutput(); err != nil {
		return errors.New("mklink /J failed: " + CleanLine(string(out)))
	}
	return nil
}

func userPathRegistry() PathRegistry { return winRegistry{} }

// winRegistry is HKCU\Environment.
type winRegistry struct{}

func (winRegistry) UserPath() (string, bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Environment`, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return "", true, nil
	}
	if err != nil {
		return "", false, err
	}
	defer k.Close()
	v, typ, err := k.GetStringValue("Path")
	if errors.Is(err, registry.ErrNotExist) {
		return "", true, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, typ == registry.EXPAND_SZ, nil
}

func (winRegistry) SetUserPath(value string, expand bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, `Environment`, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if expand {
		err = k.SetExpandStringValue("Path", value)
	} else {
		err = k.SetStringValue("Path", value)
	}
	return err
}

func (winRegistry) Expand(s string) string {
	if x, err := registry.ExpandString(s); err == nil {
		return x
	}
	return s
}

var sendMessageTimeout = windows.NewLazySystemDLL("user32.dll").NewProc("SendMessageTimeoutW")

func (winRegistry) Broadcast() {
	const hwndBroadcast, wmSettingChange, smtoAbortIfHung = 0xffff, 0x001A, 0x0002
	env, _ := windows.UTF16PtrFromString("Environment")
	var result uintptr
	sendMessageTimeout.Call(hwndBroadcast, wmSettingChange, 0, uintptr(unsafe.Pointer(env)),
		smtoAbortIfHung, 5000, uintptr(unsafe.Pointer(&result)))
}
