// Package messages serves Anthropic Messages (POST /v1/messages and
// /v1/messages/count_tokens) for Claude Code (docs/DESIGN.md §4): requests
// to an Anthropic backend pass through; everything else is translated to
// Chat Completions and back, streaming or not, with auto-mode classifier
// requests answered by the decision hook and Claude Code's WebSearch run
// as a loop over the configured search provider.
//
// Port of the Swift Translate/Messages (except MediaPreprocessor, which is
// internal/media).
package messages

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"codelink/internal/apierr"
	"codelink/internal/automode"
	"codelink/internal/config"
	"codelink/internal/core"
	"codelink/internal/hooks"
	"codelink/internal/httpx"
	"codelink/internal/jsonx"
	"codelink/internal/media"
	"codelink/internal/routing"
	"codelink/internal/upstream"
)

const route = "/v1/messages"

func sinceMS(t time.Time) int { return int(time.Since(t) / time.Millisecond) }

// Handle serves POST /v1/messages.
func Handle(env *core.Env, w http.ResponseWriter, r *http.Request, body []byte) {
	started := time.Now()
	ctx := r.Context()
	logged := func(status int, model string) {
		env.Log.Record(hooks.Event{Kind: "request", Route: route, Model: model, Status: status, LatencyMS: sinceMS(started)})
	}
	fail := func(e apierr.Anthropic, model string) {
		logged(e.Status, model)
		httpx.WriteAnthropic(w, e)
	}
	req := jsonx.ParseObject(body)
	if req == nil {
		fail(apierr.Invalid("invalid JSON request body"), "")
		return
	}
	requested := jsonx.Str(req["model"])
	if requested == "" {
		fail(apierr.Invalid("missing model field"), "")
		return
	}
	stream := jsonx.Bool(req["stream"])

	// Claude Code's auto-mode classifier: answered by the decision head
	// whatever model it names; a miss runs the same request on the
	// decision model.
	// A flagged model's backend takes it natively over /v1/messages (its
	// prompt prefix is reused); a 4xx rejection falls through to the head.
	if automode.IsClassifier(req) {
		if m, ok := env.Routing.Resolve(requested); ok && m.NativeClassifier {
			if t, ok := env.Routing.Target(m); ok && passthrough(env, w, r, body, "/messages", t, started, true) {
				return
			}
		}
	}

	var forced *config.Model
	if am := env.Hooks.AutoMode; am != nil && am.IsClassifierRequest(req) {
		if answer := am.Respond(ctx, req); answer != nil {
			replyMessage(w, answer, stream)
			logged(200, requested)
			return
		}
		if m, ok := am.DecisionModel(); ok {
			forced = &m
		}
	}

	var model config.Model
	if forced != nil {
		model = *forced
	} else {
		m, ok := env.Routing.Resolve(requested)
		if !ok {
			fail(apierr.NotFound("unknown model: "+requested+" (no main model is configured)"), requested)
			return
		}
		model = m
	}
	target, ok := env.Routing.Target(model)
	if !ok {
		fail(apierr.NotFound("model "+model.ID+" has no configured backend ("+model.BackendID+")"), model.ID)
		return
	}
	if target.IsAnthropic() {
		passthrough(env, w, r, body, "/messages", target, started, false)
		return
	}

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
	processed := media.ProcessMessages(ctx, req, mc)
	if ctx.Err() != nil {
		fail(apierr.API(499, "cancelled"), model.ID)
		return
	}

	ws := findWebSearch(jsonx.Arr(req["tools"]))
	window := env.Facts.ContextWindow(ctx, target)
	c := &call{
		env: env, w: w, ctx: ctx, requested: requested, target: target,
		chatReq:       chatRequest(processed, model, window, ws),
		stopSequences: stringList(req["stop_sequences"]),
		stream:        stream, started: started,
	}
	if ws != nil {
		searchLoop{c: c, tool: ws}.run()
		return
	}
	c.run()
}

