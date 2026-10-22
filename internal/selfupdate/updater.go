package selfupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// CheckInterval is how often the feed is checked after the check at start.
const CheckInterval = 30 * time.Minute

const (
	checkTimeout    = 30 * time.Second
	downloadTimeout = 30 * time.Minute
	maxManifest     = 1 << 20
	maxSignature    = 4096
)

// Options configure an Updater; the zero values are the real ones.
type Options struct {
	// Current is the running version (from the build); "dev" or empty turns
	// updates off.
	Current     string
	ManifestURL string
	// PublicKey verifies the manifest; nil = the embedded key.
	PublicKey ed25519.PublicKey
	Client    *http.Client
	// GOOS and Platform ("darwin-arm64") pick the plan and the download.
	GOOS     string
	Platform string
	// OnChange hears every change of State (from any goroutine).
	OnChange func()

	// Seams for tests.
	Exe     func() (string, error)
	Extract func(zip, dest string) error
	Start   func(name string, args []string) error
	Pid     func() int
	After   func(time.Duration) <-chan time.Time
	Now     func() time.Time
}

// Phase is where an update is.
type Phase string

const (
	PhaseDownload  Phase = "download"
	PhaseVerify    Phase = "verify"
	PhaseInstall   Phase = "install"
	PhaseRestart   Phase = "restart"
	PhaseFailed    Phase = "failed"
	PhaseCancelled Phase = "cancelled"
)

// Job is an update as its progress window shows it.
type Job struct {
	Version string `json:"version"`
	Phase   Phase  `json:"phase"`
	Running bool   `json:"running"`
	// Cancellable until the swap starts.
	Cancellable bool  `json:"cancellable"`
	Received    int64 `json:"received"`
	Total       int64 `json:"total"`
	// Message is a failure in plain words; Detail the technical reason.
	Message string `json:"message"`
	Detail  string `json:"detail"`
}

// State is what the tray and Settings show.
type State struct {
	Current string `json:"current"`
	// Disabled says why this build doesn't update ("" = it does).
	Disabled  string `json:"disabled"`
	Checking  bool   `json:"checking"`
	CheckedAt string `json:"checkedAt"` // RFC 3339; "" = not yet
	// Error is the last check's problem in plain words.
	Error     string `json:"error"`
	Latest    string `json:"latest"`
	Notes     string `json:"notes"`
	Available bool   `json:"available"`
	// RestartNeeded: the new version is in place but code-link couldn't
	// start it; quitting and reopening finishes the update.
	RestartNeeded bool `json:"restartNeeded"`
	Job           Job  `json:"job"`
}

// Updater checks the feed and applies updates.
type Updater struct {
	opt Options
	pub ed25519.PublicKey

	mu        sync.Mutex
	disabled  string
	checking  bool
	checkedAt time.Time
	checkErr  string
	manifest  *Manifest
	restart   bool
	job       Job
	cancel    context.CancelFunc
}

// New builds an Updater; it does nothing until Watch or Check.
func New(opt Options) *Updater {
	if opt.ManifestURL == "" {
		opt.ManifestURL = DefaultManifestURL
	}
	if opt.Client == nil {
		opt.Client = &http.Client{}
	}
	if opt.GOOS == "" {
		opt.GOOS = runtime.GOOS
	}
	if opt.Platform == "" {
		opt.Platform = runtime.GOOS + "-" + runtime.GOARCH
	}
	if opt.Exe == nil {
		opt.Exe = RunningExe
	}
	if opt.Extract == nil {
		opt.Extract = extractZip
	}
	if opt.Start == nil {
		opt.Start = startDetached
	}
	if opt.Pid == nil {
		opt.Pid = os.Getpid
	}
	if opt.After == nil {
		opt.After = time.After
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	u := &Updater{opt: opt, pub: opt.PublicKey}
	switch {
	case !isVersion(opt.Current):
		u.disabled = "This is a development build, so it doesn't update itself."
	case opt.GOOS != "darwin" && opt.GOOS != "windows":
		u.disabled = "code-link updates itself only on macOS and Windows."
	case u.pub == nil:
		pub, err := PublicKey()
		if err != nil {
			u.disabled = "This build isn't set up to update itself."
		}
		u.pub = pub
	}
	return u
}

func isVersion(v string) bool { _, ok := ParseVersion(v); return ok }

// State is the current state.
func (u *Updater) State() State {
	u.mu.Lock()
	defer u.mu.Unlock()
	s := State{Current: u.opt.Current, Disabled: u.disabled, Checking: u.checking, Error: u.checkErr,
		RestartNeeded: u.restart, Job: u.job}
	if s.Current == "" {
		s.Current = "dev"
	}
	if !u.checkedAt.IsZero() {
		s.CheckedAt = u.checkedAt.UTC().Format(time.RFC3339)
	}
	if m := u.manifest; m != nil {
		s.Latest, s.Notes = m.Version, m.Notes
		_, has := m.Files[u.opt.Platform]
		s.Available = has && !u.restart && Newer(m.Version, u.opt.Current)
	}
	return s
}

// MenuLabel is the tray's item for code-link itself ("" = none): offered
// when an update is available, kept while one runs (it shows the progress),
// and after an update that couldn't restart (it quits).
func MenuLabel(s State) string {
	switch {
	case s.RestartNeeded:
		return "Quit and reopen code-link to finish updating"
	case s.Job.Running:
		return "Updating code-link to " + s.Job.Version + "…"
	case s.Available:
		return "Update code-link to " + s.Latest + "…"
	}
	return ""
}

func (u *Updater) changed() {
	if f := u.opt.OnChange; f != nil {
		f()
	}
}

// Watch checks now and then every CheckInterval until ctx ends. Failures
// are quiet: they only mean no update is offered.
func (u *Updater) Watch(ctx context.Context) {
	for {
		u.Check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-u.opt.After(CheckInterval):
		}
	}
}

