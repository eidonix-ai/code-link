package responses

import (
	"strings"
	"time"

	"codelink/internal/ids"
	"codelink/internal/jsonx"
	"codelink/internal/thinktag"
)

// usage is a Chat Completions usage.
type usage struct {
	prompt, completion, cached, reasoning int
}

// parseUsage reads a usage object; ok false when it isn't one.
func parseUsage(v any) (usage, bool) {
	o := jsonx.Obj(v)
	if o == nil {
		return usage{}, false
	}
	var u usage
	u.prompt, _ = jsonx.Int(o["prompt_tokens"])
	u.completion, _ = jsonx.Int(o["completion_tokens"])
	u.cached, _ = jsonx.Int(jsonx.Get(o, "prompt_tokens_details", "cached_tokens"))
	u.reasoning, _ = jsonx.Int(jsonx.Get(o, "completion_tokens_details", "reasoning_tokens"))
	return u, true
}

func (u *usage) add(o usage) {
	u.prompt += o.prompt
	u.completion += o.completion
	u.cached += o.cached
	u.reasoning += o.reasoning
}

// responses is the Responses API usage, with the cached and reasoning
// token details.
func (u usage) responses() map[string]any {
	return map[string]any{
		"input_tokens": u.prompt, "input_tokens_details": map[string]any{"cached_tokens": u.cached},
		"output_tokens": u.completion, "output_tokens_details": map[string]any{"reasoning_tokens": u.reasoning},
		"total_tokens": u.prompt + u.completion,
	}
}

// reasoningHeading is the summary's first line for a local model's
// reasoning (which has no summary of its own): Codex shows it in its status
// line while the model thinks.
const reasoningHeading = "**Thinking**"

// reasoningSummary is the heading and then the reasoning: Codex lists it in
// the expanded transcript (Ctrl+T), not in the main view.
func reasoningSummary(text string) string {
	if body := strings.TrimSpace(text); body != "" {
		return reasoningHeading + "\n\n" + body
	}
	return reasoningHeading
}

// reasoningItem is a completed reasoning item: the summary as above, and the
// text as reasoning_text content (what clients replay in later requests).
func reasoningItem(id, text string) map[string]any {
	return map[string]any{"id": id, "type": "reasoning",
		"summary": []any{map[string]any{"type": "summary_text", "text": reasoningSummary(text)}},
		"content": []any{map[string]any{"type": "reasoning_text", "text": text}}}
}

// searchCall is a model's web_search call held back from the client.
type searchCall struct{ id, args string }

// round is one generation's output items (a complete, non-streaming
// Chat Completions response as Responses output).
type round struct {
	items        []any
	text         string
	searchCalls  []searchCall
	finishReason string
	usage        usage
}

// roundOf gives the items of a chat completion: reasoning, the answer,
// then the tool calls — in the order the stream emits them. ok false when
// it has no choice.
func roundOf(chat any, reg *registry, jsonMode, searchMode bool) (round, bool) {
	choice := jsonx.Get(chat, "choices", 0)
	if choice == nil {
		return round{}, false
	}
	msg := jsonx.Get(choice, "message")
	var r round
	r.finishReason = jsonx.Str(jsonx.Get(choice, "finish_reason"))
	r.usage, _ = parseUsage(jsonx.Get(chat, "usage"))

	reasoning := jsonx.Str(jsonx.Get(msg, "reasoning_content"))
	if reasoning == "" {
		reasoning = jsonx.Str(jsonx.Get(msg, "reasoning"))
	}
	text := ""
	if c := jsonx.Str(jsonx.Get(msg, "content")); c != "" {
		think, rest := thinktag.Strip(c)
		text = rest
		if reasoning == "" {
			reasoning = think
		}
	}
	calls := jsonx.Arr(jsonx.Get(msg, "tool_calls"))
	if jsonMode && len(calls) == 0 {
		text = cleanStructuredOutput(text)
	}
	if reasoning != "" {
		r.items = append(r.items, reasoningItem(ids.New("rs_"), reasoning))
	}
	if text != "" {
		r.text = text
		r.items = append(r.items, map[string]any{"id": ids.New("msg_"), "type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}})
	}
	for _, tc := range calls {
		name := jsonx.Str(jsonx.Get(tc, "function", "name"))
		args := jsonx.Str(jsonx.Get(tc, "function", "arguments"))
		id := jsonx.Str(jsonx.Get(tc, "id"))
		if id == "" {
			id = ids.New("call_")
		}
		if searchMode && name == webSearchFunction {
			r.searchCalls = append(r.searchCalls, searchCall{id, args})
			continue
		}
		if args == "" {
			args = "{}"
		}
		r.items = append(r.items, reg.outputItem(ids.New("fc_"), id, name, args, false))
	}
	return r, true
}

// sources are a search's hits as url_citation sources.
func sources(o searchOutcome) []any {
	var out []any
	for _, h := range o.hits {
		out = append(out, map[string]any{"type": "url_citation", "url": h.URL, "title": h.Title})
	}
	return out
}

// webSearchItem is the web_search_call item of a search the engine ran.
func webSearchItem(id string, o searchOutcome) map[string]any {
	status := "completed"
	if o.errorCode != "" {
		status = "failed"
	}
	item := map[string]any{"id": id, "type": "web_search_call", "status": status,
		"action": map[string]any{"type": "search", "query": o.query}}
	if len(o.hits) > 0 {
		item["sources"] = sources(o)
	}
	return item
}

// responseObject is the response of a finished generation (or a search
// loop's).
func responseObject(model string, items []any, text, finishReason string, u usage) map[string]any {
	status := "completed"
	var incomplete, errObj any
	switch finishReason {
	case "length":
		status = "incomplete"
		incomplete = map[string]any{"reason": "max_output_tokens"}
	case "content_filter":
		status = "failed"
		errObj = map[string]any{"code": "content_filter", "message": "the response was blocked by the backend's content filter"}
	}
	if items == nil {
		items = []any{}
	}
	return map[string]any{"id": ids.New("resp_"), "object": "response", "created_at": time.Now().Unix(),
		"model": model, "status": status, "error": errObj, "incomplete_details": incomplete,
		"output": items, "output_text": text, "usage": u.responses()}
}

// chunkLine is a completion as one stream chunk line, for a backend that
// answered a streaming request with a plain JSON body.
func chunkLine(chat any) (string, bool) {
	choice := jsonx.Get(chat, "choices", 0)
	if choice == nil {
		return "", false
	}
	msg := jsonx.Get(choice, "message")
	delta := map[string]any{"role": "assistant"}
	for _, k := range []string{"content", "reasoning_content", "reasoning"} {
		if v := jsonx.Get(msg, k); v != nil {
			delta[k] = v
		}
	}
	if calls := jsonx.Arr(jsonx.Get(msg, "tool_calls")); len(calls) > 0 {
		indexed := make([]any, len(calls))
		for i, c := range calls {
			call := map[string]any{}
			for k, v := range jsonx.Obj(c) {
				call[k] = v
			}
			call["index"] = i
			indexed[i] = call
		}
		delta["tool_calls"] = indexed
	}
	finish := jsonx.Get(choice, "finish_reason")
	if _, ok := jsonx.Obj(choice)["finish_reason"]; !ok {
		finish = "stop"
	}
	out := map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
	if u, ok := jsonx.Obj(chat)["usage"]; ok {
		out["usage"] = u
	}
	return "data: " + jsonx.String(out), true
}
