package selfupdate

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// RelaunchFlag starts the new copy after an update: "--update-relaunch-wait=<pid>"
// makes it wait for the old process to exit (so the single-instance guard
// and the port are free), then remove the old copy. It is internal
// plumbing: AfterRelaunch strips it before anything else sees the arguments.
const RelaunchFlag = "--update-relaunch-wait"

// relaunchWait bounds the wait: the old process quits right after starting
// the new one, so this only runs out if it hung on the way out.
const relaunchWait = 30 * time.Second

// waitForExit and cleanupRunning are seams for tests.
var (
	waitForExit    = platformWaitForExit
	cleanupRunning = CleanupRunning
)

// StripRelaunch removes the flag from args and returns the old process's
// pid (0 when absent or malformed).
func StripRelaunch(args []string) (rest []string, pid int, found bool) {
	rest = make([]string, 0, len(args))
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, RelaunchFlag+"="); ok || a == RelaunchFlag {
			found = true
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				pid = n
			}
			continue
		}
		rest = append(rest, a)
	}
	return rest, pid, found
}

// AfterRelaunch is called first thing in main with os.Args[1:]. On a normal
// start it returns args unchanged. Started by an update, it waits for the
// old process to exit, removes the old copy, and returns args without the
// flag.
func AfterRelaunch(args []string) []string {
	rest, pid, found := StripRelaunch(args)
	if !found {
		return args
	}
	if pid > 0 && pid != os.Getpid() {
		waitForExit(pid, relaunchWait)
	}
	cleanupRunning()
	return rest
}

// CleanupRunning removes what the last update left next to the running
// copy, if anything. Safe on every start: it runs once code-link is the
// only instance.
func CleanupRunning() {
	if exe, err := RunningExe(); err == nil {
		if p, err := PlanFor(runtime.GOOS, exe); err == nil {
			p.Cleanup()
		}
	}
}

// RunningExe is the running executable, symlinks resolved.
func RunningExe() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}
