package messages

import (
	"strings"

	"codelink/internal/apierr"
	"codelink/internal/httpx"
	"codelink/internal/jsonx"
	"codelink/internal/upstream"
)

// lineResult is what one upstream line did to the translator.
type lineResult int

const (
	lineMore lineResult = iota
	lineDone            // data: [DONE]
	lineFailed
)

type toolState struct {
	callID     string
	name       string
	blockIndex int
	held       bool
	args       string
}

// streamTranslator translates a Chat Completions SSE stream into Anthropic
// Messages events (docs/DESIGN.md §4.2), one upstream line at a time. In
// search mode one message spans several generations ("rounds", §4.5): the
// model's web_search calls are held back from the client and answered
// between rounds.
type streamTranslator struct {
	sink          *httpx.Stream
	model         string
	stopSequences []string
	searchMode    bool
	msgID         string

	started      bool
	done         bool // message_stop or error sent
	blockIndex   int
	textOpen     bool
	thinkingOpen bool
	tools        map[int]*toolState
	toolOrder    []*toolState // this round's client tool calls, in order
	held         []*toolState // this round's web_search calls (search mode)
	finishReason string
	backendStop  string
	usage        *chatUsage
	usageSum     chatUsage
	think        thinkFilter
	roundClosed  bool
	roundText    strings.Builder
	roundReason  strings.Builder
	searches     int
	failure      *apierr.Anthropic // the error the stream ended with
}

func newStreamTranslator(sink *httpx.Stream, model string, stopSequences []string, searchMode bool) *streamTranslator {
	return &streamTranslator{sink: sink, model: model, stopSequences: stopSequences, searchMode: searchMode,
		msgID: newID("msg_"), tools: map[int]*toolState{}}
}

func (t *streamTranslator) emit(event string, data obj) { t.sink.Event(event, data) }

// start sends message_start and a first ping.
func (t *streamTranslator) start() {
	if t.started {
		return
	}
	t.started = true
	t.emit("message_start", obj{"message": obj{
		"id": t.msgID, "type": "message", "role": "assistant", "content": []any{}, "model": t.model,
		"stop_reason": nil, "stop_sequence": nil,
		"usage": obj{"input_tokens": 0, "output_tokens": 0, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0},
	}})
	t.sink.Ping()
}

// line takes one upstream line.
func (t *streamTranslator) line(line string) lineResult {
	if t.done {
		return lineFailed
	}
	rest, ok := strings.CutPrefix(line, "data:")
	if !ok {
		return lineMore // event:, id:, comments, blank lines
	}
	payload := strings.Trim(rest, " \t")
	if payload == "[DONE]" {
		return lineDone
	}
	v, err := jsonx.Parse([]byte(payload))
	chunk := jsonx.Obj(v)
	if err != nil || chunk == nil {
		return lineMore
	}
	t.start()
	if e, ok := chunk["error"]; ok && e != nil {
		if _, has := chunk["choices"]; !has {
			t.fail(inBandError(e))
			return lineFailed
		}
	}
	if u, ok := parseUsage(chunk["usage"]); ok {
		t.usage = &u
	}
	choice := jsonx.Get(chunk, "choices", 0)
	if choice == nil {
		return lineMore
	}
	delta := jsonx.Get(choice, "delta")
	reasoning := jsonx.Str(jsonx.Get(delta, "reasoning_content"))
	if reasoning == "" {
		reasoning = jsonx.Str(jsonx.Get(delta, "reasoning"))
	}
	if reasoning != "" {
		t.thinking(reasoning)
	}
	if c := jsonx.Str(jsonx.Get(delta, "content")); c != "" {
		t.segments(t.think.process(c))
	}
	for _, tc := range jsonx.Arr(jsonx.Get(delta, "tool_calls")) {
		t.toolDelta(tc)
	}
	if f := jsonx.Str(jsonx.Get(choice, "finish_reason")); f != "" {
		t.finishReason = f
	}
	if s := jsonx.Str(jsonx.Get(choice, "stop_reason")); s != "" {
		t.backendStop = s
	}
	return lineMore
}

func (t *streamTranslator) segments(segs []segment) {
	for _, s := range segs {
		if s.reasoning {
			t.thinking(s.text)
		} else {
			t.text(s.text)
		}
	}
}

// inBandError is the client error for an in-band {"error": …} frame.
func inBandError(e any) apierr.Anthropic {
	message, ok := e.(string)
	if !ok {
		message = jsonx.Str(jsonx.Get(e, "message"))
	}
	if message == "" {
		message = "the backend reported an error"
	}
	code, ok := jsonx.Int(jsonx.Get(e, "code"))
	if !ok {
		code, _ = jsonx.Int(jsonx.Get(e, "status"))
	}
	if r := apierr.RewriteContextLength(apierr.UpstreamInfo{Message: message, Type: jsonx.Str(jsonx.Get(e, "type"))}); r != "" {
		return apierr.Invalid(r)
	}
	if code >= 400 && code < 500 {
		out := apierr.FromUpstream(code, nil, nil, false)
		return apierr.Anthropic{Status: out.Status, Type: out.Type, Message: apierr.Scrub(message)}
	}
	return apierr.Anthropic{Status: 529, Type: "overloaded_error", Message: apierr.Scrub(message)}
}

