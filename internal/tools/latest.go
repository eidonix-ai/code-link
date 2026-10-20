package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// latest is the newest release for the install's channel, "" when no feed
// answered. Lookups are cached for 6 hours (failures for 10 minutes), so
// a Settings window polling Status costs one request per feed.
//
//   - Claude Code: Anthropic's release feed, the one its installers and
//     `claude update` use, on the channel the install follows (the
//     autoUpdatesChannel setting, or the Homebrew cask's); the npm
//     registry's matching dist-tag when the feed is unreachable, and for
//     npm installs (which `npm install -g …@latest` moves to latest).
//   - Codex: the latest GitHub release's tag, with the npm registry's latest
//     as the fallback (and the first choice for npm installs).
func (m *Manager) latest(ctx context.Context, t Tool, in install) string {
	if t == Codex {
		gh := func() string { return m.cachedVersion(ctx, m.githubLatestURL(), m.fetchGitHubTag) }
		npm := func() string { return m.cachedVersion(ctx, m.npmURL(t, "latest"), m.fetchNPM) }
		if in.method == NPM {
			return first(npm, gh)
		}
		return first(gh, npm)
	}
	channel := "latest"
	switch in.method {
	case Brew:
		channel = in.channel
	case NPM:
	default:
		channel = m.claudeChannel()
	}
	feed := func() string {
		return m.cachedVersion(ctx, strings.TrimRight(m.Sources.ClaudeReleases, "/")+"/"+channel, m.fetchText)
	}
	npm := func() string { return m.cachedVersion(ctx, m.npmURL(t, channel), m.fetchNPM) }
	if in.method == NPM {
		return first(npm, feed)
	}
	return first(feed, npm)
}

func first(fs ...func() string) string {
	for _, f := range fs {
		if v := f(); v != "" {
			return v
		}
	}
	return ""
}

// claudeChannel is the autoUpdatesChannel in ~/.claude/settings.json
// ("stable" or "latest", the default). Read only.
func (m *Manager) claudeChannel() string {
	b, err := os.ReadFile(m.Env.join(m.Env.Home, ".claude", "settings.json"))
	if err != nil {
		return "latest"
	}
	var s struct {
		AutoUpdatesChannel string `json:"autoUpdatesChannel"`
	}
	if json.Unmarshal(b, &s) == nil && s.AutoUpdatesChannel == "stable" {
		return "stable"
	}
	return "latest"
}

func (m *Manager) npmURL(t Tool, tag string) string {
	return strings.TrimRight(m.Sources.NPMRegistry, "/") + "/" + npmPackage(t) + "/" + tag
}

func (m *Manager) githubLatestURL() string {
	return strings.TrimRight(m.Sources.GitHubAPI, "/") + "/repos/" + m.Sources.CodexRepo + "/releases/latest"
}

func (m *Manager) cachedVersion(parent context.Context, u string, fetch func(context.Context, string) (string, error)) string {
	now := m.now()
	m.mu.Lock()
	c, ok := m.cache[u]
	m.mu.Unlock()
	if ok && (c.ok && now.Sub(c.at) < latestTTL || !c.ok && now.Sub(c.at) < failedTTL) {
		return c.version
	}
	ctx, cancel := context.WithTimeout(parent, lookupTimeout)
	defer cancel()
	v, err := fetch(ctx, u)
	if err == nil {
		if v = ParseVersion(v); v == "" {
			err = errors.New("no version")
		}
	}
	if parent.Err() != nil {
		return "" // the caller gave up: don't cache that
	}
	m.mu.Lock()
	if m.cache == nil {
		m.cache = map[string]cached{}
	}
	m.cache[u] = cached{version: v, at: now, ok: err == nil}
	m.mu.Unlock()
	if err != nil {
		return ""
	}
	return v
}

// get fetches an https URL (small replies only).
func (m *Manager) get(ctx context.Context, u string, accept string) ([]byte, error) {
	resp, err := m.open(ctx, u, accept)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

// open starts an https GET and checks the status.
func (m *Manager) open(ctx context.Context, u string, accept string) (*http.Response, error) {
	pu, err := url.Parse(u)
	if err != nil || pu.Scheme != "https" {
		return nil, fmt.Errorf("refusing non-https URL %q", u)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "code-link")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if strings.Contains(accept, "github") {
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s answered %s", pu.Host, resp.Status)
	}
	return resp, nil
}

func (m *Manager) fetchText(ctx context.Context, u string) (string, error) {
	b, err := m.get(ctx, u, "")
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if len(s) > 64 || strings.ContainsAny(s, "<{ \n") {
		return "", errors.New("unexpected reply")
	}
	return s, nil
}

func (m *Manager) fetchNPM(ctx context.Context, u string) (string, error) {
	b, err := m.get(ctx, u, "application/json")
	if err != nil {
		return "", err
	}
	var doc struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return "", err
	}
	return doc.Version, nil
}

func (m *Manager) fetchGitHubTag(ctx context.Context, u string) (string, error) {
	rel, err := m.fetchRelease(ctx, u)
	if err != nil {
		return "", err
	}
	return releaseVersion(rel)
}

// release is the part of GitHub's release JSON used here.
type release struct {
	TagName    string  `json:"tag_name"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Assets     []asset `json:"assets"`
}

type asset struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	URL    string `json:"browser_download_url"`
	Digest string `json:"digest"` // "sha256:<hex>", computed by GitHub on upload
}

func (m *Manager) fetchRelease(ctx context.Context, u string) (release, error) {
	var rel release
	b, err := m.get(ctx, u, "application/vnd.github+json")
	if err != nil {
		return rel, err
	}
	err = json.Unmarshal(b, &rel)
	return rel, err
}

// releaseVersion is a Codex CLI release's version: tags are rust-v<version>.
func releaseVersion(rel release) (string, error) {
	if rel.Draft || rel.Prerelease || !strings.HasPrefix(rel.TagName, "rust-v") {
		return "", fmt.Errorf("%q isn't a Codex CLI release", rel.TagName)
	}
	v := ParseVersion(rel.TagName)
	if v == "" || strings.Contains(v, "-") {
		return "", fmt.Errorf("%q isn't a Codex CLI release", rel.TagName)
	}
	return v, nil
}

func newHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 30 * time.Second
	tr.TLSHandshakeTimeout = 15 * time.Second
	return &http.Client{Transport: tr, CheckRedirect: httpsOnlyRedirects}
}

// httpsOnlyRedirects follows up to 10 redirects, never to plain http
// (GitHub's downloads redirect to its object store).
func httpsOnlyRedirects(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("too many redirects")
	}
	if req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect to %s", req.URL.Redacted())
	}
	return nil
}
