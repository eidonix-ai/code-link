// Package upstream is HTTP to the backends: no overall timeout (a live
// stream may run as long as it needs), a bounded wait for response headers,
// and cancellation that closes the connection so the backend stops
// generating.
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Errors before a response arrives.
var (
	ErrHeaderTimeout = errors.New("the backend did not answer in time")
	ErrLineTooLong   = errors.New("the backend sent a stream line over 16 MiB")
	ErrBodyTooLarge  = errors.New("the backend's response is too large")
)

// TransportError is a failure to connect or send.
type TransportError struct{ Err error }

func (e *TransportError) Error() string { return e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

// Request is one upstream exchange.
type Request struct {
	URL    string
	Method string // "" = POST
	Header http.Header
	Body   []byte
	// RefusePrivateRedirects refuses redirects to loopback, private or
	// link-local addresses (fetching documents by URL).
	RefusePrivateRedirects bool
}

// Response is a response whose headers are in. Close it when done; that
// cancels the exchange if the body wasn't read to the end.
type Response struct {
	Status int
	Header http.Header
	Body   io.ReadCloser
}

func (r *Response) Close() { r.Body.Close() }

// IsEventStream: the body is SSE.
func (r *Response) IsEventStream() bool {
	return strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "text/event-stream")
}

// Collect reads the whole body, at most limit bytes (0 = 64 MiB).
func (r *Response) Collect(limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = 64 << 20
	}
	defer r.Close()
	b, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return b, err
	}
	if int64(len(b)) > limit {
		return nil, ErrBodyTooLarge
	}
	return b, nil
}

// Lines splits the body into lines (\n, a trailing \r removed), each at
// most 16 MiB.
func (r *Response) Lines() *LineReader { return NewLineReader(r.Body) }

// LineReader reads lines from a stream.
type LineReader struct{ s *bufio.Scanner }

func NewLineReader(r io.Reader) *LineReader {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64<<10), 16<<20)
	s.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			return i + 1, bytes.TrimSuffix(data[:i], []byte("\r")), nil
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	return &LineReader{s: s}
}

// Next is the next line; ok false at the end, with err set on a read error.
func (l *LineReader) Next() (line string, ok bool, err error) {
	if l.s.Scan() {
		return l.s.Text(), true, nil
	}
	err = l.s.Err()
	if errors.Is(err, bufio.ErrTooLong) {
		err = ErrLineTooLong
	}
	return "", false, err
}

// Client sends upstream requests.
type Client struct {
	plain   *http.Client
	guarded *http.Client
}

// NewClient is a client with connection reuse for many parallel streams.
func NewClient() *Client {
	t := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        512,
		MaxIdleConnsPerHost: 512,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	guarded := &http.Client{Transport: t, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if s := req.URL.Scheme; s != "http" && s != "https" {
			return errors.New("redirect to a non-http address refused")
		}
		if IsPrivateHost(req.URL.Hostname()) {
			return errors.New("redirect to a private address refused")
		}
		return nil
	}}
	return &Client{plain: &http.Client{Transport: t}, guarded: guarded}
}

// Send starts the exchange and waits at most headerTimeout for the
// response headers. Cancelling ctx (the client went away) closes the
// connection.
func (c *Client) Send(ctx context.Context, r Request, headerTimeout time.Duration) (*Response, error) {
	method := r.Method
	if method == "" {
		method = http.MethodPost
	}
	ctx, cancel := context.WithCancel(ctx)
	var body io.Reader
	if r.Body != nil {
		body = bytes.NewReader(r.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.URL, body)
	if err != nil {
		cancel()
		return nil, &TransportError{err}
	}
	for k, vs := range r.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	timedOut := make(chan struct{})
	timer := time.AfterFunc(headerTimeout, func() { close(timedOut); cancel() })
	hc := c.plain
	if r.RefusePrivateRedirects {
		hc = c.guarded
	}
	resp, err := hc.Do(req)
	if !timer.Stop() {
		<-timedOut
		if resp != nil {
			resp.Body.Close()
		}
		cancel()
		return nil, ErrHeaderTimeout
	}
	if err != nil {
		cancel()
		if ctx.Err() != nil && errors.Is(err, context.Canceled) {
			return nil, context.Canceled
		}
		return nil, &TransportError{err}
	}
	return &Response{Status: resp.StatusCode, Header: resp.Header, Body: &cancelBody{resp.Body, cancel}}, nil
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// Describe is an upstream error for a client message.
func Describe(err error) string {
	var te *TransportError
	switch {
	case errors.Is(err, ErrHeaderTimeout):
		return ErrHeaderTimeout.Error()
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.As(err, &te):
		return te.Err.Error()
	default:
		return fmt.Sprint(err)
	}
}
