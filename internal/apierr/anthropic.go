package apierr

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	"codelink/internal/upstream"
)

// Anthropic is an error in the Anthropic shape. Claude Code acts on the
// status, error.type and a few message formats:
//   - 401/403 from a backend are code-link's own credentials failing, so
//     they become 502 and never start a login flow;
//   - context overflows become "prompt is too long: N tokens > M maximum";
//   - messages are scrubbed.
type Anthropic struct {
	Status  int
	Type    string
	Message string
	// Header carries retry pacing headers to pass on.
	Header http.Header
}

func (e Anthropic) Error() string { return e.Message }

// Body is {"type":"error","error":{"type","message"}}.
func (e Anthropic) Body() map[string]any {
	return map[string]any{"type": "error", "error": map[string]any{"type": e.Type, "message": e.Message}}
}

func Invalid(m string) Anthropic  { return Anthropic{Status: 400, Type: "invalid_request_error", Message: m} }
func NotFound(m string) Anthropic { return Anthropic{Status: 404, Type: "not_found_error", Message: m} }
func API(status int, m string) Anthropic {
	return Anthropic{Status: status, Type: "api_error", Message: m}
}

var (
	Unauthorized = Anthropic{Status: 401, Type: "authentication_error", Message: "invalid or missing access token"}
	Truncated    = Anthropic{Status: 529, Type: "overloaded_error", Message: "the backend's stream ended before it was complete"}
	TooLarge     = Anthropic{Status: 413, Type: "request_too_large", Message: "request body too large"}
)

// Transport is the error for failing to reach the backend at all.
func Transport(err error) Anthropic {
	switch {
	case errors.Is(err, upstream.ErrHeaderTimeout):
		return API(504, "the backend did not answer in time")
	case errors.Is(err, context.Canceled):
		return API(499, "cancelled")
	default:
		return API(502, "the backend could not be reached: "+Scrub(upstream.Describe(err)))
	}
}

// statusFor maps an upstream HTTP status to the client status and type.
func statusFor(upstream int) (int, string) {
	switch {
	case upstream == 400, upstream == 422:
		return 400, "invalid_request_error"
	case upstream == 401, upstream == 403:
		return 502, "api_error"
	case upstream == 404:
		return 404, "not_found_error"
	case upstream == 413:
		return 413, "request_too_large"
	case upstream == 429:
		return 429, "rate_limit_error"
	case upstream == 503, upstream == 529:
		return upstream, "overloaded_error"
	case upstream >= 500:
		return upstream, "api_error"
	case upstream >= 400:
		return 400, "invalid_request_error"
	}
	return 502, "api_error"
}

// RetryHeaders are the retry pacing and rate-limit headers of a response.
func RetryHeaders(h http.Header) http.Header {
	out := http.Header{}
	for _, k := range []string{"Retry-After", "Retry-After-Ms", "X-Should-Retry"} {
		if v := h.Get(k); v != "" {
			out.Set(k, v)
		}
	}
	var keys []string
	for k := range h {
		if strings.HasPrefix(strings.ToLower(k), "anthropic-ratelimit-") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		out[k] = h[k]
	}
	return out
}

var nativeTypes = []string{"invalid_request_error", "rate_limit_error", "overloaded_error", "api_error", "not_found_error", "request_too_large"}

// FromUpstream is the client error for a backend error response. native:
// an Anthropic backend, whose own meaningful error.type is kept.
func FromUpstream(status int, body []byte, header http.Header, native bool) Anthropic {
	info := ParseUpstream(body)
	outStatus, outType := statusFor(status)
	if native && info.Type != "" && status != 401 && status != 403 && slices.Contains(nativeTypes, info.Type) {
		outType = info.Type
	}
	retry := RetryHeaders(header)
	if status == 400 || status == 413 || status == 422 {
		if r := RewriteContextLength(info); r != "" {
			return Anthropic{Status: 400, Type: "invalid_request_error", Message: r, Header: retry}
		}
	}
	var msg string
	if status == 401 || status == 403 {
		msg = "the model backend rejected code-link's credentials"
		if info.Message != "" {
			msg += ": " + Scrub(info.Message)
		}
	} else {
		msg = Scrub(info.Message)
	}
	if msg == "" {
		msg = "backend returned HTTP " + strconv.Itoa(status)
	}
	return Anthropic{Status: outStatus, Type: outType, Message: msg, Header: retry}
}
