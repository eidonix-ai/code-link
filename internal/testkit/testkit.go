// Package testkit is the shared test harness: a scripted stub backend, an
// engine started against it on a free port, fakes for every hook, and SSE
// parsing. Ported from the Swift CoreHarness; tests of every package use it.
package testkit

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"codelink/internal/config"
	"codelink/internal/core"
	"codelink/internal/hooks"
	"codelink/internal/jsonx"
)

// Seen is a request the stub backend received.
type Seen struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// JSON is the decoded body (nil when it isn't JSON).
func (s Seen) JSON() map[string]any { return jsonx.ParseObject(s.Body) }

// Reply is what the stub backend answers.
type Reply interface{ isReply() }

// JSONReply is a plain response.
type JSONReply struct {
	Status int
	Body   string
	Header map[string]string
}

// SSEReply streams "data: <chunk>" events, then "data: [DONE]" when Done.
// Every: pause between chunks; After: pause before the first.
type SSEReply struct {
	Chunks []string
	Done   bool
	Every  time.Duration
	After  time.Duration
}

// RawStream sends the text as an event stream, as is.
type RawStream struct{ Text string }

// Delayed answers Reply after a pause.
type Delayed struct {
	After time.Duration
	Reply Reply
}

// Endless streams a content chunk every interval until the client goes.
type Endless struct{ Every time.Duration }

func (JSONReply) isReply() {}
func (SSEReply) isReply()  {}
func (RawStream) isReply() {}
func (Delayed) isReply()   {}
func (Endless) isReply()   {}

// JSONR is a JSONReply.
func JSONR(status int, body string) JSONReply { return JSONReply{Status: status, Body: body} }

// SSE is an SSEReply ending in [DONE].
func SSE(chunks ...string) SSEReply { return SSEReply{Chunks: chunks, Done: true} }

// Stub is a scripted backend. The handler gets each request and its
// 0-based number among non-/models requests.
type Stub struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []Seen
	n    int
	// Disconnected counts streams whose client went away.
	disconnected int
}

// StartStub starts a stub backend; GET …/models lists glm-upstream
// (max_model_len 131072) and other (8192).
func StartStub(t testing.TB, handler func(Seen, int) Reply) *Stub {
	s := &Stub{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen := Seen{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models") {
			s.write(w, r, JSONR(200, `{"object":"list","data":[{"id":"glm-upstream","max_model_len":131072},{"id":"other","max_model_len":8192}]}`))
			return
		}
		s.mu.Lock()
		s.seen = append(s.seen, seen)
		n := s.n
		s.n++
		s.mu.Unlock()
		s.write(w, r, handler(seen, n))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *Stub) write(w http.ResponseWriter, r *http.Request, reply Reply) {
	switch rp := reply.(type) {
	case JSONReply:
		for k, v := range rp.Header {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rp.Status)
		io.WriteString(w, rp.Body)
	case Delayed:
		select {
		case <-time.After(rp.After):
			s.write(w, r, rp.Reply)
		case <-r.Context().Done():
		}
	case RawStream:
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, rp.Text)
	case SSEReply:
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		f := w.(http.Flusher)
		f.Flush()
		if !s.sleep(r, rp.After) {
			return
		}
		for i, c := range rp.Chunks {
			if i > 0 && !s.sleep(r, rp.Every) {
				return
			}
			io.WriteString(w, "data: "+c+"\n\n")
			f.Flush()
		}
		if rp.Done {
			io.WriteString(w, "data: [DONE]\n\n")
		}
	case Endless:
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		f := w.(http.Flusher)
		for {
			if _, err := io.WriteString(w, "data: "+Content("x")+"\n\n"); err != nil {
				s.gone()
				return
			}
			f.Flush()
			if !s.sleep(r, rp.Every) {
				return
			}
		}
	default:
		w.WriteHeader(500)
	}
}

func (s *Stub) sleep(r *http.Request, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	select {
	case <-time.After(d):
		return true
	case <-r.Context().Done():
		s.gone()
		return false
	}
}