// replyMessage sends a complete Anthropic message: as JSON, or as the
// event sequence of a stream.
func replyMessage(w http.ResponseWriter, msg obj, stream bool) {
	if !stream {
		httpx.WriteJSON(w, 200, msg, nil)
		return
	}
	s := httpx.StartStream(w, 200, nil, 0, false)
	emitMessageAsSSE(msg, s)
	s.Close()
}

// call is one translated generation.
type call struct {
	env           *core.Env
	w             http.ResponseWriter
	ctx           context.Context
	requested     string
	target        routing.Target
	chatReq       obj
	stopSequences []string
	stream        bool
	started       time.Time
}

func (c *call) log(status int) {
	c.env.Log.Record(hooks.Event{Kind: "request", Route: route, Model: c.target.Model.ID, Status: status, LatencyMS: sinceMS(c.started)})
}

func (c *call) fail(e apierr.Anthropic) {
	c.log(e.Status)
	if e.Status >= 500 {
		c.env.Log.Record(hooks.Event{Kind: "error", Message: c.target.Model.ID + ": " + e.Message})
	}
	httpx.WriteAnthropic(c.w, e)
}

func (c *call) reply(msg obj) { replyMessage(c.w, msg, c.stream) }

// send sends body and gives the response, or the client error for a
// failure before anything was streamed.
func (c *call) send(body obj, stream bool) (*upstream.Response, *apierr.Anthropic) {
	resp, err := c.env.Client.Send(c.ctx, c.target.ChatRequest(body, stream), c.env.HeaderTimeout())
	if err != nil {
		e := apierr.Transport(err)
		return nil, &e
	}
	if resp.Status >= 400 {
		data, _ := resp.Collect(1 << 20)
		e := apierr.FromUpstream(resp.Status, data, resp.Header, false)
		return nil, &e
	}
	return resp, nil
}

func (c *call) run() {
	resp, e := c.send(c.chatReq, c.stream)
	if e != nil {
		c.fail(*e)
		return
	}
	if !c.stream || !resp.IsEventStream() {
		data, err := resp.Collect(0)
		var chat any
		if err == nil {
			chat, err = jsonx.Parse(data)
		}
		if err != nil {
			c.fail(apierr.API(502, "the backend returned an invalid response"))
			return
		}
		msg := translateMessage(chat, c.requested, c.stopSequences)
		if msg == nil {
			if e, ok := jsonx.Obj(chat)["error"]; ok && e != nil {
				c.fail(inBandError(e))
			} else {
				c.fail(apierr.API(502, "the backend returned no choices"))
			}
			return
		}
		c.log(200)
		c.reply(msg)
		return
	}
	s := httpx.StartStream(c.w, 200, nil, c.env.Tuning.PingInterval, false)
	defer s.Close()
	t := newStreamTranslator(s, c.requested, c.stopSequences, false)
	t.start()
	if pump(resp, t) {
		t.finalize()
	} else if !t.done {
		t.fail(apierr.Truncated)
	}
	c.log(t.status())
}

