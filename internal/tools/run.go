package tools

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

const tailLines = 15

// runCmd runs c with no stdin, sends each line of its output (stdout and
// stderr together, progress-bar carriage returns and colours removed) to
// progress, and returns the last lines. Cancelling ctx kills the process
// and everything it started.
func runCmd(ctx context.Context, c Cmd, progress func(string)) ([]string, error) {
	cmd := exec.CommandContext(ctx, c.Path, c.Args...)
	cmd.Env = mergeEnv(os.Environ(), c.Env)
	cmd.Stdin = nil
	prepare(cmd) // own process group (killed as one), no console window
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Start(); err != nil {
		r.Close()
		w.Close()
		return nil, err
	}
	w.Close()
	t := &tail{progress: progress}
	done := make(chan struct{})
	go func() {
		t.read(r)
		close(done)
	}()
	err = cmd.Wait()
	// Output ends when the last process holding the pipe exits; don't wait
	// long for a background child that keeps it open.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		r.Close()
		<-done
	}
	r.Close()
	return t.lines(), err
}

// mergeEnv is base with extra's entries replacing any of the same name
// (case-insensitively, as on Windows).
func mergeEnv(base, extra []string) []string {
	if len(extra) == 0 {
		return base
	}
	drop := map[string]bool{}
	for _, kv := range extra {
		k, _, _ := strings.Cut(kv, "=")
		drop[strings.ToUpper(k)] = true
	}
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if !drop[strings.ToUpper(k)] {
			out = append(out, kv)
		}
	}
	return append(out, extra...)
}

// ansi matches terminal escape sequences: colours, cursor moves, titles.
var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-Z\\-_]`)

// CleanLine removes terminal escapes and surrounding space.
func CleanLine(s string) string {
	return strings.TrimSpace(ansi.ReplaceAllString(s, ""))
}

// tail sends lines on and keeps the last few.
type tail struct {
	progress func(string)
	mu       sync.Mutex
	last     []string
}

func (t *tail) add(line string) {
	line = CleanLine(line)
	if line == "" {
		return
	}
	t.mu.Lock()
	t.last = append(t.last, line)
	if len(t.last) > tailLines {
		t.last = t.last[len(t.last)-tailLines:]
	}
	t.mu.Unlock()
	if t.progress != nil {
		t.progress(line)
	}
}

func (t *tail) lines() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.last...)
}

// read splits r into lines at \n and \r (progress bars redraw with \r).
func (t *tail) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
			return i + 1, data[:i], nil
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	for sc.Scan() {
		t.add(sc.Text())
	}
}

// exitCode is the process's exit status, -1 when it didn't exit normally.
func exitCode(err error) int {
	var ee interface{ ExitCode() int } // *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// failed is the plain-words error for a process that failed: "what (exit 1)"
// with the tail of its output.
func failed(what string, tail []string, err error) error {
	if code := exitCode(err); code >= 0 {
		return &Error{Message: fmt.Sprintf("%s (exit %d).", what, code), Tail: tail, Err: err}
	}
	var ee *exec.Error
	if errors.As(err, &ee) {
		return &Error{Message: fmt.Sprintf("%s: %v.", what, ee.Err), Tail: tail, Err: err}
	}
	return &Error{Message: fmt.Sprintf("%s: %v.", what, err), Tail: tail, Err: err}
}

// readVersion runs `path --version` and finds the version in its output.
func readVersion(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	name, args := path, []string{"--version"}
	if strings.HasSuffix(strings.ToLower(path), ".ps1") {
		name, args = powerShell(), []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", path, "--version"}
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = nil
	prepare(cmd)
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	if v := ParseVersion(firstLines(string(out), 5)); v != "" {
		return v, nil
	}
	if err != nil {
		if ctx.Err() != nil {
			return "", errors.New("it didn't answer `--version` in time")
		}
		return "", err
	}
	return "", fmt.Errorf("no version in %q", firstLines(CleanLine(string(out)), 1))
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
