// Package chat serves POST /v1/chat/completions for every client that
// speaks Chat Completions (opencode, qwen-code, aider, editor plugins,
// scripts; docs/DESIGN.md §5a).
//   - An OpenAI-compatible backend gets the request with its upstream model
//     name, and its answer passes through unchanged, chunk by chunk.
//   - An Anthropic backend gets the request translated to Messages, and the
//     answer is translated back.
//
// Either way documents and images are handled as on the other routes.
// Port of ChatCompletionsHandler.swift.
package chat

import (
	"net/http"
	"strconv"
	"time"

	"codelink/internal/apierr"
	"codelink/internal/core"
	"codelink/internal/hooks"
	"codelink/internal/httpx"
	"codelink/internal/jsonx"
	"codelink/internal/media"
	"codelink/internal/routing"
)

const route = "/v1/chat/completions"

// Handle serves the route.
func Handle(env *core.Env, w http.ResponseWriter, r *http.Request, body []byte) {
	started := time.Now()
	ctx := r.Context()
	fail := func(e apierr.OpenAI, model string) {
		env.Log.Record(hooks.Event{Kind: "request", Route: route, Model: model, Status: e.Status, LatencyMS: ms(started)})
		e.Write(w)
	}
	req := jsonx.ParseObject(body)
	if req == nil {
		fail(apierr.OAIInvalid("invalid JSON request body", ""), "")
		return
	}
	requested := jsonx.Str(req["model"])
	if requested == "" {
		fail(apierr.OAIInvalid("missing model field in request", ""), "")
		return
	}
	messages := jsonx.Arr(req["messages"])
	if len(messages) == 0 {
		fail(apierr.OAIInvalid("messages must be a non-empty array", ""), requested)
		return
	}
	model, ok := env.Routing.Resolve(requested)
	if !ok {
		fail(apierr.OAINotFound("unknown model: "+requested+" (no main model is configured)"), requested)
		return
	}
	target, ok := env.Routing.Target(model)
	if !ok {
		fail(apierr.OAINotFound("model "+model.ID+" has no configured backend ("+model.BackendID+")"), model.ID)
		return
	}

	// Documents and images; images go to the model when it reads them.
	mc := media.Context{Reader: env.Hooks.Documents, ModelHasVision: env.Facts.HasVision(ctx, target, req),
		ModelID: model.ID, Fetcher: env.Client, Log: env.Log}
	if env.Config.Roles.Vision != "" {
		mc.Describer = env.Hooks.Images
	}
	out := make(map[string]any, len(req))
	for k, v := range req {
		out[k] = v
	}
	out["messages"] = media.ProcessChatMessages(ctx, messages, mc)
	if ctx.Err() != nil {
		fail(apierr.OAIServer(499, "cancelled", "cancelled"), model.ID)
		return
	}

	c := &call{env: env, w: w, r: r, requested: requested, target: target, started: started,
		stream: jsonx.Bool(req["stream"]), includeUsage: jsonx.Bool(jsonx.Get(req, "stream_options", "include_usage"))}
	if target.IsAnthropic() {
		c.anthropic(out)
	} else {
		c.forward(out)
	}
}

func ms(since time.Time) int { return int(time.Since(since).Milliseconds()) }

// call is one request on its resolved target.
type call struct {
	env          *core.Env
	w            http.ResponseWriter
	r            *http.Request
	requested    string
	target       routing.Target
	stream       bool
	includeUsage bool
	started      time.Time
}

func (c *call) log(status int) {
	c.env.Log.Record(hooks.Event{Kind: "request", Route: route, Model: c.target.Model.ID, Status: status, LatencyMS: ms(c.started)})
}

func (c *call) logError(message string) {
	c.env.Log.Record(hooks.Event{Kind: "error", Message: c.target.Model.ID + ": " + message})
}

func (c *call) fail(e apierr.OpenAI) {
	c.log(e.Status)
	if e.Status >= 500 {
		c.logError(e.Message)
	}
	e.Write(c.w)
}

// OpenAI-compatible backends.

