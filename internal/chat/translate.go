package chat

import (
	"crypto/rand"
	"strings"
	"time"

	"codelink/internal/jsonx"
)

// Chat Completions ↔ Anthropic Messages, for a Chat client on an Anthropic
// backend (docs/DESIGN.md §5a). Port of ChatToAnthropic.swift.

const defaultMaxTokens = 4096

// toAnthropic is the Messages request for a Chat Completions request, or
// nil when it has no user or assistant message to send.
func toAnthropic(req map[string]any, upstreamModel string) map[string]any {
	var system []string
	var messages, toolResults []any
	flush := func() {
		if len(toolResults) > 0 {
			messages = append(messages, map[string]any{"role": "user", "content": toolResults})
			toolResults = nil
		}
	}
	for _, m := range jsonx.Arr(req["messages"]) {
		mo := jsonx.Obj(m)
		switch jsonx.Str(mo["role"]) {
		case "system", "developer":
			flush()
			if t := text(mo["content"]); t != "" {
				system = append(system, t)
			}
		case "user":
			flush()
			if b := userBlocks(mo["content"]); len(b) > 0 {
				messages = append(messages, map[string]any{"role": "user", "content": b})
			}
		case "assistant":
			flush()
			if b := assistantBlocks(mo); len(b) > 0 {
				messages = append(messages, map[string]any{"role": "assistant", "content": b})
			}
		case "tool":
			id := jsonx.Str(mo["tool_call_id"])
			if id == "" {
				continue
			}
			toolResults = append(toolResults, map[string]any{"type": "tool_result", "tool_use_id": id, "content": toolText(mo["content"])})
		}
	}
	flush()
	if len(messages) == 0 {
		return nil
	}

	out := map[string]any{"model": upstreamModel, "messages": messages}
	maxTokens := defaultMaxTokens
	for _, k := range []string{"max_completion_tokens", "max_tokens"} {
		if n, ok := jsonx.Int(req[k]); ok && n > 0 {
			maxTokens = n
			break
		}
	}
	out["max_tokens"] = maxTokens
	out["stream"] = jsonx.Bool(req["stream"])
	if len(system) > 0 {
		out["system"] = strings.Join(system, "\n")
	}
	for _, k := range []string{"temperature", "top_p", "top_k"} {
		if v, ok := req[k]; ok && v != nil {
			out[k] = v
		}
	}
	if stop, ok := req["stop"]; ok {
		var stops []any
		if s, ok := stop.(string); ok {
			stops = []any{s}
		} else {
			for _, x := range jsonx.Arr(stop) {
				if s, ok := x.(string); ok {
					stops = append(stops, s)
				}
			}
		}
		if len(stops) > 0 {
			out["stop_sequences"] = stops
		}
	}
	if u := jsonx.Str(req["user"]); u != "" {
		out["metadata"] = map[string]any{"user_id": u}
	}
	if t := tools(jsonx.Arr(req["tools"])); len(t) > 0 {
		out["tools"] = t
		var parallel *bool
		if b, ok := jsonx.BoolOK(req["parallel_tool_calls"]); ok {
			parallel = &b
		}
		if c := toolChoice(req["tool_choice"], parallel); c != nil {
			out["tool_choice"] = c
		}
	}
	return out
}

// text is message content as plain text: a string, or its text parts joined.
func text(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	var b strings.Builder
	for _, p := range jsonx.Arr(content) {
		if t := jsonx.Type(p); t == "text" || t == "input_text" {
			b.WriteString(jsonx.Str(jsonx.Get(p, "text")))
		}
	}
	return b.String()
}

func toolText(content any) string {
	if content == nil {
		return ""
	}
	if s, ok := content.(string); ok {
		return s
	}
	if t := text(content); t != "" {
		return t
	}
	return jsonx.String(content)
}

func userBlocks(content any) []any {
	if content == nil {
		return nil
	}
	if s, ok := content.(string); ok {
		if s == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": s}}
	}
	var out []any
	for _, p := range jsonx.Arr(content) {
		var block map[string]any
		switch jsonx.Type(p) {
		case "text", "input_text":
			if t := jsonx.Str(jsonx.Get(p, "text")); t != "" {
				block = map[string]any{"type": "text", "text": t}
			}
		case "image_url", "input_image":
			block = image(jsonx.Obj(p))
		}
		if block == nil {
			continue
		}
		if cc := jsonx.Get(p, "cache_control"); cc != nil {
			block["cache_control"] = cc
		}
		out = append(out, block)
	}
	return out
}

func image(p map[string]any) map[string]any {
	raw, ok := p["image_url"]
	if !ok {
		raw = p["url"]
	}
	url, isString := raw.(string)
	if !isString {
		url = jsonx.Str(jsonx.Get(raw, "url"))
	}
	if url == "" {
		return nil
	}
	if strings.HasPrefix(url, "data:") {
		mediaType, data, ok := dataURL(url)
		if !ok {
			return nil
		}
		return map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mediaType, "data": data}}
	}
	return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": url}}
}

// dataURL splits a base64 data: URL into its media type and data.
func dataURL(s string) (mediaType, data string, ok bool) {
	rest, found := strings.CutPrefix(s, "data:")
	if !found {
		return "", "", false
	}
	header, data, found := strings.Cut(rest, ",")
	if !found {
		return "", "", false
	}
	parts := strings.Split(header, ";")
	base64 := false
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
		if strings.EqualFold(parts[i], "base64") {
			base64 = true
		}
	}
	if !base64 {
		return "", "", false
	}
	mediaType = strings.ToLower(parts[0])
	if mediaType == "" || mediaType == "base64" {
		mediaType = "application/octet-stream"
	}
	return mediaType, data, true
}

