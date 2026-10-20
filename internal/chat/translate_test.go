package chat

import (
	"codelink/internal/media"
	"testing"
)

// The finish_reason table from ChatCompletionsTests.anthropicMessageBecomesAChatCompletion.
func TestFinishReason(t *testing.T) {
	for stop, want := range map[string]string{"max_tokens": "length", "end_turn": "stop", "stop_sequence": "stop", "refusal": "content_filter",
		"tool_use": "tool_calls", "model_context_window_exceeded": "length", "": "stop"} {
		if got := finishReason(stop); got != want {
			t.Errorf("%q: %q, want %q", stop, got, want)
		}
	}
}

func TestDataURL(t *testing.T) {
	cases := []struct {
		in, mediaType, data string
		ok                  bool
	}{
		{"data:image/PNG;base64,AAAA", "image/png", "AAAA", true},
		{"data:;base64,AAAA", "application/octet-stream", "AAAA", true},
		{"data:base64,AAAA", "application/octet-stream", "AAAA", true},
		{"data:image/png,AAAA", "", "", false},
		{"https://x/y.png", "", "", false},
	}
	for _, c := range cases {
		mt, d, ok := media.DataURL(c.in)
		if ok != c.ok || (ok && (mt != c.mediaType || d != c.data)) {
			t.Errorf("%s: %q %q %v", c.in, mt, d, ok)
		}
	}
}