// forward sends the request as it is, with the upstream model name, and
// answers with the backend's answer as received.
func (c *call) forward(body map[string]any) {
	resp, err := c.env.Client.Send(c.r.Context(), c.target.ChatRequest(body, c.stream), c.env.HeaderTimeout())
	if err != nil {
		c.fail(apierr.OAITransport(err))
		return
	}
	defer resp.Close()
	if resp.Status >= 400 {
		data, _ := resp.Collect(1 << 20)
		c.upstreamError(resp.Status, data, resp.Header)
		return
	}
	c.log(resp.Status)
	if resp.IsEventStream() {
		// The bytes as they come, flushed per read. No keepalives of our
		// own: one could land inside a backend event; the backend's own
		// comments pass through.
		s := httpx.StartStream(c.w, resp.Status, nil, 0, true)
		defer s.Close()
		buf := make([]byte, 32<<10)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				s.Raw(buf[:n])
				if s.Failed() {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	data, err := resp.Collect(0)
	if err != nil {
		c.fail(apierr.OAIServer(502, "upstream_error", "failed to read the backend's response"))
		return
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	c.w.Header().Set("Content-Type", ct)
	if id := resp.Header.Get("X-Request-Id"); id != "" {
		c.w.Header().Set("X-Request-Id", id)
	}
	c.w.WriteHeader(resp.Status)
	c.w.Write(data)
}

// upstreamError answers with a backend's error: its body and status as
// they are. The engine's own credentials failing (401/403) is a gateway
// problem, never the client's, so it is a 502.
func (c *call) upstreamError(status int, body []byte, header http.Header) {
	c.log(status)
	if status >= 500 {
		c.logError("backend returned HTTP " + strconv.Itoa(status))
	}
	if status == 401 || status == 403 {
		apierr.OAIFromUpstream(status, body, header).Write(c.w)
		return
	}
	retry := header.Get("Retry-After")
	if j := jsonx.ParseObject(body); j != nil {
		if _, ok := j["error"]; ok {
			if retry != "" {
				c.w.Header().Set("Retry-After", retry)
			}
			c.w.Header().Set("Content-Type", "application/json")
			c.w.WriteHeader(status)
			c.w.Write(body)
			return
		}
	}
	message := apierr.Scrub(apierr.ParseUpstream(body).Message)
	if message == "" {
		message = "backend returned HTTP " + strconv.Itoa(status)
	}
	typ := "server_error"
	if status < 500 {
		typ = "invalid_request_error"
	}
	apierr.OpenAI{Status: status, Type: typ, Message: message, RetryAfter: retry}.Write(c.w)
}

// Anthropic backends.

func (c *call) anthropic(body map[string]any) {
	msg := toAnthropic(body, c.target.Model.Upstream)
	if msg == nil {
		c.fail(apierr.OAIInvalid("at least one user or assistant message is required", ""))
		return
	}
	h := http.Header{}
	if c.stream {
		h.Set("Accept", "text/event-stream")
	} else {
		h.Set("Accept", "application/json")
	}
	resp, err := c.env.Client.Send(c.r.Context(), c.target.AnthropicRequest("/messages", jsonx.Marshal(msg), h), c.env.HeaderTimeout())
	if err != nil {
		c.fail(apierr.OAITransport(err))
		return
	}
	defer resp.Close()
	if resp.Status >= 400 {
		data, _ := resp.Collect(1 << 20)
		c.fail(apierr.OAIFromAnthropic(apierr.FromUpstream(resp.Status, data, resp.Header, true)))
		return
	}
	if c.stream && resp.IsEventStream() {
		s := httpx.StartStream(c.w, http.StatusOK, nil, c.env.Tuning.PingInterval, true)
		t := newAnthropicStream(s.Raw, c.requested, c.includeUsage)
		lines := resp.Lines()
		for {
			line, ok, _ := lines.Next()
			if !ok || !t.line(line) {
				break
			}
		}
		t.finish()
		s.Close()
		if t.failed {
			c.log(502)
		} else {
			c.log(200)
		}
		return
	}
	data, err := resp.Collect(0)
	var chat map[string]any
	if err == nil {
		if m := jsonx.ParseObject(data); m != nil {
			chat = fromAnthropic(m, c.requested)
		}
	}
	if chat == nil {
		c.fail(apierr.OAIServer(502, "upstream_error", "the backend returned an invalid response"))
		return
	}
	c.log(200)
	if c.stream {
		// Streaming was asked for but a whole message came back.
		s := httpx.StartStream(c.w, http.StatusOK, nil, 0, true)
		replay(chat, s.Raw, c.includeUsage)
		s.Close()
		return
	}
	httpx.WriteJSON(c.w, http.StatusOK, chat, nil)
}