func assistantBlocks(m map[string]any) []any {
	var out []any
	if t := text(m["content"]); t != "" {
		out = append(out, map[string]any{"type": "text", "text": t})
	}
	for _, call := range jsonx.Arr(m["tool_calls"]) {
		name := jsonx.Str(jsonx.Get(call, "function", "name"))
		if name == "" {
			continue
		}
		var input any = map[string]any{}
		if args := jsonx.Str(jsonx.Get(call, "function", "arguments")); args != "" {
			if v, err := jsonx.Parse([]byte(args)); err == nil {
				input = v
			}
		}
		id := jsonx.Get(call, "id")
		if id == nil {
			id = ""
		}
		out = append(out, map[string]any{"type": "tool_use", "id": id, "name": name, "input": input})
	}
	return out
}

func tools(list []any) []any {
	var out []any
	for _, t := range list {
		if typ := jsonx.Type(t); typ != "" && typ != "function" {
			continue
		}
		fn := jsonx.Obj(jsonx.Get(t, "function"))
		name := jsonx.Str(fn["name"])
		if name == "" {
			continue
		}
		tool := map[string]any{"name": name, "input_schema": map[string]any{"type": "object", "properties": map[string]any{}}}
		if d := jsonx.Str(fn["description"]); d != "" {
			tool["description"] = d
		}
		if p := fn["parameters"]; p != nil {
			tool["input_schema"] = p
		}
		out = append(out, tool)
	}
	return out
}

func toolChoice(tc any, parallel *bool) map[string]any {
	choice := map[string]any{}
	if s, ok := tc.(string); ok {
		switch s {
		case "auto", "none":
			choice["type"] = s
		case "required":
			choice["type"] = "any"
		}
	} else if jsonx.Type(tc) == "function" {
		if name := jsonx.Str(jsonx.Get(tc, "function", "name")); name != "" {
			choice["type"] = "tool"
			choice["name"] = name
		}
	}
	if parallel != nil && !*parallel {
		if len(choice) == 0 {
			choice["type"] = "auto"
		}
		choice["disable_parallel_tool_use"] = true
	}
	if len(choice) == 0 {
		return nil
	}
	return choice
}

// finishReason is an Anthropic stop_reason as a Chat finish_reason.
func finishReason(stop string) string {
	switch stop {
	case "tool_use":
		return "tool_calls"
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "refusal":
		return "content_filter"
	}
	return "stop"
}

func intOf(v any) int { n, _ := jsonx.Int(v); return n }

// usage is Chat usage for Anthropic usage: the prompt is the input plus
// every cached token, and the cache reads are the cached ones.
func usage(u any) map[string]any {
	input := intOf(jsonx.Get(u, "input_tokens")) + intOf(jsonx.Get(u, "cache_creation_input_tokens")) + intOf(jsonx.Get(u, "cache_read_input_tokens"))
	output := intOf(jsonx.Get(u, "output_tokens"))
	out := map[string]any{"prompt_tokens": input, "completion_tokens": output, "total_tokens": input + output}
	if cached := intOf(jsonx.Get(u, "cache_read_input_tokens")); cached > 0 {
		out["prompt_tokens_details"] = map[string]any{"cached_tokens": cached}
	}
	return out
}

// fromAnthropic is a non-streaming Anthropic message as a Chat Completions
// response, or nil when it isn't a message.
func fromAnthropic(message map[string]any, model string) map[string]any {
	content, isArray := message["content"].([]any)
	if !isArray && jsonx.Str(message["type"]) != "message" {
		return nil
	}
	var texts, thinking []string
	var calls []any
	for _, b := range content {
		switch jsonx.Type(b) {
		case "text":
			if t, ok := jsonx.StrOK(jsonx.Get(b, "text")); ok {
				texts = append(texts, t)
			}
		case "thinking":
			if t := jsonx.Str(jsonx.Get(b, "thinking")); t != "" {
				thinking = append(thinking, t)
			}
		case "tool_use":
			input := jsonx.Get(b, "input")
			if input == nil {
				input = map[string]any{}
			}
			calls = append(calls, map[string]any{"id": orEmpty(jsonx.Get(b, "id")), "type": "function",
				"function": map[string]any{"name": orEmpty(jsonx.Get(b, "name")), "arguments": jsonx.String(input)}})
		}
	}
	msg := map[string]any{"role": "assistant", "content": nil}
	if len(texts) > 0 {
		msg["content"] = strings.Join(texts, "")
	}
	if len(thinking) > 0 {
		msg["reasoning_content"] = strings.Join(thinking, "")
	}
	if len(calls) > 0 {
		msg["tool_calls"] = calls
	}
	id := jsonx.Str(message["id"])
	if !strings.HasPrefix(id, "chatcmpl-") {
		id = newID("chatcmpl-")
	}
	return map[string]any{
		"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finishReason(jsonx.Str(message["stop_reason"]))}},
		"usage":   usage(message["usage"]),
	}
}

func orEmpty(v any) any {
	if v == nil {
		return ""
	}
	return v
}

const idAlphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// newID is prefix + 24 random alphanumerics.
func newID(prefix string) string {
	b := make([]byte, 24)
	rand.Read(b)
	for i := range b {
		b[i] = idAlphabet[int(b[i])%len(idAlphabet)]
	}
	return prefix + string(b)
}