func (s *Stub) gone() {
	s.mu.Lock()
	s.disconnected++
	s.mu.Unlock()
}

// URL is the stub's base URL with /v1.
func (s *Stub) URL() string { return s.srv.URL + "/v1" }

// Requests are the non-/models requests so far.
func (s *Stub) Requests() []Seen {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Seen(nil), s.seen...)
}

// ChatRequests are the decoded /chat/completions bodies.
func (s *Stub) ChatRequests() []map[string]any {
	var out []map[string]any
	for _, r := range s.Requests() {
		if strings.HasSuffix(r.Path, "/chat/completions") {
			out = append(out, r.JSON())
		}
	}
	return out
}

// LastChat is the last /chat/completions body.
func (s *Stub) LastChat() map[string]any {
	c := s.ChatRequests()
	if len(c) == 0 {
		return nil
	}
	return c[len(c)-1]
}

// Disconnected counts streams whose client went away.
func (s *Stub) Disconnected() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.disconnected
}

// Chunk helpers: Chat Completions stream chunks.

func Chunk(delta string, finish string, extra string) string {
	fr := "null"
	if finish != "" {
		fr = strconv.Quote(finish)
	}
	return `{"id":"c","object":"chat.completion.chunk","model":"glm-upstream","choices":[{"index":0,"delta":` + delta + `,"finish_reason":` + fr + extra + `}]}`
}
func Content(s string) string   { return Chunk(`{"content":`+jsonx.String(s)+`}`, "", "") }
func Reasoning(s string) string { return Chunk(`{"reasoning_content":`+jsonx.String(s)+`}`, "", "") }
func Finish(reason string) string {
	if reason == "" {
		reason = "stop"
	}
	return Chunk("{}", reason, "")
}
func Usage(prompt, completion, cached int) string {
	return fmt.Sprintf(`{"id":"c","choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"prompt_tokens_details":{"cached_tokens":%d}}}`, prompt, completion, cached)
}
func ToolCall(index int, id, name, args string) string {
	fn := `"arguments":` + jsonx.String(args)
	if name != "" {
		fn = `"name":"` + name + `",` + fn
	}
	idPart := ""
	if id != "" {
		idPart = `"id":"` + id + `","type":"function",`
	}
	return Chunk(`{"tool_calls":[{"index":`+strconv.Itoa(index)+`,`+idPart+`"function":{`+fn+`}}]}`, "", "")
}

// Completion is a non-streaming Chat Completions body. content "" with
// nullContent = null content.
type Completion struct {
	Content     *string
	Reasoning   string
	ToolCalls   string // raw JSON array
	Finish      string // "" = stop
	StopReason  string
	Prompt      int // 0 = 10
	CompletionN int // 0 = 5
	Cached      int
}

func (c Completion) String() string {
	content := "null"
	if c.Content != nil {
		content = jsonx.String(*c.Content)
	}
	msg := `"role":"assistant","content":` + content
	if c.Reasoning != "" {
		msg += `,"reasoning_content":` + jsonx.String(c.Reasoning)
	}
	if c.ToolCalls != "" {
		msg += `,"tool_calls":` + c.ToolCalls
	}
	finish := c.Finish
	if finish == "" {
		finish = "stop"
	}
	sr := ""
	if c.StopReason != "" {
		sr = `,"stop_reason":"` + c.StopReason + `"`
	}
	p, n := c.Prompt, c.CompletionN
	if p == 0 {
		p = 10
	}
	if n == 0 {
		n = 5
	}
	return fmt.Sprintf(`{"id":"c","object":"chat.completion","model":"glm-upstream","choices":[{"index":0,"message":{%s},"finish_reason":"%s"%s}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d,"prompt_tokens_details":{"cached_tokens":%d}}}`,
		msg, finish, sr, p, n, p+n, c.Cached)
}

// Text is a completion with content s.
func Text(s string) string { return Completion{Content: &s}.String() }

// Fakes for the hooks.

// Log records events.
type Log struct {
	mu     sync.Mutex
	events []hooks.Event
}

