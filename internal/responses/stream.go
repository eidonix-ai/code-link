package responses

import (
	"sort"
	"strings"
	"time"

	"codelink/internal/apierr"
	"codelink/internal/httpx"
	"codelink/internal/ids"
	"codelink/internal/jsonx"
	"codelink/internal/thinktag"
	"codelink/internal/upstream"
)

// lineResult is what one upstream line did to the translator.
type lineResult int

const (
	lineMore lineResult = iota
	lineDone            // data: [DONE]
	lineFailed
)

// streamCall is a tool call being streamed.
type streamCall struct {
	itemID      string
	callID      string
	name        string
	custom      bool
	held        bool
	outputIndex int
	args        string
}

type openItem struct {
	id    string
	index int
	text  string
}

type finishedItem struct {
	index int
	item  map[string]any
}

// translator turns a Chat Completions SSE stream into Responses API events
// (docs/DESIGN.md §5), one upstream line at a time, in the order Codex reads
// them: response.created, response.in_progress, then each output item as
// output_item.added, its deltas and output_item.done, and finally
// response.completed with usage. In search mode one response spans several
// generations ("rounds", §4.5): the model's web_search calls are held back
// and answered between rounds, shown as web_search_call items.
type translator struct {
	sink       *httpx.Stream
	model      string
	reg        *registry
	jsonMode   bool // structured output: the answer is held back and cleaned of fences
	searchMode bool
	responseID string
	createdAt  int64

	seq       int
	started   bool
	done      bool // response.completed / incomplete / failed sent
	nextIndex int
	finished  []finishedItem

	reasoning *openItem
	message   *openItem
	tools     map[int]*streamCall
	// toolOrder are this round's client tool calls, in order.
	toolOrder []*streamCall
	// held are this round's web_search calls (search mode).
	held         []*streamCall
	finishReason string
	usage        *usage
	usageSum     usage
	think        thinktag.Filter
	roundClosed  bool
	jsonBuffer   string
	roundText    string
	roundReason  string
	outputText   string
	failure      *apierr.OpenAI
}

func newTranslator(sink *httpx.Stream, model string, reg *registry, jsonMode, searchMode bool) *translator {
	return &translator{sink: sink, model: model, reg: reg, jsonMode: jsonMode, searchMode: searchMode,
		responseID: ids.New("resp_"), createdAt: time.Now().Unix(), tools: map[int]*streamCall{}}
}

// status is the request's status for the activity log.
func (t *translator) status() int {
	if t.failure != nil {
		return t.failure.Status
	}
	return 200
}

func (t *translator) emit(name string, data map[string]any) {
	data["sequence_number"] = t.seq
	t.seq++
	t.sink.Event(name, data)
}

func (t *translator) response(status string, output []any) map[string]any {
	if output == nil {
		output = []any{}
	}
	return map[string]any{"id": t.responseID, "object": "response", "created_at": t.createdAt,
		"model": t.model, "status": status, "output": output}
}

// start sends response.created and response.in_progress.
func (t *translator) start() {
	if t.started {
		return
	}
	t.started = true
	t.emit("response.created", map[string]any{"response": t.response("in_progress", nil)})
	t.emit("response.in_progress", map[string]any{"response": t.response("in_progress", nil)})
}

// line takes one upstream line.
func (t *translator) line(line string) lineResult {
	if t.done {
		return lineFailed
	}
	payload, ok := strings.CutPrefix(line, "data:")
	if !ok {
		return lineMore
	}
	payload = strings.TrimSpace(payload)
	if payload == "[DONE]" {
		return lineDone
	}
	v, err := jsonx.Parse([]byte(payload))
	if err != nil {
		return lineMore
	}
	chunk := jsonx.Obj(v)
	if chunk == nil {
		return lineMore
	}
	if e, ok := chunk["error"]; ok && e != nil {
		if _, hasChoices := chunk["choices"]; !hasChoices {
			t.fail(apierr.OAIInBand(e))
			return lineFailed
		}
	}
	t.start()
	if u, ok := parseUsage(chunk["usage"]); ok {
		t.usage = &u
	}
	choice := jsonx.Get(chunk, "choices", 0)
	if choice == nil {
		return lineMore
	}
	delta := jsonx.Get(choice, "delta")
	r := jsonx.Str(jsonx.Get(delta, "reasoning_content"))
	if r == "" {
		r = jsonx.Str(jsonx.Get(delta, "reasoning"))
	}
	if r != "" {
		t.reasoningText(r)
	}
	if c := jsonx.Str(jsonx.Get(delta, "content")); c != "" {
		t.segments(t.think.Process(c))
	}
	for _, tc := range jsonx.Arr(jsonx.Get(delta, "tool_calls")) {
		t.toolDelta(tc)
	}
	if f := jsonx.Str(jsonx.Get(choice, "finish_reason")); f != "" {
		t.finishReason = f
	}
	return lineMore
}