// Check reads the feed now and returns the new state.
func (u *Updater) Check(ctx context.Context) State {
	u.mu.Lock()
	if u.disabled != "" || u.checking {
		u.mu.Unlock()
		return u.State()
	}
	u.checking = true
	u.mu.Unlock()
	u.changed()

	m, err := u.fetch(ctx)
	u.mu.Lock()
	u.checking = false
	u.checkedAt = u.opt.Now()
	if err != nil {
		u.checkErr = checkMessage(err)
	} else {
		u.checkErr, u.manifest = "", m
	}
	u.mu.Unlock()
	u.changed()
	return u.State()
}

func checkMessage(err error) string {
	if errors.Is(err, ErrBadSignature) {
		return "Couldn't check for updates: " + ErrBadSignature.Error() + "."
	}
	return "Couldn't check for updates. Check your internet connection and try again later."
}

func (u *Updater) fetch(ctx context.Context) (*Manifest, error) {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	body, err := u.get(ctx, u.opt.ManifestURL, maxManifest)
	if err != nil {
		return nil, err
	}
	sig, err := u.get(ctx, u.opt.ManifestURL+SignatureSuffix, maxSignature)
	if err != nil {
		return nil, err
	}
	return Verify(body, sig, u.pub)
}

func (u *Updater) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := u.opt.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is too large", url)
	}
	return b, nil
}

// Start begins installing the available update in the background and
// returns at once. onRestart is called once the new copy has been started:
// the caller quits. Starting while one runs returns the running one.
func (u *Updater) Start(onRestart func()) State {
	u.mu.Lock()
	if u.job.Running {
		u.mu.Unlock()
		return u.State()
	}
	m := u.manifest
	ctx, cancel := context.WithCancel(context.Background())
	u.cancel = cancel
	version := ""
	if m != nil {
		version = m.Version
	}
	u.job = Job{Version: version, Phase: PhaseDownload, Running: true, Cancellable: true}
	u.mu.Unlock()
	u.changed()

	go func() {
		defer cancel()
		err := u.run(ctx, m)
		u.mu.Lock()
		u.job.Running, u.job.Cancellable = false, false
		switch {
		case err == nil:
			u.job.Phase = PhaseRestart
		case ctx.Err() != nil && (u.job.Phase == PhaseDownload || u.job.Phase == PhaseVerify):
			u.job.Phase, u.job.Message, u.job.Detail = PhaseCancelled, "The update was cancelled.", ""
		default:
			u.job.Phase = PhaseFailed
			var ue *UserError
			if errors.As(err, &ue) {
				u.job.Message, u.job.Detail = ue.Message, ue.Detail
			} else {
				u.job.Message, u.job.Detail = "The update didn't work.", err.Error()
			}
		}
		u.mu.Unlock()
		u.changed()
		if err == nil && onRestart != nil {
			onRestart()
		}
	}()
	return u.State()
}

// Cancel stops a running update while it is still downloading.
func (u *Updater) Cancel() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.job.Running && u.job.Cancellable && u.cancel != nil {
		u.cancel()
	}
}

