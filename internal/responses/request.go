package responses

import (
	"strings"

	"codelink/internal/config"
	"codelink/internal/jsonx"
	"codelink/internal/media"
)

// Responses API → Chat Completions (docs/DESIGN.md §5). The request's input
// has been through media.ProcessResponsesInput: documents are text, and
// images are left only for a model with vision.

// systemSeparator joins the parts of the merged leading system message.
const systemSeparator = "\n\n"

func textPart(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

// translateMessages gives the chat messages for a request's input and
// instructions, or nil when there is nothing to send.
//
// instructions and the LEADING run of system/developer messages become ONE
// system message (strict chat templates reject a system message anywhere
// but first, and one stable first message keeps prefix caches warm). Items
// that make no message (additional_tools, reasoning, …) do not end the run.
// A system/developer message after the conversation has started becomes
// user text <developer>…</developer>.
func translateMessages(input any, instructions string, reg *registry) []any {
	var messages []map[string]any
	var system []string
	if instructions != "" {
		system = append(system, instructions)
	}
	finish := func() []any {
		if len(messages) == 0 && len(system) == 0 {
			return nil
		}
		out := make([]any, 0, len(messages)+1)
		if len(system) > 0 {
			out = append(out, map[string]any{"role": "system", "content": strings.Join(system, systemSeparator)})
		}
		for _, m := range messages {
			out = append(out, m)
		}
		return out
	}

	if s, ok := input.(string); ok {
		messages = append(messages, map[string]any{"role": "user", "content": s})
		return finish()
	}
	items, ok := input.([]any)
	if !ok {
		return nil
	}

	// Reasoning waiting for the next assistant message; anything else in
	// between drops it.
	pendingReasoning := ""
	attachReasoning := func(m map[string]any) {
		if pendingReasoning == "" {
			return
		}
		r := pendingReasoning
		if prev := jsonx.Str(m["reasoning_content"]); prev != "" {
			r = prev + "\n\n" + pendingReasoning
		}
		m["reasoning_content"] = r
		m["reasoning"] = r
		pendingReasoning = ""
	}
	// Media from tool outputs: one user message after the tool messages.
	var toolMedia []any
	flushToolMedia := func() {
		if len(toolMedia) == 0 {
			return
		}
		messages = append(messages, map[string]any{"role": "user", "content": toolMedia})
		toolMedia = nil
	}

	for _, it := range items {
		item := jsonx.Obj(it)
		if item == nil {
			continue
		}
		typ := jsonx.Str(item["type"])
		role := jsonx.Str(item["role"])
		switch {
		case typ == "function_call" || typ == "local_shell_call" || typ == "custom_tool_call":
			var args string
			if typ == "custom_tool_call" {
				args = jsonx.String(map[string]any{"input": jsonx.Str(item["input"])})
			} else {
				args = jsonx.Str(item["arguments"])
				if args == "" {
					args = jsonx.Str(item["input"])
				}
				if a, ok := item["action"]; args == "" && ok && a != nil {
					args = jsonx.String(a)
				}
				if args == "" {
					args = "{}"
				}
			}
			name := jsonx.Str(item["name"])
			if name == "" && typ == "local_shell_call" {
				name = "shell"
			}
			call := map[string]any{"id": jsonx.Str(item["call_id"]), "type": "function",
				"function": map[string]any{"name": reg.flatName(jsonx.Str(item["namespace"]), name), "arguments": args}}
			flushToolMedia()
			if n := len(messages) - 1; n >= 0 && messages[n]["role"] == "assistant" {
				last := messages[n]
				last["tool_calls"] = append(jsonx.Arr(last["tool_calls"]), call)
				if c, ok := last["content"].(string); ok && c == "" {
					last["content"] = nil
				}
				attachReasoning(last)
			} else {
				m := map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{call}}
				messages = append(messages, m)
				attachReasoning(m)
			}

		case typ == "function_call_output" || typ == "local_shell_call_output" ||
			typ == "custom_tool_call_output" || typ == "mcp_tool_call_output":
			pendingReasoning = ""
			id := jsonx.Str(item["call_id"])
			text, parts := toolOutput(item["output"])
			if len(parts) > 0 {
				if strings.TrimSpace(text) == "" {
					text = "[the tool returned attachments; they follow in the next message]"
				}
				toolMedia = append(toolMedia, textPart("[attachments from tool result "+id+"]"))
				toolMedia = append(toolMedia, parts...)
			}
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": id, "content": text})

		case typ == "reasoning":
			if t := reasoningText(item); t != "" {
				if pendingReasoning == "" {
					pendingReasoning = t
				} else {
					pendingReasoning += "\n\n" + t
				}
			}

		case typ == "additional_tools" || typ == "compaction" || typ == "tool_search_call" || typ == "tool_search_output" ||
			typ == "web_search_call" || typ == "image_generation_call" || typ == "mcp_list_tools":
			// Tool definitions are collected separately; the rest has no
			// chat equivalent.

		case role == "system" || role == "developer":
			text := messageText(item["content"])
			if text == "" {
				continue
			}
			if len(messages) == 0 {
				system = append(system, text)
			} else {
				flushToolMedia()
				pendingReasoning = ""
				messages = append(messages, map[string]any{"role": "user", "content": "<developer>\n" + text + "\n</developer>"})
			}

		case role == "user" || role == "assistant":
			flushToolMedia()
			m := map[string]any{"role": role, "content": messageContent(item["content"], role)}
			messages = append(messages, m)
			if role == "assistant" {
				attachReasoning(m)
			} else {
				pendingReasoning = ""
			}
		}
	}
	flushToolMedia()
	return finish()
}