func (t *translator) segments(segs []thinktag.Segment) {
	for _, s := range segs {
		if s.Reasoning {
			t.reasoningText(s.Text)
		} else {
			t.contentText(s.Text)
		}
	}
}

// pump feeds the upstream stream to the translator; true when it ended
// complete (a finish reason was delivered).
func (t *translator) pump(resp *upstream.Response) bool {
	lines := resp.Lines()
	for {
		line, ok, err := lines.Next()
		if !ok {
			if err != nil {
				return false
			}
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

// reasoning

func (t *translator) reasoningText(s string) {
	t.closeMessage()
	if t.reasoning == nil {
		t.reasoning = &openItem{id: ids.New("rs_"), index: t.allocate()}
		t.emit("response.output_item.added", map[string]any{
			"item":         map[string]any{"id": t.reasoning.id, "type": "reasoning", "summary": []any{}, "content": []any{}},
			"output_index": t.reasoning.index})
		// Codex's status line shows the summary's latest line while it
		// streams: a fixed heading keeps it at "Thinking"; the reasoning
		// itself joins the summary when the block ends (Codex shows it in
		// the expanded transcript, not the main view).
		t.emit("response.reasoning_summary_part.added", map[string]any{"item_id": t.reasoning.id, "output_index": t.reasoning.index, "summary_index": 0})
		t.emit("response.reasoning_summary_text.delta", map[string]any{"delta": reasoningHeading, "item_id": t.reasoning.id, "output_index": t.reasoning.index, "summary_index": 0})
	}
	r := t.reasoning
	r.text += s
	t.roundReason += s
	// Raw reasoning streams as reasoning_text, which Codex keeps out of view
	// unless show_raw_agent_reasoning is on.
	t.emit("response.reasoning_text.delta", map[string]any{"delta": s, "item_id": r.id, "output_index": r.index, "content_index": 0})
}

func (t *translator) closeReasoning() {
	r := t.reasoning
	if r == nil {
		return
	}
	t.reasoning = nil
	body := strings.TrimSpace(r.text)
	if body != "" {
		t.emit("response.reasoning_summary_text.delta", map[string]any{"delta": "\n\n" + body, "item_id": r.id, "output_index": r.index, "summary_index": 0})
	}
	t.emit("response.reasoning_summary_text.done", map[string]any{"text": reasoningSummary(r.text), "item_id": r.id, "output_index": r.index, "summary_index": 0})
	t.emit("response.reasoning_text.done", map[string]any{"text": r.text, "item_id": r.id, "output_index": r.index, "content_index": 0})
	item := reasoningItem(r.id, r.text)
	t.emit("response.output_item.done", map[string]any{"item": item, "output_index": r.index})
	t.finished = append(t.finished, finishedItem{r.index, item})
}

// text

func (t *translator) contentText(s string) {
	if t.jsonMode {
		t.jsonBuffer += s
		return
	}
	t.emitText(s)
}

func (t *translator) emitText(s string) {
	t.closeReasoning()
	if t.message == nil {
		t.message = &openItem{id: ids.New("msg_"), index: t.allocate()}
		t.emit("response.output_item.added", map[string]any{
			"item":         map[string]any{"id": t.message.id, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}},
			"output_index": t.message.index})
		t.emit("response.content_part.added", map[string]any{
			"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
			"content_index": 0, "output_index": t.message.index, "item_id": t.message.id})
	}
	m := t.message
	m.text += s
	t.roundText += s
	t.outputText += s
	t.emit("response.output_text.delta", map[string]any{"delta": s, "content_index": 0, "output_index": m.index, "item_id": m.id})
}

func (t *translator) closeMessage() {
	m := t.message
	if m == nil {
		return
	}
	t.message = nil
	part := func() map[string]any {
		return map[string]any{"type": "output_text", "text": m.text, "annotations": []any{}}
	}
	t.emit("response.output_text.done", map[string]any{"text": m.text, "content_index": 0, "output_index": m.index, "item_id": m.id})
	t.emit("response.content_part.done", map[string]any{"part": part(), "content_index": 0, "output_index": m.index, "item_id": m.id})
	item := map[string]any{"id": m.id, "type": "message", "role": "assistant", "status": "completed", "content": []any{part()}}
	t.emit("response.output_item.done", map[string]any{"item": item, "output_index": m.index})
	t.finished = append(t.finished, finishedItem{m.index, item})
}

// flushJSON sends structured output held back: cleaned of fences and prose
// when it is the answer, as written when a tool call follows it.
func (t *translator) flushJSON(clean bool) {
	if t.jsonBuffer == "" {
		return
	}
	text := t.jsonBuffer
	if clean {
		text = cleanStructuredOutput(text)
	}
	t.jsonBuffer = ""
	t.emitText(text)
}

// cleanStructuredOutput is the final text of a structured-output request.
// Models without enforced decoding wrap the JSON in Markdown fences or
// prose; if a valid JSON value can be pulled out, exactly that is
// returned, else the text as is.
func cleanStructuredOutput(text string) string {
	valid := func(s string) bool { _, err := jsonx.Parse([]byte(s)); return err == nil }
	trimmed := strings.TrimSpace(text)
	if valid(trimmed) {
		return trimmed
	}
	if body, ok := strings.CutPrefix(trimmed, "```"); ok {
		if nl := strings.IndexByte(body, '\n'); nl >= 0 {
			body = body[nl+1:]
		}
		body = strings.TrimSpace(body)
		body = strings.TrimSuffix(body, "```")
		body = strings.TrimSpace(body)
		if valid(body) {
			return body
		}
	}
	if a, b := strings.IndexByte(trimmed, '{'), strings.LastIndexByte(trimmed, '}'); a >= 0 && a < b {
		if cand := trimmed[a : b+1]; valid(cand) {
			return cand
		}
	}
	return text
}

// tool calls

func (t *translator) allocate() int {
	i := t.nextIndex
	t.nextIndex++
	return i
}

func (t *translator) toolDelta(tc any) {
	index, _ := jsonx.Int(jsonx.Get(tc, "index"))
	id := jsonx.Str(jsonx.Get(tc, "id"))
	name := jsonx.Str(jsonx.Get(tc, "function", "name"))
	args := jsonx.Str(jsonx.Get(tc, "function", "arguments"))
	c := t.tools[index]
	if c == nil || (id != "" && id != c.callID) {
		if c == nil && id == "" && name == "" && args == "" {
			return
		}
		if t.jsonBuffer != "" {
			t.flushJSON(false)
		}
		t.closeReasoning()
		t.closeMessage()
		callID := id
		if callID == "" {
			callID = ids.New("call_")
		}
		held := t.searchMode && name == webSearchFunction
		c = &streamCall{itemID: ids.New("fc_"), callID: callID, name: name, custom: t.reg.isCustom(name), held: held, outputIndex: -1}
		t.tools[index] = c
		if held {
			t.held = append(t.held, c)
		} else {
			c.outputIndex = t.allocate()
			t.toolOrder = append(t.toolOrder, c)
			t.emit("response.output_item.added", map[string]any{"item": t.item(c, true), "output_index": c.outputIndex})
		}
	} else if name != "" && c.name == "" {
		c.name = name
		c.custom = t.reg.isCustom(name)
	}
	if args == "" {
		return
	}
	c.args += args
	// A freeform tool's input is sent whole when the call completes.
	if !c.held && !c.custom {
		t.emit("response.function_call_arguments.delta", map[string]any{"delta": args, "item_id": c.itemID, "output_index": c.outputIndex})
	}
}

func (t *translator) item(c *streamCall, inProgress bool) map[string]any {
	args := ""
	if !inProgress {
		args = c.args
		if args == "" {
			args = "{}"
		}
	}
	return t.reg.outputItem(c.itemID, c.callID, c.name, args, inProgress)
}

func (t *translator) closeCall(c *streamCall) {
	if c.custom {
		input := customInput(c.args)
		t.emit("response.custom_tool_call_input.delta", map[string]any{"delta": input, "item_id": c.itemID, "output_index": c.outputIndex})
		t.emit("response.custom_tool_call_input.done", map[string]any{"input": input, "item_id": c.itemID, "output_index": c.outputIndex})
	} else {
		args := c.args
		if args == "" {
			args = "{}"
		}
		t.emit("response.function_call_arguments.done", map[string]any{"arguments": args, "item_id": c.itemID, "output_index": c.outputIndex})
	}
	complete := t.item(c, false)
	t.emit("response.output_item.done", map[string]any{"item": complete, "output_index": c.outputIndex})
	t.finished = append(t.finished, finishedItem{c.outputIndex, complete})
}

// rounds and the end

// finishRound closes everything the generation left open and adds its
// usage to the response's total.
func (t *translator) finishRound() {
	if t.roundClosed || t.done {
		return
	}
	t.roundClosed = true
	t.start()
	t.segments(t.think.Flush())
	t.flushJSON(len(t.toolOrder) == 0)
	t.closeReasoning()
	t.closeMessage()
	for _, c := range t.toolOrder {
		t.closeCall(c)
	}
	if t.usage != nil {
		t.usageSum.add(*t.usage)
		t.usage = nil
	}
}

// finalize sends the final event: response.completed, or
// response.incomplete when the model hit its output limit, or
// response.failed for a blocked answer.
func (t *translator) finalize() {
	if t.done {
		return
	}
	t.finishRound()
	if t.finishReason == "content_filter" {
		t.fail(apierr.OAIServer(502, "content_filter", "the response was blocked by the backend's content filter"))
		return
	}
	incomplete := t.finishReason == "length"
	status, event := "completed", "response.completed"
	var details any
	if incomplete {
		status, event = "incomplete", "response.incomplete"
		details = map[string]any{"reason": "max_output_tokens"}
	}
	r := t.response(status, t.outputItems())
	r["error"] = nil
	r["incomplete_details"] = details
	r["output_text"] = t.outputText
	r["usage"] = t.usageSum.responses()
	t.emit(event, map[string]any{"response": r})
	t.done = true
}

// fail ends the response with response.failed: only the items that
// completed before the failure are in it.
func (t *translator) fail(e apierr.OpenAI) {
	if t.done {
		return
	}
	t.start()
	t.done = true
	t.failure = &e
	r := t.response("failed", t.outputItems())
	r["error"] = e.Object()
	t.emit("response.failed", map[string]any{"response": r})
}

func (t *translator) outputItems() []any {
	f := append([]finishedItem(nil), t.finished...)
	sort.SliceStable(f, func(i, j int) bool { return f[i].index < f[j].index })
	out := make([]any, len(f))
	for i, x := range f {
		out[i] = x.item
	}
	return out
}

// nextRound readies the translator for the model's next generation
// (search mode).
func (t *translator) nextRound() {
	t.tools = map[int]*streamCall{}
	t.toolOrder, t.held = nil, nil
	t.finishReason, t.usage = "", nil
	t.roundClosed = false
	t.think = thinktag.Filter{}
	t.roundText, t.roundReason = "", ""
}

// webSearch shows the client one search the engine ran, as a
// web_search_call item.
func (t *translator) webSearch(o searchOutcome) {
	t.start()
	index := t.allocate()
	id := ids.New("ws_")
	action := map[string]any{"type": "search", "query": o.query}
	t.emit("response.output_item.added", map[string]any{
		"item": map[string]any{"id": id, "type": "web_search_call", "status": "in_progress", "action": action}, "output_index": index})
	t.emit("response.web_search_call.in_progress", map[string]any{"item_id": id, "output_index": index})
	t.emit("response.web_search_call.searching", map[string]any{"item_id": id, "output_index": index})
	t.emit("response.web_search_call.completed", map[string]any{"item_id": id, "output_index": index})
	complete := webSearchItem(id, o)
	t.emit("response.output_item.done", map[string]any{"item": complete, "output_index": index})
	t.finished = append(t.finished, finishedItem{index, complete})
}