// passthrough sends the request as it came to an Anthropic backend, only
// the model name rewritten (every other byte as the client sent it). With
// native (a non-Anthropic backend asked for /v1/messages) it gives false,
// having written nothing, when the backend rejects the request with a 4xx
// other than 401, 403 or 429, so the caller can take the ordinary path.
func passthrough(env *core.Env, w http.ResponseWriter, r *http.Request, body []byte, path string, t routing.Target, started time.Time, native bool) bool {
	log := func(status int) {
		env.Log.Record(hooks.Event{Kind: "request", Route: "/v1" + path, Model: t.Model.ID, Status: status, LatencyMS: sinceMS(started)})
	}
	b, err := setTopLevel(body, "model", t.Model.Upstream)
	if err != nil {
		log(400)
		httpx.WriteAnthropic(w, apierr.Invalid("invalid JSON request body"))
		return true
	}
	ureq := t.AnthropicRequest(path, b, r.Header)
	if native {
		ureq = t.NativeMessagesRequest(b, r.Header)
	}
	resp, err := env.Client.Send(r.Context(), ureq, env.HeaderTimeout())
	if err != nil {
		e := apierr.Transport(err)
		log(e.Status)
		httpx.WriteAnthropic(w, e)
		return true
	}
	defer resp.Close()
	if native && resp.Status >= 400 && resp.Status < 500 && !slices.Contains([]int{401, 403, 429}, resp.Status) {
		return false
	}
	if path == "/messages/count_tokens" && slices.Contains([]int{404, 405, 501}, resp.Status) {
		log(200)
		countEstimate(w, body)
		return true
	}
	if resp.Status >= 400 {
		data, _ := resp.Collect(1 << 20)
		e := apierr.FromUpstream(resp.Status, data, resp.Header, true)
		log(e.Status)
		httpx.WriteAnthropic(w, e)
		return true
	}
	h := apierr.RetryHeaders(resp.Header)
	if id := resp.Header.Get("request-id"); id != "" {
		h.Set("request-id", id)
	}
	log(resp.Status)
	if resp.IsEventStream() {
		s := httpx.StartStream(w, resp.Status, h, 0, false)
		defer s.Close()
		buf := make([]byte, 32<<10)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				s.Raw(buf[:n])
			}
			if err != nil || s.Failed() {
				return true
			}
		}
	}
	data, err := resp.Collect(0)
	if err != nil {
		httpx.WriteAnthropic(w, apierr.API(502, "failed to read the backend's response"))
		return true
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	h.Set("Content-Type", ct)
	for k, vs := range h {
		w.Header()[k] = vs
	}
	w.WriteHeader(resp.Status)
	w.Write(data)
	return true
}

var errNotObject = errors.New("not a JSON object")

// setTopLevel replaces (or adds) one top-level key of a JSON object,
// leaving every other byte as it was, so a native backend sees the
// client's request (key order, number spelling) unchanged.
func setTopLevel(body []byte, key string, value any) ([]byte, error) {
	d := json.NewDecoder(bytes.NewReader(body))
	if tok, err := d.Token(); err != nil || tok != json.Delim('{') {
		return nil, errNotObject
	}
	for d.More() {
		tok, err := d.Token()
		if err != nil {
			return nil, err
		}
		afterKey := int(d.InputOffset())
		var raw json.RawMessage
		if err := d.Decode(&raw); err != nil {
			return nil, err
		}
		if k, _ := tok.(string); k != key {
			continue
		}
		end := int(d.InputOffset())
		start := afterKey // past the colon and spaces after the key
		for start < end && bytes.IndexByte([]byte(" \t\r\n:"), body[start]) >= 0 {
			start++
		}
		out := append([]byte(nil), body[:start]...)
		out = append(out, jsonx.Marshal(value)...)
		return append(out, body[end:]...), nil
	}
	o := jsonx.ParseObject(body)
	if o == nil {
		return nil, errNotObject
	}
	o[key] = value
	return jsonx.Marshal(o), nil
}

// CountTokens serves POST /v1/messages/count_tokens: an Anthropic backend
// counts; anything else gets an estimate.
func CountTokens(env *core.Env, w http.ResponseWriter, r *http.Request, body []byte) {
	req := jsonx.ParseObject(body)
	requested := jsonx.Str(req["model"])
	if requested == "" {
		httpx.WriteAnthropic(w, apierr.Invalid("missing model field"))
		return
	}
	model, ok := env.Routing.Resolve(requested)
	if !ok {
		httpx.WriteAnthropic(w, apierr.NotFound("unknown model: "+requested+" (no main model is configured)"))
		return
	}
	if t, ok := env.Routing.Target(model); ok && t.IsAnthropic() {
		passthrough(env, w, r, body, "/messages/count_tokens", t, time.Now(), false)
		return
	}
	countEstimate(w, body)
}

func countEstimate(w http.ResponseWriter, body []byte) {
	httpx.WriteJSON(w, 200, obj{"input_tokens": estimateTokens(body)}, nil)
}
