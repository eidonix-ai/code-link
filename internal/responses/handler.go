// Package responses serves POST /v1/responses for Codex (docs/DESIGN.md §5):
// the OpenAI Responses API on a Chat Completions backend. Port of the Swift
// Translate/Responses/*.
package responses

import (
	"context"
	"net/http"
	"strings"
	"time"

	"codelink/internal/apierr"
	"codelink/internal/config"
	"codelink/internal/core"
	"codelink/internal/hooks"
	"codelink/internal/httpx"
	"codelink/internal/jsonx"
	"codelink/internal/media"
	"codelink/internal/routing"
	"codelink/internal/upstream"
)

const (
	route = "/v1/responses"
	// ReviewModel is the model Codex's auto-review asks for.
	ReviewModel = "codex-auto-review"
)

// Handle serves the route.
func Handle(env *core.Env, w http.ResponseWriter, r *http.Request, body []byte) {
	started := time.Now()
	ctx := r.Context()
	logged := func(e apierr.OpenAI, model string) {
		env.Log.Record(hooks.Event{Kind: "request", Route: route, Model: model, Status: e.Status, LatencyMS: since(started)})
		e.Write(w)
	}
	req := jsonx.ParseObject(body)
	if req == nil {
		logged(apierr.OAIInvalid("invalid JSON request body", ""), "")
		return
	}
	requested := jsonx.Str(req["model"])
	if requested == "" {
		logged(apierr.OAIInvalid("missing model field in request", ""), "")
		return
	}
	// Server-side conversation state cannot be kept over Chat Completions.
	if id := jsonx.Str(req["previous_response_id"]); id != "" {
		logged(apierr.OAIInvalid("previous_response_id is not supported; resend the full conversation in input",
			"previous_response_id_not_supported"), requested)
		return
	}
	if jsonx.Bool(req["store"]) {
		logged(apierr.OAIInvalid("store=true is not supported; set store to false", "store_not_supported"), requested)
		return
	}
	model, ok := resolve(requested, env)
	if !ok {
		logged(apierr.OAINotFound("unknown model: "+requested+" (no main model is configured)"), requested)
		return
	}
	target, ok := env.Routing.Target(model)
	if !ok {
		logged(apierr.OAINotFound("model "+model.ID+" has no configured backend ("+model.BackendID+")"), model.ID)
		return
	}
	if target.IsAnthropic() {
		logged(apierr.OAIInvalid("the Responses API needs an OpenAI-compatible backend; "+model.ID+" is on an Anthropic backend",
			"unsupported_backend"), model.ID)
		return
	}

	// Documents and images.
	mc := media.Context{
		Reader:         env.Hooks.Documents,
		ModelHasVision: env.Facts.HasVision(ctx, target, req),
		ModelID:        model.ID,
		Fetcher:        env.Client,
		Log:            env.Log,
	}
	if env.Config.Roles.Vision != "" {
		mc.Describer = env.Hooks.Images
	}
	input := req["input"]
	if items, ok := input.([]any); ok {
		input = media.ProcessResponsesInput(ctx, items, mc)
	}
	if ctx.Err() != nil {
		logged(apierr.OAIServer(499, "cancelled", "cancelled"), model.ID)
		return
	}

	translated := translateTools(append(append([]any{}, jsonx.Arr(req["tools"])...), additionalTools(input)...))
	reg := translated.registry
	webSearch := findSearchTool(translated.stripped, reg)
	messages := translateMessages(input, jsonx.Str(req["instructions"]), reg)
	if messages == nil {
		logged(apierr.OAIInvalid("no valid input items", "invalid_prompt"), model.ID)
		return
	}
	tools := translated.tools
	if webSearch != nil {
		tools = append(tools, webSearch.function())
	}
	chatReq := chatRequest(req, messages, tools, reg, model, env.Facts.ContextWindow(ctx, target))
	_, jsonMode := chatReq["response_format"]
	c := &call{requested: requested, target: target, chatReq: chatReq, reg: reg, jsonMode: jsonMode,
		stream: jsonx.Bool(req["stream"]), env: env, w: w, ctx: ctx, started: started}
	if webSearch != nil {
		(&searchLoop{call: c, tool: *webSearch}).run()
		return
	}
	c.run()
}

func since(t time.Time) int { return int(time.Since(t) / time.Millisecond) }