// messageText is message content as plain text: a string, or the text
// parts joined by a blank line. Other parts are ignored.
func messageText(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	var texts []string
	for _, p := range jsonx.Arr(content) {
		t := jsonx.Str(jsonx.Get(p, "text"))
		switch jsonx.Type(p) {
		case "input_text", "output_text", "text":
			if t != "" {
				texts = append(texts, t)
			}
		}
	}
	return strings.Join(texts, systemSeparator)
}

// reasoningText is the text of a replayed reasoning item: its
// reasoning_text parts, else its summary (this engine writes both with the
// same text).
func reasoningText(item map[string]any) string {
	collect := func(parts any) string {
		var out []string
		for _, p := range jsonx.Arr(parts) {
			if t := jsonx.Str(jsonx.Get(p, "text")); t != "" {
				out = append(out, t)
			}
		}
		return strings.Join(out, "\n\n")
	}
	if c := collect(item["content"]); c != "" {
		return c
	}
	return collect(item["summary"])
}

// messageContent is the chat content of a user or assistant message.
func messageContent(content any, role string) any {
	if content == nil {
		return ""
	}
	if s, ok := content.(string); ok {
		return s
	}
	parts, ok := content.([]any)
	if !ok {
		return jsonx.String(content)
	}
	if role == "assistant" {
		var b strings.Builder
		for _, p := range parts {
			if t := jsonx.Type(p); t == "output_text" || t == "text" {
				b.WriteString(jsonx.Str(jsonx.Get(p, "text")))
			}
		}
		return b.String()
	}
	out := []any{}
	for _, p := range parts {
		switch jsonx.Type(p) {
		case "input_text", "text":
			out = append(out, textPart(jsonx.Str(jsonx.Get(p, "text"))))
		case "input_image":
			out = append(out, imagePart(p))
		case "input_file":
			out = append(out, textPart(unreadableFile(p)))
		default:
			out = append(out, p)
		}
	}
	if len(out) == 1 && jsonx.Type(out[0]) == "text" {
		return jsonx.Get(out[0], "text")
	}
	if len(out) == 0 {
		return ""
	}
	return out
}

func imagePart(p any) map[string]any {
	url := jsonx.Str(jsonx.Get(p, "image_url"))
	if url == "" {
		url = jsonx.Str(jsonx.Get(p, "image_url", "url"))
	}
	image := map[string]any{"url": url}
	if d := jsonx.Str(jsonx.Get(p, "detail")); d != "" {
		image["detail"] = d
	}
	return map[string]any{"type": "image_url", "image_url": image}
}

func unreadableFile(p any) string {
	name := jsonx.Str(jsonx.Get(p, "filename"))
	if _, ok := jsonx.Get(p, "filename").(string); !ok {
		name = "unnamed file"
	}
	return "[attached file \"" + name + "\" could not be read: only inline file_data is supported]"
}

// toolOutput is a tool output as the tool message text and the parts a
// tool message cannot carry (images, attachment text). The output is a
// string, parts, or an object {content, success}.
func toolOutput(output any) (string, []any) {
	switch v := output.(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	case []any:
		var texts []string
		var parts []any
		for _, p := range v {
			t := jsonx.Str(jsonx.Get(p, "text"))
			switch jsonx.Type(p) {
			case "input_text", "text", "output_text":
				if t != "" {
					texts = append(texts, t)
				}
			case media.AttachmentTextType:
				parts = append(parts, textPart(t))
			case "input_image":
				parts = append(parts, imagePart(p))
			case "input_file":
				parts = append(parts, textPart(unreadableFile(p)))
			default:
				if t != "" {
					texts = append(texts, t)
				}
			}
		}
		return strings.Join(texts, "\n"), parts
	case map[string]any:
		if inner, ok := v["content"]; ok && inner != nil {
			return toolOutput(inner)
		}
	}
	return jsonx.String(output), nil
}

