// Package httpx is the engine's HTTP plumbing: JSON replies, access-token
// auth and server-sent event streams with keepalive pings.
package httpx

import (
	"crypto/subtle"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"codelink/internal/apierr"
	"codelink/internal/jsonx"
)

// WriteJSON sends v with status and extra headers.
func WriteJSON(w http.ResponseWriter, status int, v any, extra http.Header) {
	for k, vs := range extra {
		for _, x := range vs {
			w.Header().Add(k, x)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(jsonx.Marshal(v))
}

// WriteAnthropic sends an Anthropic error.
func WriteAnthropic(w http.ResponseWriter, e apierr.Anthropic) {
	WriteJSON(w, e.Status, e.Body(), e.Header)
}

// Token is the access token a request presents: Authorization: Bearer, or
// x-api-key.
func Token(r *http.Request) string {
	if a := r.Header.Get("Authorization"); a != "" {
		if k, v, ok := strings.Cut(a, " "); ok && strings.EqualFold(k, "bearer") {
			return strings.TrimSpace(v)
		}
	}
	return r.Header.Get("x-api-key")
}

// RequireToken answers 401 (Anthropic shape) unless the request carries token.
func RequireToken(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := Token(r)
		if token == "" || subtle.ConstantTimeCompare([]byte(p), []byte(token)) != 1 {
			WriteAnthropic(w, apierr.Unauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ReadBody reads at most max bytes; ok false (and a 413 sent) when larger.
func ReadBody(w http.ResponseWriter, r *http.Request, max int64) ([]byte, bool) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err != nil {
		WriteAnthropic(w, apierr.TooLarge)
		return nil, false
	}
	return b, true
}

// Stream is a server-sent event response. Events are written whole and
// flushed; a ping goes out whenever the stream has been quiet for the ping
// interval. Safe for concurrent use.
type Stream struct {
	w            http.ResponseWriter
	f            http.Flusher
	mu           sync.Mutex
	last         time.Time
	closed       bool
	failed       bool
	commentPings bool
	stop         chan struct{}
}

// StartStream sends the headers (status, text/event-stream) and starts
// pings every interval of silence (0 = none). commentPings: keepalives are
// SSE comments, for clients with no ping event (Responses, Chat
// Completions). Close it when done.
func StartStream(w http.ResponseWriter, status int, extra http.Header, interval time.Duration, commentPings bool) *Stream {
	for k, vs := range extra {
		for _, x := range vs {
			w.Header().Add(k, x)
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(status)
	f, _ := w.(http.Flusher)
	s := &Stream{w: w, f: f, last: time.Now(), commentPings: commentPings, stop: make(chan struct{})}
	if f != nil {
		f.Flush()
	}
	if interval > 0 {
		go s.pings(interval)
	}
	return s
}

func (s *Stream) pings(interval time.Duration) {
	t := time.NewTimer(interval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.mu.Lock()
			idle := time.Since(s.last)
			s.mu.Unlock()
			if idle >= interval {
				s.Ping()
				t.Reset(interval)
			} else {
				t.Reset(interval - idle)
			}
		}
	}
}

// Raw writes bytes as they are (a native stream passed through).
func (s *Stream) Raw(b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.failed {
		return
	}
	if _, err := s.w.Write(b); err != nil {
		s.failed = true
		return
	}
	if s.f != nil {
		s.f.Flush()
	}
	s.last = time.Now()
}

// Event writes "event: <name>" with data, its "type" set to name.
func (s *Stream) Event(name string, data map[string]any) {
	d := make(map[string]any, len(data)+1)
	for k, v := range data {
		d[k] = v
	}
	d["type"] = name
	s.Raw([]byte("event: " + name + "\ndata: " + jsonx.String(d) + "\n\n"))
}

// Data writes a "data:" line only (Chat Completions chunks).
func (s *Stream) Data(v any) { s.Raw([]byte("data: " + jsonx.String(v) + "\n\n")) }

// Ping writes a keepalive.
func (s *Stream) Ping() {
	if s.commentPings {
		s.Raw([]byte(": keepalive\n\n"))
	} else {
		s.Event("ping", nil)
	}
}

// Failed: a write failed (the client went away).
func (s *Stream) Failed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failed
}

// Close stops pings; later writes are dropped.
func (s *Stream) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.stop)
	}
}