// resolve is the model a request names: a configured id, a tier or Claude
// name as on the Messages route, codex-auto-review → the decision model
// (answering generatively), anything else → roles.codex, else main.
func resolve(requested string, env *core.Env) (config.Model, bool) {
	rt := env.Routing
	roles := env.Config.Roles
	if requested == ReviewModel {
		if env.Hooks.AutoMode != nil {
			if m, ok := env.Hooks.AutoMode.DecisionModel(); ok {
				return m, true
			}
		}
		if m, ok := rt.Model(roles.Decision); ok {
			return m, true
		}
		if m, ok := rt.Model(roles.Codex); ok {
			return m, true
		}
		return rt.Main()
	}
	if m, ok := rt.Model(requested); ok {
		return m, true
	}
	lower := strings.ToLower(requested)
	if lower == "opus" || lower == "sonnet" || lower == "haiku" || strings.HasPrefix(lower, "claude") {
		return rt.Resolve(requested)
	}
	for _, m := range env.Config.Models {
		if strings.ToLower(m.ID) == lower {
			return m, true
		}
	}
	if m, ok := rt.Model(roles.Codex); ok {
		return m, true
	}
	return rt.Main()
}

// call is one translated generation.
type call struct {
	requested string
	target    routing.Target
	chatReq   map[string]any
	reg       *registry
	jsonMode  bool
	stream    bool
	env       *core.Env
	w         http.ResponseWriter
	ctx       context.Context
	started   time.Time
}

func (c *call) log(status int) {
	c.env.Log.Record(hooks.Event{Kind: "request", Route: route, Model: c.target.Model.ID, Status: status, LatencyMS: since(c.started)})
}

// fail answers with an error before anything was streamed.
func (c *call) fail(e apierr.OpenAI) {
	c.log(e.Status)
	if e.Status >= 500 {
		c.env.Log.Record(hooks.Event{Kind: "error", Message: c.target.Model.ID + ": " + e.Message})
	}
	e.Write(c.w)
}

// send sends body and returns the response, or the client error for a
// failure before anything was streamed.
func (c *call) send(body map[string]any, stream bool) (*upstream.Response, *apierr.OpenAI) {
	resp, err := c.env.Client.Send(c.ctx, c.target.ChatRequest(body, stream), c.env.HeaderTimeout())
	if err != nil {
		e := apierr.OAITransport(err)
		return nil, &e
	}
	if resp.Status >= 400 {
		data, _ := resp.Collect(1 << 20)
		e := apierr.OAIFromUpstream(resp.Status, data, resp.Header)
		return nil, &e
	}
	return resp, nil
}

func (c *call) translator(s *httpx.Stream, searchMode bool) *translator {
	return newTranslator(s, c.requested, c.reg, c.jsonMode, searchMode)
}

func (c *call) startStream(pings bool) *httpx.Stream {
	interval := c.env.Tuning.PingInterval
	if !pings {
		interval = 0
	}
	return httpx.StartStream(c.w, 200, nil, interval, true)
}

func (c *call) run() {
	resp, e := c.send(c.chatReq, c.stream)
	if e != nil {
		c.fail(*e)
		return
	}
	defer resp.Close()
	if c.stream && resp.IsEventStream() {
		s := c.startStream(true)
		defer s.Close()
		t := c.translator(s, false)
		t.start()
		if t.pump(resp) {
			t.finalize()
		} else if !t.done {
			t.fail(apierr.OAITruncated)
		}
		c.log(t.status())
		return
	}
	data, err := resp.Collect(0)
	var chat any
	if err == nil {
		chat, err = jsonx.Parse(data)
	}
	if err != nil {
		c.fail(apierr.OAIServer(502, "invalid_upstream_response", "the backend returned an invalid response"))
		return
	}
	if c.stream {
		// The backend answered a streaming request with a plain body.
		line, ok := chunkLine(chat)
		if !ok {
			c.fail(blockingError(chat))
			return
		}
		c.log(200)
		s := c.startStream(false)
		defer s.Close()
		t := c.translator(s, false)
		t.start()
		t.line(line)
		t.finalize()
		return
	}
	rd, ok := roundOf(chat, c.reg, c.jsonMode, false)
	if !ok {
		c.fail(blockingError(chat))
		return
	}
	c.log(200)
	httpx.WriteJSON(c.w, 200, responseObject(c.requested, rd.items, rd.text, rd.finishReason, rd.usage), nil)
}

func blockingError(chat any) apierr.OpenAI {
	if e := jsonx.Get(chat, "error"); e != nil {
		return apierr.OAIInBand(e)
	}
	return apierr.OAIServer(502, "invalid_upstream_response", "the backend returned no choices")
}

// searchLoop is Codex's web_search tool on a chat model (docs/DESIGN.md
// §4.5): the same loop as the Messages route's, with Responses events.
type searchLoop struct {
	call *call
	tool webSearchTool
}

func (l *searchLoop) maxRounds() int { return l.tool.maxUses + 2 }

func (l *searchLoop) request(round int, messages []any) map[string]any {
	return searchRoundRequest(l.call.chatReq, round, l.maxRounds(), messages)
}

func (l *searchLoop) runner() *searchRunner {
	return &searchRunner{tool: l.tool, searcher: l.call.env.Hooks.WebSearch, log: l.call.env.Log}
}