// additionalTools are the tool definitions additional_tools input items
// carry (Codex's auto-review sends its tools this way).
func additionalTools(input any) []any {
	var out []any
	for _, it := range jsonx.Arr(input) {
		if jsonx.Type(it) == "additional_tools" {
			out = append(out, jsonx.Arr(jsonx.Get(it, "tools"))...)
		}
	}
	return out
}

// responseFormat is the text.format of a request as a chat response_format.
func responseFormat(text any) map[string]any {
	f := jsonx.Get(text, "format")
	switch jsonx.Type(f) {
	case "json_schema":
		name := jsonx.Str(jsonx.Get(f, "name"))
		if name == "" {
			name = "response"
		}
		schema := map[string]any{"name": name}
		if s := jsonx.Get(f, "schema"); s != nil {
			schema["schema"] = s
		}
		if strict, ok := jsonx.BoolOK(jsonx.Get(f, "strict")); ok {
			schema["strict"] = strict
		}
		return map[string]any{"type": "json_schema", "json_schema": schema}
	case "json_object":
		return map[string]any{"type": "json_object"}
	}
	return nil
}

// chatRequest is the Chat Completions body (without model) for a
// Responses request.
func chatRequest(req map[string]any, messages []any, tools []any, reg *registry, model config.Model, contextWindow int) map[string]any {
	out := map[string]any{"messages": messages}
	stream := jsonx.Bool(req["stream"])
	out["stream"] = stream
	if stream {
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	if len(tools) > 0 {
		out["tools"] = tools
		if tc, ok := req["tool_choice"]; ok && tc != nil {
			out["tool_choice"] = reg.toolChoice(tc)
		}
		if p, ok := jsonx.BoolOK(req["parallel_tool_calls"]); ok {
			out["parallel_tool_calls"] = p
		}
	}
	if n, ok := jsonx.Int(req["max_output_tokens"]); ok && n > 0 {
		out["max_tokens"] = n
	}
	for _, k := range []string{"temperature", "top_p"} {
		if v, ok := req[k]; ok && v != nil {
			out[k] = v
		}
	}
	thinkingOff := false
	if effort := strings.TrimSpace(jsonx.Str(jsonx.Get(req, "reasoning", "effort"))); effort != "" {
		// "none" turns thinking off where the model says how.
		if effort == "none" && model.ThinkingOff != nil {
			thinkingOff = true
		} else {
			out["reasoning_effort"] = effort
		}
	}
	if rf := responseFormat(req["text"]); rf != nil {
		out["response_format"] = rf
	}
	if user := jsonx.Str(req["user"]); user != "" {
		out["user"] = user
	}
	if thinkingOff {
		out = deepMerge(out, jsonx.Clone(model.ThinkingOff).(map[string]any))
	}
	if contextWindow > 0 {
		clampMaxTokens(out, contextWindow)
	}
	return out
}

func deepMerge(dst, src map[string]any) map[string]any {
	for k, v := range src {
		if d, ok := dst[k].(map[string]any); ok {
			if s, ok := v.(map[string]any); ok {
				dst[k] = deepMerge(d, s)
				continue
			}
		}
		dst[k] = v
	}
	return dst
}

// clampMaxTokens lowers max_tokens so prompt + completion fit the context
// window. The prompt size is an estimate (with 10% + 128 tokens of
// headroom); when it leaves no sensible room the request goes as is and the
// backend's context error takes over.
func clampMaxTokens(req map[string]any, contextWindow int) {
	maxTok, ok := jsonx.Int(req["max_tokens"])
	if contextWindow <= 0 || !ok || maxTok <= 0 {
		return
	}
	prompt := estimateTokens(jsonx.Marshal(req))
	room := contextWindow - prompt - prompt/10 - 128
	if room < 256 || maxTok <= room {
		return
	}
	req["max_tokens"] = room
}

// estimateTokens is the prompt size of a JSON request body: ~4 characters
// per token for text, with embedded base64 media costed separately (a flat
// figure per image, bytes-based for PDFs) instead of by its base64 length.
func estimateTokens(b []byte) int {
	const imageTokens, maxPDFTokens = 1600, 60_000
	tokens, textBytes, start := 0, len(b), -1
	for i := 0; i <= len(b); i++ {
		inRun := false
		if i < len(b) {
			c := b[i]
			inRun = c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=' || c == '_' || c == '-'
		}
		if inRun && start < 0 {
			start = i
		} else if !inRun && start >= 0 {
			if n := i - start; n >= 2048 {
				textBytes -= n
				// "JVBERi" is "%PDF-" in base64.
				if string(b[start:start+6]) == "JVBERi" {
					tokens += min(n*3/4/150, maxPDFTokens)
				} else {
					tokens += imageTokens
				}
			}
			start = -1
		}
	}
	return tokens + textBytes/4
}
