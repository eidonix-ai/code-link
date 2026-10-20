package messages

import (
	"slices"

	"codelink/internal/jsonx"
)

// signature is the thinking block signature code-link gives.
const signature = "code-link"

// chatUsage is Chat Completions usage.
type chatUsage struct {
	prompt, completion, cached, reasoning int
}

// parseUsage reads a usage object; ok false when v isn't one.
func parseUsage(v any) (chatUsage, bool) {
	o := jsonx.Obj(v)
	if o == nil {
		return chatUsage{}, false
	}
	var u chatUsage
	u.prompt, _ = jsonx.Int(o["prompt_tokens"])
	u.completion, _ = jsonx.Int(o["completion_tokens"])
	u.cached, _ = jsonx.Int(jsonx.Get(o, "prompt_tokens_details", "cached_tokens"))
	u.reasoning, _ = jsonx.Int(jsonx.Get(o, "completion_tokens_details", "reasoning_tokens"))
	return u, true
}

func (u *chatUsage) add(o chatUsage) {
	u.prompt += o.prompt
	u.completion += o.completion
	u.cached += o.cached
	u.reasoning += o.reasoning
}

// anthropic is Anthropic usage: cached prompt tokens are cache reads, not
// input. webSearches < 0 = no server_tool_use.
func (u chatUsage) anthropic(webSearches int) obj {
	cached := min(max(u.cached, 0), u.prompt)
	out := obj{"input_tokens": u.prompt - cached, "output_tokens": u.completion,
		"cache_creation_input_tokens": 0, "cache_read_input_tokens": cached}
	if webSearches >= 0 {
		out["server_tool_use"] = obj{"web_search_requests": webSearches}
	}
	return out
}

// resolveStopReason is the Anthropic stop_reason and stop_sequence for a
// Chat Completions finish. Any turn with tool calls ends with tool_use; a
// "stop" whose backend stop_reason names one of the request's stop
// sequences is stop_sequence.
func resolveStopReason(finish, backendStop string, stopSequences []string, sawToolCalls bool) (string, string) {
	if sawToolCalls && (finish == "stop" || finish == "tool_calls" || finish == "") {
		return "tool_use", ""
	}
	if finish == "stop" && backendStop != "" && slices.Contains(stopSequences, backendStop) {
		return "stop_sequence", backendStop
	}
	switch finish {
	case "tool_calls", "function_call":
		return "tool_use", ""
	case "length":
		return "max_tokens", ""
	}
	return "end_turn", ""
}

func orNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// translateMessage is a non-streaming Chat Completions response as an
// Anthropic message (docs/DESIGN.md §4.2); nil when it has no choices.
func translateMessage(chat any, model string, stopSequences []string) obj {
	choice := jsonx.Obj(jsonx.Get(chat, "choices", 0))
	if choice == nil {
		return nil
	}
	msg := choice["message"]
	content := []any{}
	reasoning := jsonx.Str(jsonx.Get(msg, "reasoning_content"))
	if reasoning == "" {
		reasoning = jsonx.Str(jsonx.Get(msg, "reasoning"))
	}
	if reasoning != "" {
		content = append(content, obj{"type": "thinking", "thinking": reasoning, "signature": signature})
	}
	if c := jsonx.Str(jsonx.Get(msg, "content")); c != "" {
		think, text := stripThink(c)
		if think != "" && reasoning == "" {
			content = append(content, obj{"type": "thinking", "thinking": think, "signature": signature})
		}
		if text != "" {
			content = append(content, obj{"type": "text", "text": text})
		}
	}
	calls := jsonx.Arr(jsonx.Get(msg, "tool_calls"))
	for _, tc := range calls {
		input := any(obj{})
		if args, ok := jsonx.StrOK(jsonx.Get(tc, "function", "arguments")); ok {
			if p := jsonx.ParseObject([]byte(args)); p != nil {
				input = p
			}
		}
		id := jsonx.Str(jsonx.Get(tc, "id"))
		if id == "" {
			id = newID("toolu_")
		}
		name := jsonx.Get(tc, "function", "name")
		if name == nil {
			name = ""
		}
		content = append(content, obj{"type": "tool_use", "id": id, "name": name, "input": input})
	}
	stop, seq := resolveStopReason(jsonx.Str(choice["finish_reason"]), jsonx.Str(choice["stop_reason"]), stopSequences, len(calls) > 0)
	u, _ := parseUsage(jsonx.Get(chat, "usage"))
	return obj{"id": newID("msg_"), "type": "message", "role": "assistant", "content": content,
		"model": model, "stop_reason": stop, "stop_sequence": orNull(seq), "usage": u.anthropic(-1)}
}
