package apierr

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"codelink/internal/jsonx"
	"codelink/internal/upstream"
)

// OpenAI is an error in the OpenAI shape, for /v1/responses and
// /v1/chat/completions. Codex acts on Code: context_length_exceeded makes it
// compact, rate_limit_exceeded and server_is_overloaded are retried with
// backoff, invalid_prompt is fatal.
type OpenAI struct {
	Status     int
	Type       string
	Code       string // "" = null
	Message    string
	RetryAfter string
}

func (e OpenAI) Error() string { return e.Message }

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Object is the error object (also response.error of a failed response).
func (e OpenAI) Object() map[string]any {
	return map[string]any{"code": nullable(e.Code), "type": e.Type, "message": e.Message}
}

// Body is {"error":{"message","type","param","code"}}.
func (e OpenAI) Body() map[string]any {
	return map[string]any{"error": map[string]any{"message": e.Message, "type": e.Type, "param": nil, "code": nullable(e.Code)}}
}

// Write sends the error as the response.
func (e OpenAI) Write(w http.ResponseWriter) {
	if e.RetryAfter != "" {
		w.Header().Set("Retry-After", e.RetryAfter)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	w.Write(jsonx.Marshal(e.Body()))
}

func OAIInvalid(message, code string) OpenAI {
	return OpenAI{Status: 400, Type: "invalid_request_error", Code: code, Message: message}
}
func OAINotFound(message string) OpenAI {
	return OpenAI{Status: 404, Type: "invalid_request_error", Code: "model_not_found", Message: message}
}
func OAIServer(status int, code, message string) OpenAI {
	return OpenAI{Status: status, Type: "server_error", Code: code, Message: message}
}

var (
	// OAITruncated is sent in response.failed when the stream ends early.
	OAITruncated   = OAIServer(502, "stream_truncated", "upstream stream ended before the response was complete")
	OAIInterrupted = OAIServer(502, "stream_interrupted", "upstream stream was interrupted before it finished")
)

// OAITransport is a failure to reach the backend at all.
func OAITransport(err error) OpenAI {
	switch {
	case errors.Is(err, upstream.ErrHeaderTimeout):
		return OAIServer(504, "upstream_timeout", "upstream request timed out")
	case errors.Is(err, context.Canceled):
		return OAIServer(499, "cancelled", "cancelled")
	}
	return OAIServer(502, "upstream_unreachable", "upstream request failed")
}

var contextLengthMarkers = []string{
	"context_length_exceeded", "maximum context length", "context length", "context window", "max_model_len",
	"maximum model length", "too many tokens", "prompt is too long", "input is too long", "input token count",
	"reduce the length", "exceeds the maximum number of tokens", "exceeds the model",
}

func hasContextMarker(s string) bool {
	s = strings.ToLower(s)
	for _, m := range contextLengthMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// OAIFromUpstream is what a client sees for a backend HTTP error. Raw
// bodies are never forwarded: a cleaned message for 400s and overflows.
func OAIFromUpstream(status int, body []byte, header http.Header) OpenAI {
	info := ParseUpstream(body)
	message := Scrub(info.Message)
	e := OAIServer(status, "upstream_error", "backend returned HTTP "+strconv.Itoa(status))
	contextLength := hasContextMarker(string(body)) || RewriteContextLength(info) != ""
	switch {
	case status >= 400 && status < 500 && contextLength && status != 401 && status != 403 && status != 429:
		e.Status, e.Type, e.Code = 400, "invalid_request_error", "context_length_exceeded"
		if message != "" {
			e.Message = message
		}
	case status == 400 || status == 422:
		e.Status, e.Type, e.Code = 400, "invalid_request_error", "invalid_prompt"
		if message != "" {
			e.Message = message
		}
	case status == 401 || status == 403:
		e.Status, e.Code, e.Message = 502, "upstream_auth_error", "backend rejected the proxy's credentials"
	case status == 429:
		e.Type, e.Code = "rate_limit_error", "rate_limit_exceeded"
		e.Message = "backend is rate limiting requests; retry shortly"
		e.RetryAfter = header.Get("Retry-After")
	case status == 503:
		e.Code = "server_is_overloaded"
		e.Message = "backend is overloaded or unavailable; retry shortly"
		e.RetryAfter = header.Get("Retry-After")
	case status >= 400 && status < 500:
		e.Type = "invalid_request_error"
	}
	return e
}

// OAIFromAnthropic is an Anthropic backend's failure as an OpenAI error.
func OAIFromAnthropic(a Anthropic) OpenAI {
	t, code := a.Type, ""
	switch a.Type {
	case "invalid_request_error":
	case "rate_limit_error":
		code = "rate_limit_exceeded"
	case "not_found_error":
		t, code = "invalid_request_error", "model_not_found"
	case "request_too_large":
		t, code = "invalid_request_error", "request_too_large"
	case "overloaded_error":
		t, code = "server_error", "server_is_overloaded"
	default:
		t = "server_error"
	}
	var retry string
	if a.Header != nil {
		retry = a.Header.Get("Retry-After")
	}
	return OpenAI{Status: a.Status, Type: t, Code: code, Message: a.Message, RetryAfter: retry}
}

// OAIInBand is an in-band {"error": …} frame of a stream.
func OAIInBand(err any) OpenAI {
	message, ok := err.(string)
	if !ok {
		message = jsonx.Str(jsonx.Get(err, "message"))
	}
	if message == "" {
		message = "the backend reported an error"
	}
	info := UpstreamInfo{Message: message, Type: jsonx.Str(jsonx.Get(err, "type"))}
	text := Scrub(message)
	if RewriteContextLength(info) != "" || hasContextMarker(message) {
		return OpenAI{Status: 400, Type: "invalid_request_error", Code: "context_length_exceeded", Message: text}
	}
	return OAIServer(502, "upstream_error", text)
}