func (l *Log) Record(e hooks.Event) { l.mu.Lock(); l.events = append(l.events, e); l.mu.Unlock() }
func (l *Log) All() []hooks.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]hooks.Event(nil), l.events...)
}

// DocReader answers every document with Text (or Fail as the error).
type DocReader struct {
	mu    sync.Mutex
	Text  string
	Fail  string
	calls []hooks.DocumentInput
}

func (d *DocReader) Read(_ context.Context, in hooks.DocumentInput) hooks.DocumentOutput {
	d.mu.Lock()
	d.calls = append(d.calls, in)
	d.mu.Unlock()
	if d.Fail != "" {
		return hooks.DocumentOutput{Error: d.Fail, Via: "fake"}
	}
	t := d.Text
	if t == "" {
		t = fmt.Sprintf("EXTRACTED %d bytes of %s", len(in.Data), in.MediaType)
	}
	return hooks.DocumentOutput{Text: t, Pages: 1, Via: "fake"}
}
func (d *DocReader) Calls() []hooks.DocumentInput {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]hooks.DocumentInput(nil), d.calls...)
}

// Describer describes every image as Text ("a cat on a mat" by default).
type Describer struct {
	mu    sync.Mutex
	Text  string
	calls int
}

func (d *Describer) Describe(context.Context, []byte, string) string {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	if d.Text == "" {
		return "a cat on a mat"
	}
	return d.Text
}
func (d *Describer) Calls() int { d.mu.Lock(); defer d.mu.Unlock(); return d.calls }

// Searcher answers with two hits per query, or Err.
type Searcher struct {
	mu      sync.Mutex
	Err     error
	queries []string
}

func (s *Searcher) Search(_ context.Context, q string, _, _ []string) ([]hooks.SearchHit, error) {
	s.mu.Lock()
	s.queries = append(s.queries, q)
	s.mu.Unlock()
	if s.Err != nil {
		return nil, s.Err
	}
	return []hooks.SearchHit{
		{Title: "Result for " + q, URL: "https://example.com/1", Snippet: "Snippet about " + q, PageAge: "2026-01-01"},
		{Title: "Second", URL: "https://example.com/2", Snippet: "More"},
	}, nil
}
func (s *Searcher) Queries() []string { s.mu.Lock(); defer s.mu.Unlock(); return append([]string(nil), s.queries...) }

// Factory gives fixed hooks for every configuration.
func Factory(h hooks.Hooks) hooks.Factory {
	return func(*config.Config, hooks.ModelCaller, hooks.EventLog) hooks.Hooks { return h }
}

// Engine is what Harness needs from the engine (avoids an import cycle:
// engine tests use testkit).
type Engine interface {
	Start() error
	Stop()
	Port() int
}

// Token is the harness access token.
const Token = "test-token-123"

// DefaultConfig: models GLM (glm-upstream, 200k), Small (32k), Big (400k)
// on the stub; roles main GLM, opus Big, small Small.
func DefaultConfig(stub *Stub) config.Config {
	w := func(n int) *int { return &n }
	return config.Config{
		Port: 0, AccessToken: Token,
		Backends: []config.Backend{{ID: "stub", Kind: config.OpenAICompatible, BaseURL: stub.URL(), APIKey: "sk-stub-secret"}},
		Models: []config.Model{
			{ID: "GLM", BackendID: "stub", Upstream: "glm-upstream", ContextWindow: w(200_000)},
			{ID: "Small", BackendID: "stub", Upstream: "small-upstream", ContextWindow: w(32_000)},
			{ID: "Big", BackendID: "stub", Upstream: "big-upstream", ContextWindow: w(400_000)},
		},
		Roles:     config.Roles{Main: "GLM", Opus: "Big", Small: "Small"},
		Documents: config.Documents{Local: config.LocalOCR},
	}
}

// FastTuning: 100 ms pings.
func FastTuning() core.Tuning {
	t := core.DefaultTuning()
	t.PingInterval = 100 * time.Millisecond
	return t
}

// Client talks to a running engine.
type Client struct {
	Port int
	HTTP *http.Client
}

