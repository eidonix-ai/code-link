package chat

import (
	"strings"
	"time"

	"codelink/internal/apierr"
	"codelink/internal/jsonx"
)

// anthropicStream translates an Anthropic Messages SSE stream into Chat
// Completions chunks (docs/DESIGN.md §5a), one upstream line at a time.
// Unknown event types are ignored, so additions to the Anthropic protocol
// do not break it. A stream that ends before message_stop is an error,
// never a quiet "stop". Port of AnthropicChatStream.swift.
type anthropicStream struct {
	raw          func([]byte)
	model        string
	includeUsage bool
	id           string
	created      int64

	roleSent     bool
	finishReason string
	inputUsage   map[string]any
	outputTokens int
	nextTool     int
	toolIndex    map[int]int
	sawStop      bool
	failed       bool
	finished     bool
}

func newAnthropicStream(raw func([]byte), model string, includeUsage bool) *anthropicStream {
	return &anthropicStream{raw: raw, model: model, includeUsage: includeUsage, id: newID("chatcmpl-"),
		created: time.Now().Unix(), finishReason: "stop", inputUsage: map[string]any{}, toolIndex: map[int]int{}}
}

func (s *anthropicStream) data(v any) { s.raw([]byte("data: " + jsonx.String(v) + "\n\n")) }

func (s *anthropicStream) chunk(delta map[string]any, finish string) {
	var fr any
	if finish != "" {
		fr = finish
	}
	s.data(map[string]any{"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": fr}}})
}

func (s *anthropicStream) usageChunk(u any) {
	s.data(map[string]any{"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": []any{}, "usage": u})
}

func (s *anthropicStream) role() {
	if !s.roleSent {
		s.roleSent = true
		s.chunk(map[string]any{"role": "assistant"}, "")
	}
}

func (s *anthropicStream) usage() map[string]any {
	u := make(map[string]any, len(s.inputUsage)+2)
	for k, v := range s.inputUsage {
		u[k] = v
	}
	u["completion_tokens"] = s.outputTokens
	u["total_tokens"] = intOf(u["prompt_tokens"]) + s.outputTokens
	return u
}

// line takes one upstream line; false when the stream is over (finished,
// failed or [DONE]) and no more lines should be read.
func (s *anthropicStream) line(line string) bool {
	if s.finished {
		return false
	}
	payload, ok := strings.CutPrefix(line, "data:")
	if !ok {
		return true
	}
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return true
	}
	if payload == "[DONE]" {
		return false
	}
	e := jsonx.ParseObject([]byte(payload))
	if e == nil {
		return true
	}
	switch jsonx.Str(e["type"]) {
	case "message_start":
		u := jsonx.Get(e, "message", "usage")
		s.inputUsage = usage(u)
		s.outputTokens = intOf(jsonx.Get(u, "output_tokens"))
		s.role()
	case "content_block_start":
		s.role()
		b := jsonx.Get(e, "content_block")
		if index, ok := jsonx.Int(e["index"]); ok && jsonx.Type(b) == "tool_use" {
			t := s.nextTool
			s.nextTool++
			s.toolIndex[index] = t
			s.chunk(map[string]any{"tool_calls": []any{map[string]any{"index": t, "id": orEmpty(jsonx.Get(b, "id")), "type": "function",
				"function": map[string]any{"name": orEmpty(jsonx.Get(b, "name")), "arguments": ""}}}}, "")
		}
	case "content_block_delta":
		s.role()
		d := jsonx.Get(e, "delta")
		switch jsonx.Type(d) {
		case "text_delta":
			if t := jsonx.Str(jsonx.Get(d, "text")); t != "" {
				s.chunk(map[string]any{"content": t}, "")
			}
		case "thinking_delta":
			if t := jsonx.Str(jsonx.Get(d, "thinking")); t != "" {
				s.chunk(map[string]any{"reasoning_content": t}, "")
			}
		case "input_json_delta":
			index, ok := jsonx.Int(e["index"])
			if t, known := s.toolIndex[index]; ok && known {
				s.chunk(map[string]any{"tool_calls": []any{map[string]any{"index": t,
					"function": map[string]any{"arguments": orEmpty(jsonx.Get(d, "partial_json"))}}}}, "")
			}
		}
	case "message_delta":
		if stop := jsonx.Str(jsonx.Get(e, "delta", "stop_reason")); stop != "" {
			s.finishReason = finishReason(stop)
		}
		if u, ok := e["usage"]; ok && u != nil {
			if out := intOf(jsonx.Get(u, "output_tokens")); out != 0 {
				s.outputTokens = out
			}
			if intOf(jsonx.Get(u, "input_tokens")) != 0 || intOf(jsonx.Get(u, "cache_read_input_tokens")) != 0 {
				s.inputUsage = usage(u)
			}
		}
	case "message_stop":
		s.sawStop = true
		s.role()
		s.chunk(map[string]any{}, s.finishReason)
		if s.includeUsage {
			s.usageChunk(s.usage())
		}
		s.finished = true
	case "error":
		message := jsonx.Str(jsonx.Get(e, "error", "message"))
		if message == "" {
			message = "upstream stream error"
		} else {
			message = apierr.Scrub(message)
		}
		s.fail(message)
		return false
	}
	return true
}

func (s *anthropicStream) fail(message string) {
	if s.finished {
		return
	}
	s.failed = true
	s.finished = true
	s.data(apierr.OAIServer(502, "upstream_error", message).Body())
}

// finish ends the stream: a stream cut short gets an error frame; every
// stream ends with [DONE].
func (s *anthropicStream) finish() {
	if !s.sawStop {
		s.fail("the backend's stream ended before it was complete")
	}
	s.raw([]byte("data: [DONE]\n\n"))
}

// replay sends a whole completion as the chunks a streaming request expects.
func replay(chat map[string]any, raw func([]byte), includeUsage bool) {
	s := newAnthropicStream(raw, jsonx.Str(chat["model"]), includeUsage)
	msg := jsonx.Obj(jsonx.Get(chat, "choices", 0, "message"))
	s.role()
	if c := jsonx.Str(msg["content"]); c != "" {
		s.chunk(map[string]any{"content": c}, "")
	}
	if r := jsonx.Str(msg["reasoning_content"]); r != "" {
		s.chunk(map[string]any{"reasoning_content": r}, "")
	}
	for i, call := range jsonx.Arr(msg["tool_calls"]) {
		c := map[string]any{}
		for k, v := range jsonx.Obj(call) {
			c[k] = v
		}
		c["index"] = i
		s.chunk(map[string]any{"tool_calls": []any{c}}, "")
	}
	finish := jsonx.Str(jsonx.Get(chat, "choices", 0, "finish_reason"))
	if finish == "" {
		finish = "stop"
	}
	s.chunk(map[string]any{}, finish)
	if u, ok := chat["usage"]; ok && includeUsage {
		s.usageChunk(u)
	}
	raw([]byte("data: [DONE]\n\n"))
}
