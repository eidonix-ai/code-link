package messages

import (
	"strings"

	"codelink/internal/config"
	"codelink/internal/jsonx"
	"codelink/internal/media"
)

type obj = map[string]any

// webSearchTool is the web_search_* server tool of a request (docs/DESIGN.md §4.5).
type webSearchTool struct {
	maxUses        int
	allowedDomains []string
	blockedDomains []string
}

const (
	webSearchFunction      = "web_search"
	webSearchDefaultMaxUse = 5
)

func stringList(v any) []string {
	a, ok := v.([]any)
	if !ok {
		return nil
	}
	out := []string{}
	for _, x := range a {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// findWebSearch is the request's web search server tool, or nil.
func findWebSearch(tools []any) *webSearchTool {
	for _, t := range tools {
		if !strings.HasPrefix(jsonx.Type(t), "web_search_") {
			continue
		}
		n, _ := jsonx.Int(jsonx.Get(t, "max_uses"))
		if n <= 0 {
			n = webSearchDefaultMaxUse
		}
		return &webSearchTool{maxUses: n, allowedDomains: stringList(jsonx.Get(t, "allowed_domains")),
			blockedDomains: stringList(jsonx.Get(t, "blocked_domains"))}
	}
	return nil
}

// function is the web_search function the model is offered.
func (webSearchTool) function() obj {
	return obj{"type": "function", "function": obj{
		"name":        webSearchFunction,
		"description": "Search the web for current information. Returns the top results with title, URL and a snippet. Use it for anything that may have changed or that you do not know; you can search again with a better query.",
		"parameters": obj{"type": "object",
			"properties": obj{"query": obj{"type": "string", "description": "The search query"}},
			"required":   []any{"query"}},
	}}
}

// chatRequest is the Chat Completions body (without "model") for an
// Anthropic Messages request (docs/DESIGN.md §4.1). The request has been
// through media processing: documents are text, and images are left only
// for a model with vision. contextWindow 0 = unknown.
func chatRequest(req obj, model config.Model, contextWindow int, ws *webSearchTool) obj {
	out := obj{}
	var msgs []any
	if sys := systemText(req["system"]); sys != "" {
		msgs = append(msgs, obj{"role": "system", "content": sys})
	}
	reqMsgs := jsonx.Arr(req["messages"])
	msgs = append(msgs, translateMessages(reqMsgs)...)
	if msgs == nil {
		msgs = []any{}
	}
	out["messages"] = msgs
	stream := jsonx.Bool(req["stream"])
	out["stream"] = stream
	if stream {
		out["stream_options"] = obj{"include_usage": true}
	}

	tools := translateTools(jsonx.Arr(req["tools"]), referencedTools(reqMsgs))
	if ws != nil {
		tools = append(tools, ws.function())
	}
	if len(tools) > 0 {
		out["tools"] = tools
		if tc := toolChoice(req["tool_choice"]); tc != nil {
			out["tool_choice"] = tc
		}
		if jsonx.Bool(jsonx.Get(req, "tool_choice", "disable_parallel_tool_use")) {
			out["parallel_tool_calls"] = false
		}
	}
	if n, ok := jsonx.Int(req["max_tokens"]); ok && n > 0 {
		out["max_tokens"] = n
	}
	if stops := jsonx.Arr(req["stop_sequences"]); len(stops) > 0 {
		out["stop"] = stops
	}
	for _, k := range []string{"temperature", "top_p", "top_k"} {
		if v, ok := req[k]; ok && v != nil {
			out[k] = v
		}
	}

	thinkingOff := jsonx.Str(jsonx.Get(req, "thinking", "type")) == "disabled"
	if effort := outputEffort(req["output_config"]); effort != "" && !thinkingOff {
		out["reasoning_effort"] = effort
	}
	format := jsonx.Get(req, "output_config", "format")
	if format == nil {
		format = req["output_format"]
	}
	if rf := responseFormat(format); rf != nil {
		out["response_format"] = rf
	}
	if user := jsonx.Str(jsonx.Get(req, "metadata", "user_id")); user != "" {
		out["user"] = user
	}

	if thinkingOff && model.ThinkingOff != nil {
		out = deepMerge(out, jsonx.Clone(model.ThinkingOff).(obj))
	}
	if contextWindow > 0 {
		clampMaxTokens(out, contextWindow)
	}
	return out
}

// systemText is the top-level system field (a string or text blocks),
// joined in order.
func systemText(s any) string {
	if str, ok := s.(string); ok {
		return str
	}
	var parts []string
	for _, b := range jsonx.Arr(s) {
		if t := jsonx.Str(jsonx.Get(b, "text")); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}

func translateMessages(msgs []any) []any {
	var out []any
	for _, m := range msgs {
		content := jsonx.Get(m, "content")
		switch jsonx.Str(jsonx.Get(m, "role")) {
		case "user":
			out = append(out, userMessage(content)...)
		case "assistant":
			out = append(out, assistantMessage(content)...)
		case "system":
			// Many templates refuse a second system message.
			if text := systemText(content); text != "" {
				out = append(out, obj{"role": "user", "content": "<system-reminder>\n" + text + "\n</system-reminder>"})
			}
		}
	}
	return out
}

func textPart(s string) obj { return obj{"type": "text", "text": s} }

// userMessage is one Anthropic user message: its tool_result blocks as
// tool messages FIRST (they must follow the assistant message that called
// them), then one user message with tool attachments followed by the
// user's parts.
func userMessage(content any) []any {
	if content == nil {
		return nil
	}
	if s, ok := content.(string); ok {
		return []any{obj{"role": "user", "content": s}}
	}
	blocks, ok := content.([]any)
	if !ok {
		return []any{obj{"role": "user", "content": jsonx.String(content)}}
	}
	var toolMessages, toolMedia, parts []any
	for _, b := range blocks {
		switch jsonx.Type(b) {
		case "text", media.AttachmentTextType:
			parts = append(parts, textPart(jsonx.Str(jsonx.Get(b, "text"))))
		case "image":
			parts = append(parts, imagePart(b))
		case "document":
			// Not read by media processing (unknown source): a note.
			parts = append(parts, textPart(documentNote(b, "unsupported document source")))
		case "tool_result":
			id := jsonx.Str(jsonx.Get(b, "tool_use_id"))
			text, med := splitToolResult(jsonx.Get(b, "content"))
			if len(med) > 0 {
				if strings.TrimSpace(text) == "" {
					text = "[the tool returned attachments; they follow in the next message]"
				}
				toolMedia = append(toolMedia, textPart("[attachments from tool result "+id+"]"))
				toolMedia = append(toolMedia, med...)
			}
			if jsonx.Bool(jsonx.Get(b, "is_error")) {
				text = "[tool error] " + text
			}
			toolMessages = append(toolMessages, obj{"role": "tool", "tool_call_id": id, "content": text})
		case "web_search_tool_result":
			var lines strings.Builder
			for _, r := range jsonx.Arr(jsonx.Get(b, "content")) {
				t, u := jsonx.Str(jsonx.Get(r, "title")), jsonx.Str(jsonx.Get(r, "url"))
				if t != "" || u != "" {
					lines.WriteString("\n- " + t + " (" + u + ")")
				}
			}
			if lines.Len() > 0 {
				parts = append(parts, textPart("[Web search results:"+lines.String()+"]"))
			}
		case "search_result":
			if t := searchResultText(b); t != "" {
				parts = append(parts, textPart(t))
			}
		}
		// thinking, redacted_thinking and unknown blocks are dropped.
	}
	out := toolMessages
	all := append(toolMedia, parts...)
	if len(all) == 1 && jsonx.Type(all[0]) == "text" {
		out = append(out, obj{"role": "user", "content": jsonx.Get(all[0], "text")})
	} else if len(all) > 0 {
		out = append(out, obj{"role": "user", "content": all})
	}
	return out
}

func imagePart(b any) obj {
	src := jsonx.Get(b, "source")
	switch jsonx.Type(src) {
	case "base64":
		mt := jsonx.Str(jsonx.Get(src, "media_type"))
		if mt == "" {
			mt = "image/png"
		}
		return obj{"type": "image_url", "image_url": obj{"url": "data:" + mt + ";base64," + jsonx.Str(jsonx.Get(src, "data"))}}
	case "url":
		u := jsonx.Get(src, "url")
		if u == nil {
			u = ""
		}
		return obj{"type": "image_url", "image_url": obj{"url": u}}
	}
	return textPart("[unsupported image format]")
}

func documentNote(b any, why string) string {
	title := jsonx.Str(jsonx.Get(b, "title"))
	if title == "" {
		title = jsonx.Str(jsonx.Get(b, "name"))
	}
	title = strings.Trim(title, " \t")
	if title == "" {
		title = "document"
	}
	return "[document: " + title + " could not be read (" + why + ")]"
}

func searchResultText(b any) string {
	var lines []string
	if t := jsonx.Str(jsonx.Get(b, "title")); t != "" {
		lines = append(lines, t)
	}
	for _, c := range jsonx.Arr(jsonx.Get(b, "content")) {
		lines = append(lines, jsonx.Str(jsonx.Get(c, "text")))
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n\r")
}

// splitToolResult is a tool_result's content as the tool message text and
// the media parts a tool message cannot carry.
func splitToolResult(content any) (string, []any) {
	if content == nil {
		return "", nil
	}
	if s, ok := content.(string); ok {
		return s, nil
	}
	blocks, ok := content.([]any)
	if !ok {
		return jsonx.String(content), nil
	}
	var texts []string
	var med []any
	for _, b := range blocks {
		switch jsonx.Type(b) {
		case "text":
			if t := jsonx.Str(jsonx.Get(b, "text")); t != "" {
				texts = append(texts, t)
			}
		case media.AttachmentTextType:
			med = append(med, textPart(jsonx.Str(jsonx.Get(b, "text"))))
		case "image":
			med = append(med, imagePart(b))
		case "document":
			med = append(med, textPart(documentNote(b, "unsupported document source")))
		case "tool_reference":
			if name := jsonx.Str(jsonx.Get(b, "tool_name")); name != "" {
				texts = append(texts, "Tool "+name+" is now loaded and can be called.")
			}
		case "search_result":
			if t := searchResultText(b); t != "" {
				texts = append(texts, t)
			}
		}
	}
	return strings.Join(texts, "\n"), med
}

// assistantMessage is one Anthropic assistant message. Text becomes
// content, tool_use tool_calls, thinking reasoning_content and reasoning;
// redacted_thinking is dropped. A web search the engine ran earlier
// (server_tool_use + its web_search_tool_result in this message) is
// replayed as the function call and tool result the model made and saw, so
// the answer can be several chat messages.
func assistantMessage(content any) []any {
	if content == nil {
		return nil
	}
	if s, ok := content.(string); ok {
		return []any{obj{"role": "assistant", "content": s}}
	}
	blocks, ok := content.([]any)
	if !ok {
		return []any{obj{"role": "assistant", "content": jsonx.String(content)}}
	}
	results := map[string]bool{}
	for _, b := range blocks {
		if jsonx.Type(b) == "web_search_tool_result" {
			if id, ok := jsonx.StrOK(jsonx.Get(b, "tool_use_id")); ok {
				results[id] = true
			}
		}
	}
	var out, deferred []any
	var texts, thinking []string
	var calls []any
	clientCalls := 0
	flush := func() obj {
		m := obj{"role": "assistant"}
		if text := strings.Join(texts, ""); text != "" {
			m["content"] = text
		} else if len(calls) == 0 {
			m["content"] = ""
		} else {
			m["content"] = nil
		}
		if len(thinking) > 0 {
			r := strings.Join(thinking, "\n")
			m["reasoning_content"] = r
			m["reasoning"] = r
		}
		if len(calls) > 0 {
			m["tool_calls"] = calls
		}
		texts, thinking, calls, clientCalls = nil, nil, nil, 0
		return m
	}
	for _, b := range blocks {
		switch jsonx.Type(b) {
		case "text":
			texts = append(texts, jsonx.Str(jsonx.Get(b, "text")))
		case "thinking":
			if t := jsonx.Str(jsonx.Get(b, "thinking")); t != "" {
				thinking = append(thinking, t)
			}
		case "tool_use":
			calls = append(calls, functionCall(jsonx.Str(jsonx.Get(b, "id")), jsonx.Str(jsonx.Get(b, "name")), jsonx.Get(b, "input")))
			clientCalls++
		case "server_tool_use":
			id := jsonx.Str(jsonx.Get(b, "id"))
			query := jsonx.Str(jsonx.Get(b, "input", "query"))
			if query == "" {
				continue
			}
			if results[id] {
				calls = append(calls, functionCall(id, webSearchFunction, jsonx.Get(b, "input")))
			} else {
				texts = append(texts, "[Searched the web: "+query+"]")
			}
		case "web_search_tool_result":
			id := jsonx.Str(jsonx.Get(b, "tool_use_id"))
			found := false
			for _, c := range calls {
				if jsonx.Str(jsonx.Get(c, "id")) == id {
					found = true
				}
			}
			if !found {
				continue
			}
			tool := obj{"role": "tool", "tool_call_id": id, "content": historicSearchText(jsonx.Get(b, "content"))}
			if clientCalls > 0 {
				deferred = append(deferred, tool) // the client's results come in the next user message
			} else {
				out = append(out, flush(), tool)
			}
		}
	}
	if len(texts) > 0 || len(thinking) > 0 || len(calls) > 0 || len(out) == 0 {
		out = append(out, flush())
	}
	return append(out, deferred...)
}

func functionCall(id, name string, input any) obj {
	args := "{}"
	if input != nil {
		args = jsonx.String(input)
	}
	return obj{"id": id, "type": "function", "function": obj{"name": name, "arguments": args}}
}

// referencedTools are the tools Claude Code's ToolSearch loaded: every
// tool_reference in a tool_result of the conversation.
func referencedTools(msgs []any) map[string]bool {
	names := map[string]bool{}
	for _, m := range msgs {
		for _, b := range jsonx.Arr(jsonx.Get(m, "content")) {
			if jsonx.Type(b) != "tool_result" {
				continue
			}
			for _, c := range jsonx.Arr(jsonx.Get(b, "content")) {
				if jsonx.Type(c) == "tool_reference" {
					if n := jsonx.Str(jsonx.Get(c, "tool_name")); n != "" {
						names[n] = true
					}
				}
			}
		}
	}
	return names
}

// translateTools gives the function tools. A deferred tool (defer_loading)
// is sent only once a tool_reference has loaded it; DeferredToolPlaceholder
// never; server tools are dropped (web search is offered separately).
func translateTools(tools []any, loaded map[string]bool) []any {
	var out []any
	for _, t := range tools {
		name := jsonx.Str(jsonx.Get(t, "name"))
		if name == "" || name == "DeferredToolPlaceholder" {
			continue
		}
		if jsonx.Bool(jsonx.Get(t, "defer_loading")) && !loaded[name] {
			continue
		}
		if typ, ok := jsonx.StrOK(jsonx.Get(t, "type")); ok && typ != "custom" {
			continue
		}
		fn := obj{"name": name}
		if d := jsonx.Str(jsonx.Get(t, "description")); d != "" {
			fn["description"] = d
		}
		if s := jsonx.Get(t, "input_schema"); s != nil {
			fn["parameters"] = s
		}
		out = append(out, obj{"type": "function", "function": fn})
	}
	return out
}

func toolChoice(tc any) any {
	switch jsonx.Type(tc) {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		name := jsonx.Get(tc, "name")
		if name == nil {
			name = ""
		}
		return obj{"type": "function", "function": obj{"name": name}}
	}
	return nil
}

// outputEffort is output_config.effort as reasoning_effort (max → xhigh).
func outputEffort(oc any) string {
	e := strings.ToLower(strings.Trim(jsonx.Str(jsonx.Get(oc, "effort")), " \t"))
	if e == "max" {
		return "xhigh"
	}
	return e
}

// responseFormat is a json_schema output format as a strict Chat
// Completions response_format.
func responseFormat(f any) any {
	if jsonx.Type(f) != "json_schema" {
		return nil
	}
	schema := jsonx.Get(f, "schema")
	if schema == nil {
		return nil
	}
	return obj{"type": "json_schema", "json_schema": obj{"name": "output", "schema": schema, "strict": true}}
}

// deepMerge merges src into dst, src winning; nested objects merged key by key.
func deepMerge(dst, src obj) obj {
	for k, v := range src {
		if d, ok := dst[k].(obj); ok {
			if s, ok := v.(obj); ok {
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
// headroom); when it leaves no sensible room the request goes as-is and
// the backend's context error (rewritten for the client) takes over.
func clampMaxTokens(req obj, contextWindow int) {
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
