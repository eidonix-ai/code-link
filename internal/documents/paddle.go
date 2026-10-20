package documents

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"codelink/internal/jsonx"
)

// paddleClient is a PaddleOCR(-VL) /layout-parsing server.
// Request: {file: base64, fileType: 0 (PDF) | 1 (image), returnMarkdownImages: false, visualize: false}.
// Response: {errorCode, errorMsg, result: {layoutParsingResults: [{markdown: {text}}]}}, one entry per page.
type paddleClient struct {
	endpoint     string
	client       *http.Client
	probeTTL     time.Duration
	probeTimeout time.Duration
	readTimeout  time.Duration
	now          func() time.Time

	mu     sync.Mutex
	probed *probe
}

type probe struct {
	reachable bool
	at        time.Time
}

// paddleError is why PaddleOCR could not read a document.
type paddleError struct {
	kind    string // unreachable, http, server, empty, unsupported
	status  int
	message string
}

func (e *paddleError) Error() string {
	switch e.kind {
	case "http":
		return fmt.Sprintf("HTTP %d", e.status)
	case "server":
		return e.message
	case "empty":
		return "no text returned"
	case "unreachable":
		return "connection failed"
	}
	return e.kind
}

var errPaddleUnsupported = &paddleError{kind: "unsupported"}

func newPaddle(endpoint string, client *http.Client) *paddleClient {
	if client == nil {
		client = http.DefaultClient
	}
	return &paddleClient{endpoint: endpoint, client: client, probeTTL: time.Minute,
		probeTimeout: 3 * time.Second, readTimeout: 300 * time.Second, now: time.Now}
}

// reachable is a cheap probe, cached: any HTTP answer below 500 means the
// server is up (the endpoint itself answers a bare GET with 405).
// Connection errors, timeouts and gateway errors mean it is not.
func (p *paddleClient) reachable(ctx context.Context) bool {
	now := p.now()
	p.mu.Lock()
	if p.probed != nil && now.Sub(p.probed.at) < p.probeTTL {
		ok := p.probed.reachable
		p.mu.Unlock()
		return ok
	}
	p.mu.Unlock()
	probeCtx, cancel := context.WithTimeout(ctx, p.probeTimeout)
	defer cancel()
	ok := false
	if req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, p.endpoint, nil); err == nil {
		if resp, err := p.client.Do(req); err == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			ok = resp.StatusCode < 500
		}
	}
	if ctx.Err() == nil { // the caller going away says nothing about the server
		p.remember(ok, now)
	}
	return ok
}

func (p *paddleClient) remember(reachable bool, at time.Time) {
	p.mu.Lock()
	p.probed = &probe{reachable, at}
	p.mu.Unlock()
}

// read gives the page texts in order (trimmed; a page may be empty).
func (p *paddleClient) read(ctx context.Context, data []byte, mediaType string) ([]string, error) {
	var fileType int
	switch {
	case mediaType == "application/pdf":
		fileType = 0
	case strings.HasPrefix(mediaType, "image/"):
		fileType = 1
	default:
		return nil, errPaddleUnsupported
	}
	body := jsonx.Marshal(map[string]any{
		"file": base64.StdEncoding.EncodeToString(data), "fileType": fileType,
		"returnMarkdownImages": false, "visualize": false,
	})
	ctx, cancel := context.WithTimeout(ctx, p.readTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, &paddleError{kind: "unreachable"}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			p.remember(false, p.now()) // a dead server: skip it for the next minute
		}
		return nil, &paddleError{kind: "unreachable"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &paddleError{kind: "http", status: resp.StatusCode}
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &paddleError{kind: "unreachable"}
	}
	j, err := jsonx.Parse(raw)
	if err != nil {
		return nil, &paddleError{kind: "server", message: "invalid response"}
	}
	if code, ok := jsonx.Float(jsonx.Get(j, "errorCode")); ok && code != 0 {
		msg := jsonx.Str(jsonx.Get(j, "errorMsg"))
		if msg == "" {
			msg = fmt.Sprintf("error %d", int(code))
		}
		return nil, &paddleError{kind: "server", message: msg}
	}
	var texts []string
	found := false
	for _, page := range jsonx.Arr(jsonx.Get(j, "result", "layoutParsingResults")) {
		t := strings.TrimSpace(jsonx.Str(jsonx.Get(page, "markdown", "text")))
		texts = append(texts, t)
		found = found || t != ""
	}
	if !found {
		return nil, &paddleError{kind: "empty"}
	}
	return texts, nil
}