func (t *streamTranslator) toolDelta(tc any) {
	index, _ := jsonx.Int(jsonx.Get(tc, "index"))
	id := jsonx.Str(jsonx.Get(tc, "id"))
	args := jsonx.Str(jsonx.Get(tc, "function", "arguments"))
	state := t.tools[index]
	if state == nil || (id != "" && id != state.callID) {
		name := jsonx.Str(jsonx.Get(tc, "function", "name"))
		if state == nil && id == "" && name == "" && args == "" {
			return
		}
		t.closeThinking()
		t.closeText()
		callID := id
		if callID == "" {
			callID = newID("toolu_")
		}
		isHeld := t.searchMode && name == webSearchFunction
		state = &toolState{callID: callID, name: name, blockIndex: -1, held: isHeld}
		t.tools[index] = state
		if isHeld {
			t.held = append(t.held, state)
		} else {
			state.blockIndex = t.blockIndex
			t.toolOrder = append(t.toolOrder, state)
			t.emit("content_block_start", obj{"index": t.blockIndex, "content_block": obj{
				"type": "tool_use", "id": callID, "name": name, "input": obj{}}})
			t.blockIndex++
		}
	}
	if args == "" {
		return
	}
	state.args += args
	if !state.held {
		t.emit("content_block_delta", obj{"index": state.blockIndex,
			"delta": obj{"type": "input_json_delta", "partial_json": args}})
	}
}

func (t *streamTranslator) thinking(s string) {
	t.closeText()
	if !t.thinkingOpen {
		t.emit("content_block_start", obj{"index": t.blockIndex, "content_block": obj{"type": "thinking", "thinking": "", "signature": ""}})
		t.thinkingOpen = true
	}
	if t.searchMode {
		t.roundReason.WriteString(s)
	}
	t.emit("content_block_delta", obj{"index": t.blockIndex, "delta": obj{"type": "thinking_delta", "thinking": s}})
}

func (t *streamTranslator) text(s string) {
	t.closeThinking()
	if !t.textOpen {
		t.emit("content_block_start", obj{"index": t.blockIndex, "content_block": obj{"type": "text", "text": ""}})
		t.textOpen = true
	}
	if t.searchMode {
		t.roundText.WriteString(s)
	}
	t.emit("content_block_delta", obj{"index": t.blockIndex, "delta": obj{"type": "text_delta", "text": s}})
}

func (t *streamTranslator) closeThinking() {
	if !t.thinkingOpen {
		return
	}
	t.emit("content_block_delta", obj{"index": t.blockIndex, "delta": obj{"type": "signature_delta", "signature": signature}})
	t.emit("content_block_stop", obj{"index": t.blockIndex})
	t.blockIndex++
	t.thinkingOpen = false
}

func (t *streamTranslator) closeText() {
	if !t.textOpen {
		return
	}
	t.emit("content_block_stop", obj{"index": t.blockIndex})
	t.blockIndex++
	t.textOpen = false
}

// finishRound closes everything the generation left open and adds its
// usage to the message's total.
func (t *streamTranslator) finishRound() {
	if t.roundClosed || t.done {
		return
	}
	t.roundClosed = true
	t.start()
	t.segments(t.think.flush())
	t.closeThinking()
	t.closeText()
	for _, s := range t.toolOrder {
		t.emit("content_block_stop", obj{"index": s.blockIndex})
	}
	if t.usage != nil {
		t.usageSum.add(*t.usage)
		t.usage = nil
	}
}

// finalize sends message_delta (stop reason, every round's usage) and
// message_stop.
func (t *streamTranslator) finalize() {
	if t.done {
		return
	}
	t.finishRound()
	stop, seq := resolveStopReason(t.finishReason, t.backendStop, t.stopSequences, len(t.toolOrder) > 0)
	searches := -1
	if t.searchMode {
		searches = t.searches
		if len(t.toolOrder) == 0 {
			// Held web_search calls are not the client's tool calls.
			switch {
			case t.finishReason == "length":
				stop = "max_tokens"
			case stop != "stop_sequence":
				stop = "end_turn"
			}
			if stop != "stop_sequence" {
				seq = ""
			}
		}
	}
	t.emit("message_delta", obj{"delta": obj{"stop_reason": stop, "stop_sequence": orNull(seq)},
		"usage": t.usageSum.anthropic(searches)})
	t.emit("message_stop", nil)
	t.done = true
}