func (l *searchLoop) run() {
	c := l.call
	first, e := c.send(l.request(0, jsonx.Arr(c.chatReq["messages"])), c.stream)
	if e != nil {
		c.fail(*e)
		return
	}
	if c.stream {
		if !first.IsEventStream() {
			first.Close()
			c.fail(apierr.OAIServer(502, "invalid_upstream_response", "the backend did not stream its answer"))
			return
		}
		l.streamLoop(first)
		return
	}
	j, fe := l.blocking(first)
	if fe != nil {
		c.fail(*fe)
		return
	}
	c.log(200)
	httpx.WriteJSON(c.w, 200, j, nil)
}

// continuation is the assistant message and tool results that carry a
// round's searches back to the model.
func continuation(text, reasoning string, searches []searchCall, outcomes []searchOutcome) []any {
	var content any
	if text != "" {
		content = text
	}
	assistant := map[string]any{"role": "assistant", "content": content}
	if reasoning != "" {
		assistant["reasoning_content"] = reasoning
		assistant["reasoning"] = reasoning
	}
	var calls []any
	for _, s := range searches {
		calls = append(calls, functionCall(s.id, webSearchFunction, searchArgs(s.args)))
	}
	assistant["tool_calls"] = calls
	out := []any{assistant}
	for _, o := range outcomes {
		out = append(out, map[string]any{"role": "tool", "tool_call_id": o.callID, "content": o.modelText()})
	}
	return out
}

func (l *searchLoop) streamLoop(first *upstream.Response) {
	c := l.call
	s := c.startStream(true)
	defer s.Close()
	runner := l.runner()
	t := c.translator(s, true)
	t.start()
	messages := jsonx.Arr(c.chatReq["messages"])
	resp := first
	defer func() { c.log(t.status()) }()
	for round := range l.maxRounds() {
		if round > 0 {
			t.nextRound()
			r, e := c.send(l.request(round, messages), true)
			if e != nil {
				t.fail(*e)
				return
			}
			resp = r
		}
		complete := t.pump(resp)
		resp.Close()
		if t.done {
			return
		}
		if !complete {
			t.fail(apierr.OAITruncated)
			return
		}
		t.finishRound()
		if len(t.held) == 0 {
			t.finalize()
			return
		}

		s.Ping() // the searches take a moment
		var searches []searchCall
		for _, h := range t.held {
			searches = append(searches, searchCall{h.callID, h.args})
		}
		outcomes := runner.answer(c.ctx, searches)
		if c.ctx.Err() != nil {
			return
		}
		for _, o := range outcomes {
			t.webSearch(o)
		}
		if len(t.toolOrder) > 0 || round >= l.maxRounds()-1 {
			// The client answers its own tool calls; its next request
			// carries the searches in history.
			t.finalize()
			return
		}
		messages = append(messages, continuation(t.roundText, t.roundReason, searches, outcomes)...)
	}
	t.finalize()
}

func (l *searchLoop) blocking(first *upstream.Response) (map[string]any, *apierr.OpenAI) {
	c := l.call
	runner := l.runner()
	messages := jsonx.Arr(c.chatReq["messages"])
	var items []any
	var text, finish string
	var total usage
	resp := first
	for round := range l.maxRounds() {
		if round > 0 {
			r, e := c.send(l.request(round, messages), false)
			if e != nil {
				return nil, e
			}
			resp = r
		}
		data, err := resp.Collect(0)
		var chat any
		if err == nil {
			chat, err = jsonx.Parse(data)
		}
		if err != nil {
			e := apierr.OAIServer(502, "invalid_upstream_response", "the backend returned an invalid response")
			return nil, &e
		}
		rd, ok := roundOf(chat, c.reg, c.jsonMode, true)
		if !ok {
			e := blockingError(chat)
			return nil, &e
		}
		total.add(rd.usage)
		finish = rd.finishReason
		items = append(items, rd.items...)
		text += rd.text
		if len(rd.searchCalls) == 0 {
			break
		}
		outcomes := runner.answer(c.ctx, rd.searchCalls)
		for _, o := range outcomes {
			items = append(items, webSearchItem(newID("ws_"), o))
		}
		clientCalls := false
		for _, it := range rd.items {
			if ty := jsonx.Type(it); ty == "function_call" || ty == "custom_tool_call" {
				clientCalls = true
			}
		}
		if clientCalls || round >= l.maxRounds()-1 || c.ctx.Err() != nil {
			break
		}
		assistant := map[string]any{}
		for k, v := range jsonx.Obj(jsonx.Get(chat, "choices", 0, "message")) {
			assistant[k] = v
		}
		assistant["role"] = "assistant"
		if _, ok := assistant["content"]; !ok {
			assistant["content"] = nil
		}
		messages = append(messages, assistant)
		for _, o := range outcomes {
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": o.callID, "content": o.modelText()})
		}
	}
	return responseObject(c.requested, items, text, finish, total), nil
}