// HTTPReply is a response from the engine.
type HTTPReply struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r HTTPReply) Text() string          { return string(r.Body) }
func (r HTTPReply) JSON() map[string]any { return jsonx.ParseObject(r.Body) }
func (r HTTPReply) Events() []SSEEvent    { return ParseSSE(string(r.Body)) }

// Request sends a request; auth adds the access token.
func (c Client) Request(method, path, body string, header map[string]string, auth bool) (HTTPReply, error) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", c.Port, path), rd)
	if err != nil {
		return HTTPReply{}, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+Token)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return HTTPReply{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return HTTPReply{Status: resp.StatusCode, Header: resp.Header, Body: b}, err
}

// Messages posts to /v1/messages.
func (c Client) Messages(body string) (HTTPReply, error) {
	return c.Request("POST", "/v1/messages", body, nil, true)
}

// MessagesBody is a minimal Messages request; messages "" = one "hi".
func MessagesBody(extra, model string, stream bool, messages string) string {
	if model == "" {
		model = "GLM"
	}
	if messages == "" {
		messages = `[{"role":"user","content":"hi"}]`
	}
	s := `{"model":"` + model + `","max_tokens":100,"stream":` + strconv.FormatBool(stream) + `,"messages":` + messages
	if extra != "" {
		s += "," + extra
	}
	return s + "}"
}

// SSEEvent is one parsed event.
type SSEEvent struct {
	Event string
	Data  map[string]any
}

// ParseSSE splits an event stream into named events.
func ParseSSE(s string) []SSEEvent {
	var out []SSEEvent
	for _, block := range strings.Split(s, "\n\n") {
		var name, data string
		for _, line := range strings.Split(block, "\n") {
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				name = v
			}
			if v, ok := strings.CutPrefix(line, "data: "); ok {
				data = v
			}
		}
		if name != "" {
			out = append(out, SSEEvent{Event: name, Data: jsonx.ParseObject([]byte(data))})
		}
	}
	return out
}

// Names are the event names.
func Names(ev []SSEEvent) []string {
	var n []string
	for _, e := range ev {
		n = append(n, e.Event)
	}
	return n
}

// WithoutPings drops ping events.
func WithoutPings(ev []SSEEvent) []SSEEvent {
	var out []SSEEvent
	for _, e := range ev {
		if e.Event != "ping" {
			out = append(out, e)
		}
	}
	return out
}

// DataLines are the "data: " payloads of a Chat Completions stream.
func DataLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if v, ok := strings.CutPrefix(line, "data: "); ok {
			out = append(out, v)
		}
	}
	return out
}

// CheckBlocksBalanced: every content_block_start has a unique index and
// exactly one stop, and deltas fall inside open blocks. Returns index → type.
func CheckBlocksBalanced(t testing.TB, ev []SSEEvent) map[int]string {
	t.Helper()
	started := map[int]string{}
	stopped := map[int]bool{}
	for _, e := range ev {
		idx, ok := jsonx.Int(e.Data["index"])
		if !ok {
			continue
		}
		switch e.Event {
		case "content_block_start":
			if _, dup := started[idx]; dup {
				t.Errorf("duplicate block index %d", idx)
			}
			started[idx] = jsonx.Type(e.Data["content_block"])
		case "content_block_stop":
			if _, ok := started[idx]; !ok || stopped[idx] {
				t.Errorf("unbalanced stop %d", idx)
			}
			stopped[idx] = true
		case "content_block_delta":
			if _, ok := started[idx]; !ok || stopped[idx] {
				t.Errorf("delta outside block %d", idx)
			}
		}
	}
	if len(started) != len(stopped) {
		t.Errorf("blocks left open: started %v stopped %v", started, stopped)
	}
	return started
}

// Compact re-encodes JSON text without spaces, for comparisons.
func Compact(s string) string {
	var b bytes.Buffer
	v, err := jsonx.Parse([]byte(s))
	if err != nil {
		return s
	}
	b.Write(jsonx.Marshal(v))
	return b.String()
}