// fail ends the stream with an error event (open blocks closed first).
func (t *streamTranslator) fail(e apierr.Anthropic) {
	if t.done {
		return
	}
	t.start()
	t.closeThinking()
	t.closeText()
	t.done = true
	t.failure = &e
	t.emit("error", obj{"error": obj{"type": e.Type, "message": e.Message}})
}

// status is the status to log for the stream.
func (t *streamTranslator) status() int {
	if t.failure != nil {
		return t.failure.Status
	}
	return 200
}

// nextRound readies the translator for the model's next generation
// (search mode).
func (t *streamTranslator) nextRound() {
	t.tools = map[int]*toolState{}
	t.toolOrder, t.held = nil, nil
	t.finishReason, t.backendStop, t.usage = "", "", nil
	t.roundClosed = false
	t.think = thinkFilter{}
	t.roundText.Reset()
	t.roundReason.Reset()
}

// serverSearch shows the client one search the engine ran, as the
// Anthropic API streams it: server_tool_use and its web_search_tool_result.
func (t *streamTranslator) serverSearch(o searchOutcome) {
	t.start()
	t.emit("content_block_start", obj{"index": t.blockIndex, "content_block": obj{
		"type": "server_tool_use", "id": o.id, "name": webSearchFunction, "input": obj{}}})
	t.emit("content_block_delta", obj{"index": t.blockIndex, "delta": obj{
		"type": "input_json_delta", "partial_json": jsonx.String(obj{"query": o.query})}})
	t.emit("content_block_stop", obj{"index": t.blockIndex})
	t.blockIndex++
	t.emit("content_block_start", obj{"index": t.blockIndex, "content_block": obj{
		"type": "web_search_tool_result", "tool_use_id": o.id, "content": o.resultContent()}})
	t.emit("content_block_stop", obj{"index": t.blockIndex})
	t.blockIndex++
	if o.errorCode == "" {
		t.searches++
	}
}

// pump reads one generation into the translator. True when it completed:
// a finish reason arrived and the stream was not cut.
func pump(resp *upstream.Response, t *streamTranslator) bool {
	defer resp.Close()
	lines := resp.Lines()
	for {
		line, ok, err := lines.Next()
		if err != nil {
			return false
		}
		if !ok {
			return t.finishReason != ""
		}
		switch t.line(line) {
		case lineDone:
			return t.finishReason != ""
		case lineFailed:
			return false
		}
	}
}

// emitMessageAsSSE sends a complete (non-streaming) Anthropic message as
// the event sequence a streaming request expects.
func emitMessageAsSSE(msg obj, sink *httpx.Stream) {
	start := obj{}
	for k, v := range msg {
		start[k] = v
	}
	start["content"] = []any{}
	start["stop_reason"] = nil
	start["stop_sequence"] = nil
	u := obj{}
	for k, v := range jsonx.Obj(msg["usage"]) {
		u[k] = v
	}
	u["output_tokens"] = 0
	start["usage"] = u
	sink.Event("message_start", obj{"message": start})
	sink.Ping()
	for i, b := range jsonx.Arr(msg["content"]) {
		orEmpty := func(v any) any {
			if v == nil {
				return ""
			}
			return v
		}
		switch jsonx.Type(b) {
		case "text":
			sink.Event("content_block_start", obj{"index": i, "content_block": obj{"type": "text", "text": ""}})
			sink.Event("content_block_delta", obj{"index": i, "delta": obj{"type": "text_delta", "text": orEmpty(jsonx.Get(b, "text"))}})
		case "thinking":
			sig := jsonx.Get(b, "signature")
			if sig == nil {
				sig = signature
			}
			sink.Event("content_block_start", obj{"index": i, "content_block": obj{"type": "thinking", "thinking": "", "signature": ""}})
			sink.Event("content_block_delta", obj{"index": i, "delta": obj{"type": "thinking_delta", "thinking": orEmpty(jsonx.Get(b, "thinking"))}})
			sink.Event("content_block_delta", obj{"index": i, "delta": obj{"type": "signature_delta", "signature": sig}})
		case "tool_use", "server_tool_use":
			cb := obj{}
			for k, v := range jsonx.Obj(b) {
				cb[k] = v
			}
			input := jsonx.Get(b, "input")
			if input == nil {
				input = obj{}
			}
			cb["input"] = obj{}
			sink.Event("content_block_start", obj{"index": i, "content_block": cb})
			sink.Event("content_block_delta", obj{"index": i, "delta": obj{"type": "input_json_delta", "partial_json": jsonx.String(input)}})
		default:
			sink.Event("content_block_start", obj{"index": i, "content_block": b})
		}
		sink.Event("content_block_stop", obj{"index": i})
	}
	stop := msg["stop_reason"]
	if stop == nil {
		stop = "end_turn"
	}
	usage := msg["usage"]
	if usage == nil {
		usage = obj{}
	}
	sink.Event("message_delta", obj{"delta": obj{"stop_reason": stop, "stop_sequence": msg["stop_sequence"]}, "usage": usage})
	sink.Event("message_stop", nil)
}
