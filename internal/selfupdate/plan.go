package selfupdate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Plan is how one update replaces the running copy. It is pure paths, so
// both platforms' plans are tested on any machine.
//
// Windows: the running exe can be renamed but not overwritten, so the
// download goes to code-link.new.exe beside it, the running exe becomes
// code-link.old.exe, the new one takes the original name, and the new
// process (started with RelaunchFlag) waits for the old one to exit and
// deletes code-link.old.exe.
//
// macOS: the update zip is downloaded and extracted into .code-link-update
// beside the bundle, the running bundle is moved into it, the new bundle
// takes its place, and `open -n` starts it; once the old process has exited
// the new one removes .code-link-update.
type Plan struct {
	OS string `json:"os"`
	// Target is what gets replaced: the exe (Windows) or the .app bundle
	// (macOS). The new copy ends up at exactly this path.
	Target string `json:"target"`
	// Dir is Target's folder; it must be writable.
	Dir string `json:"dir"`
	// Download is where the verified file is written.
	Download string `json:"download"`
	// Old is where the running copy waits until the new one has started.
	Old string `json:"old"`
	// Staging (macOS) holds the download, the extracted bundle and Old.
	Staging string `json:"staging,omitempty"`
}

// Names next to the app.
const (
	windowsOldName = "code-link.old.exe"
	windowsNewName = "code-link.new.exe"
	darwinStaging  = ".code-link-update"
)

// UserError is a failure in plain words, with the technical detail apart.
type UserError struct {
	Message string
	Detail  string
}

func (e *UserError) Error() string {
	if e.Detail == "" {
		return e.Message
	}
	return e.Message + " (" + e.Detail + ")"
}

func userErr(msg string, err error) *UserError {
	e := &UserError{Message: msg}
	if err != nil {
		e.Detail = err.Error()
	}
	return e
}

// PlanFor is the plan for the running executable exe on goos.
func PlanFor(goos, exe string) (Plan, error) {
	switch goos {
	case "windows":
		return windowsPlan(exe), nil
	case "darwin":
		return darwinPlan(exe)
	}
	return Plan{}, &UserError{Message: "code-link updates itself only on macOS and Windows."}
}

func windowsPlan(exe string) Plan {
	dir := filepath.Dir(exe)
	return Plan{OS: "windows", Target: exe, Dir: dir,
		Download: filepath.Join(dir, windowsNewName), Old: filepath.Join(dir, windowsOldName)}
}

func darwinPlan(exe string) (Plan, error) {
	macos := filepath.Dir(exe)
	bundle := filepath.Dir(filepath.Dir(macos))
	if filepath.Base(macos) != "MacOS" || filepath.Base(filepath.Dir(macos)) != "Contents" || !strings.HasSuffix(bundle, ".app") {
		return Plan{}, &UserError{Message: "This copy of code-link isn't an app (it's a development build), so it can't update itself."}
	}
	if strings.Contains(bundle, "/AppTranslocation/") {
		return Plan{}, &UserError{Message: "macOS is running code-link from a temporary copy, so it can't update itself. " +
			"Drag code-link onto Applications, open it from there, then update."}
	}
	dir := filepath.Dir(bundle)
	staging := filepath.Join(dir, darwinStaging)
	return Plan{OS: "darwin", Target: bundle, Dir: dir, Staging: staging,
		Download: filepath.Join(staging, "update.zip"),
		Old:      filepath.Join(staging, "old", filepath.Base(bundle))}, nil
}

// Prepare checks the folder can be written and clears what an earlier,
// unfinished update left behind.
func (p Plan) Prepare() error {
	if err := writable(p.Dir); err != nil {
		return p.notWritable(err)
	}
	switch p.OS {
	case "windows":
		os.Remove(p.Download)
	case "darwin":
		if err := os.RemoveAll(p.Staging); err != nil {
			return userErr("An earlier update left files code-link can't remove next to the app.", err)
		}
		if err := os.MkdirAll(p.Staging, 0o755); err != nil {
			return p.notWritable(err)
		}
	}
	return nil
}

func (p Plan) notWritable(err error) error {
	if p.OS == "windows" {
		return userErr(fmt.Sprintf("code-link can't update itself because the folder it's in (%s) isn't writable. "+
			"Move code-link.exe to a folder of your own — for example a code-link folder in Documents — "+
			"open it from there, and try again.", p.Dir), err)
	}
	if strings.HasPrefix(p.Dir, "/Volumes/") {
		return userErr("code-link is running from the disk image, so it can't update itself. "+
			"Drag code-link onto Applications, eject the disk image, open code-link from Applications, then update.", err)
	}
	return userErr(fmt.Sprintf("code-link can't update itself because the folder it's in (%s) isn't writable. "+
		"Move code-link to Applications (or another folder of your own), open it from there, and try again.", p.Dir), err)
}