func (u *Updater) phase(p Phase, cancellable bool) {
	u.mu.Lock()
	u.job.Phase, u.job.Cancellable = p, cancellable
	if p != PhaseDownload {
		u.job.Received = u.job.Total
	}
	u.mu.Unlock()
	u.changed()
}

func (u *Updater) run(ctx context.Context, m *Manifest) error {
	if u.disabled != "" {
		return &UserError{Message: u.disabled}
	}
	if m == nil || !Newer(m.Version, u.opt.Current) {
		return &UserError{Message: "There's no newer code-link to install right now."}
	}
	file, ok := m.Files[u.opt.Platform]
	if !ok {
		return &UserError{Message: "code-link " + m.Version + " isn't published for this computer yet."}
	}
	exe, err := u.opt.Exe()
	if err != nil {
		return userErr("code-link couldn't find where it's running from.", err)
	}
	plan, err := PlanFor(u.opt.GOOS, exe)
	if err != nil {
		return err
	}
	if err := plan.Prepare(); err != nil {
		return err
	}
	link, err := resolveURL(u.opt.ManifestURL, file.URL)
	if err != nil {
		return userErr("The update's download address isn't usable.", err)
	}

	sum, err := u.download(ctx, link, file, plan.Download)
	if err != nil {
		os.Remove(plan.Download)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	u.phase(PhaseVerify, true)
	if !strings.EqualFold(sum, file.SHA256) {
		os.Remove(plan.Download)
		return &UserError{Message: "The download didn't match the published update, so it wasn't installed. Try again later.",
			Detail: "SHA-256 " + sum + ", expected " + strings.ToLower(file.SHA256)}
	}
	if ctx.Err() != nil {
		os.Remove(plan.Download)
		return ctx.Err()
	}

	u.phase(PhaseInstall, false)
	if err := plan.Install(u.opt.Extract); err != nil {
		return err
	}
	u.phase(PhaseRestart, false)
	name, args := plan.Relaunch(u.opt.Pid())
	if err := u.opt.Start(name, args); err != nil {
		u.mu.Lock()
		u.restart = true
		u.mu.Unlock()
		return userErr("code-link "+m.Version+" is installed, but it couldn't restart by itself. Quit code-link and open it again.", err)
	}
	return nil
}

// download writes the file to dest, reporting progress, and returns its
// SHA-256. More or fewer bytes than the manifest says is refused.
func (u *Updater) download(ctx context.Context, link string, file File, dest string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	failed := func(err error) error {
		return userErr("Couldn't download the update. Check your internet connection and try again.", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return "", failed(err)
	}
	resp, err := u.opt.Client.Do(req)
	if err != nil {
		return "", failed(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", failed(fmt.Errorf("HTTP %d", resp.StatusCode))
	}
	wrongSize := func(got int64) error {
		return &UserError{Message: "The download didn't match the published update, so it wasn't installed. Try again later.",
			Detail: fmt.Sprintf("%d bytes, expected %d", got, file.Size)}
	}
	if resp.ContentLength >= 0 && resp.ContentLength != file.Size {
		return "", wrongSize(resp.ContentLength)
	}
	u.mu.Lock()
	u.job.Total = file.Size
	u.mu.Unlock()

	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return "", userErr("The update couldn't be saved next to the app.", err)
	}
	h := sha256.New()
	pw := &progressWriter{u: u, step: max(file.Size/100, 64<<10)}
	n, err := io.Copy(io.MultiWriter(out, h, pw), io.LimitReader(resp.Body, file.Size+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", failed(err)
	}
	if n != file.Size {
		return "", wrongSize(n)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type progressWriter struct {
	u        *Updater
	n, shown int64
	step     int64
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.n += int64(len(b))
	if p.n-p.shown >= p.step {
		p.shown = p.n
		p.u.mu.Lock()
		p.u.job.Received = p.n
		p.u.mu.Unlock()
		p.u.changed()
	}
	return len(b), nil
}

// resolveURL makes a file URL absolute against the manifest's and allows
// only http(s).
func resolveURL(base, ref string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	abs := b.ResolveReference(r)
	if abs.Scheme != "https" && abs.Scheme != "http" {
		return "", fmt.Errorf("unsupported address %q", ref)
	}
	return abs.String(), nil
}

// startDetached starts the new copy; it outlives this process.
func startDetached(name string, args []string) error {
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	if name == "/usr/bin/open" {
		// open returns once Launch Services has started the app.
		return cmd.Wait()
	}
	return cmd.Process.Release()
}