// writable creates and removes a file in dir.
func writable(dir string) error {
	f, err := os.CreateTemp(dir, ".code-link-write-check-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// Install swaps the verified download in place of the running copy. On
// failure the running copy is put back where it was. extract unzips a
// macOS update (ditto -x -k) and is unused on Windows.
func (p Plan) Install(extract func(zip, dest string) error) error {
	if p.OS == "windows" {
		return p.installWindows()
	}
	return p.installDarwin(extract)
}

func (p Plan) installWindows() error {
	if err := os.Remove(p.Old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return userErr("An older "+windowsOldName+" next to code-link is still open or locked. Close it (or restart Windows) and try again.", err)
	}
	if err := rename(p.Target, p.Old); err != nil {
		return userErr("Windows didn't let code-link move itself aside to make room for the new version.", err)
	}
	if err := rename(p.Download, p.Target); err != nil {
		if rerr := rename(p.Old, p.Target); rerr != nil {
			return userErr(fmt.Sprintf("The update couldn't be put in place, and code-link couldn't put itself back. "+
				"Rename %s to %s to get it back.", p.Old, filepath.Base(p.Target)), fmt.Errorf("%v; restoring: %v", err, rerr))
		}
		return userErr("Windows didn't let code-link put the new version in place.", err)
	}
	return nil
}

func (p Plan) installDarwin(extract func(zip, dest string) error) error {
	unpacked := filepath.Join(p.Staging, "new")
	os.RemoveAll(unpacked)
	if err := os.MkdirAll(unpacked, 0o755); err != nil {
		return userErr("The update couldn't be unpacked next to the app.", err)
	}
	if err := extract(p.Download, unpacked); err != nil {
		return userErr("The update couldn't be unpacked.", err)
	}
	newBundle, err := findBundle(unpacked)
	if err != nil {
		return userErr("The update doesn't contain the code-link app.", err)
	}
	carryPortableSettings(p.Target, newBundle)
	if err := os.MkdirAll(filepath.Dir(p.Old), 0o755); err != nil {
		return userErr("The update couldn't be prepared next to the app.", err)
	}
	if err := os.Rename(p.Target, p.Old); err != nil {
		return userErr("macOS didn't let code-link move itself aside to make room for the new version.", err)
	}
	if err := os.Rename(newBundle, p.Target); err != nil {
		if rerr := os.Rename(p.Old, p.Target); rerr != nil {
			return userErr(fmt.Sprintf("The update couldn't be put in place, and code-link couldn't put itself back. "+
				"Move %s back to %s to get it back.", p.Old, p.Dir), fmt.Errorf("%v; restoring: %v", err, rerr))
		}
		return userErr("macOS didn't let code-link put the new version in place.", err)
	}
	os.Remove(p.Download)
	os.RemoveAll(unpacked)
	return nil
}

// findBundle is the one .app at the top of dir.
func findBundle(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var found []string
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), ".app") {
			found = append(found, filepath.Join(dir, e.Name()))
		}
	}
	if len(found) != 1 {
		return "", fmt.Errorf("%d .app bundles in the zip", len(found))
	}
	if _, err := os.Stat(filepath.Join(found[0], "Contents", "MacOS")); err != nil {
		return "", err
	}
	return found[0], nil
}

// carryPortableSettings copies a settings file kept next to the executable
// inside the bundle (portable mode), and the files beside it, into the new
// bundle, so the update keeps the same settings.
func carryPortableSettings(oldBundle, newBundle string) {
	from := filepath.Join(oldBundle, "Contents", "MacOS")
	to := filepath.Join(newBundle, "Contents", "MacOS")
	if _, err := os.Stat(filepath.Join(from, "code-link.json")); err != nil {
		return
	}
	entries, _ := os.ReadDir(from)
	for _, e := range entries {
		dst := filepath.Join(to, e.Name())
		if !e.Type().IsRegular() {
			continue
		}
		if _, err := os.Lstat(dst); err == nil {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(from, e.Name())); err == nil {
			os.WriteFile(dst, b, 0o600)
		}
	}
}

// Relaunch is the command that starts the new copy, telling it to wait for
// this process (pid) to exit first.
func (p Plan) Relaunch(pid int) (name string, args []string) {
	flag := RelaunchFlag + "=" + strconv.Itoa(pid)
	if p.OS == "windows" {
		return p.Target, []string{flag}
	}
	return "/usr/bin/open", []string{"-n", p.Target, "--args", flag}
}

// Cleanup removes what the last update left: code-link.old.exe and a stray
// code-link.new.exe (Windows) or .code-link-update (macOS). Call it only
// when the old process has exited.
func (p Plan) Cleanup() error {
	if p.OS == "windows" {
		var last error
		for _, f := range []string{p.Old, p.Download} {
			if err := retry(func() error { return os.Remove(f) }); err != nil && !errors.Is(err, os.ErrNotExist) {
				last = err
			}
		}
		return last
	}
	if p.Staging == "" {
		return nil
	}
	return os.RemoveAll(p.Staging)
}

// rename retries briefly: on Windows a virus scanner often holds a fresh
// exe for a moment.
func rename(from, to string) error {
	return retry(func() error { return os.Rename(from, to) })
}

var retryDelay = 250 * time.Millisecond

func retry(f func() error) error {
	var err error
	for i := 0; i < 8; i++ {
		if err = f(); err == nil || errors.Is(err, os.ErrNotExist) {
			return err
		}
		time.Sleep(retryDelay)
	}
	return err
}
