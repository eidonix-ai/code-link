// Package core is what every request runs with: a snapshot of the
// configuration, its routing and hooks, the upstream client and the
// per-model facts the engine learns (context windows, vision, logprobs).
// Engine.Apply swaps the Env; a request in flight keeps the one it started
// with.
package core

import (
	"context"
	"fmt"
	"time"

	"codelink/internal/config"
	"codelink/internal/hooks"
	"codelink/internal/jsonx"
	"codelink/internal/routing"
	"codelink/internal/upstream"
)

// Tuning holds timings tests shorten.
type Tuning struct {
	PingInterval time.Duration
	// DefaultHeaderTimeout is the wait for a backend's response headers
	// when the config doesn't set one.
	DefaultHeaderTimeout time.Duration
	MaxRequestBody       int64
}

func DefaultTuning() Tuning {
	return Tuning{PingInterval: 15 * time.Second, DefaultHeaderTimeout: 600 * time.Second, MaxRequestBody: 128 << 20}
}

// Env is one request's world.
type Env struct {
	Config  *config.Config
	Routing routing.Routing
	Hooks   hooks.Hooks
	Caller  *Caller
	Client  *upstream.Client
	Log     hooks.EventLog
	Tuning  Tuning
	Facts   *Facts
}

// NewEnv builds the environment for one configuration. cfg must not be
// modified afterwards.
func NewEnv(cfg *config.Config, client *upstream.Client, log hooks.EventLog, tuning Tuning, facts *Facts, factory hooks.Factory) *Env {
	e := &Env{Config: cfg, Routing: routing.Routing{Config: cfg}, Client: client, Log: log, Tuning: tuning, Facts: facts}
	e.Caller = &Caller{env: e}
	if factory != nil {
		e.Hooks = factory(cfg, e.Caller, log)
	}
	return e
}

// HeaderTimeout is the wait for a backend's response headers.
func (e *Env) HeaderTimeout() time.Duration {
	if s := e.Config.HeaderTimeoutSeconds; s > 0 {
		return time.Duration(s * float64(time.Second))
	}
	return e.Tuning.DefaultHeaderTimeout
}

// Caller is the core's hooks.ModelCaller: one non-streaming Chat
// Completions call on a configured model.
type Caller struct{ env *Env }

func (c *Caller) Chat(ctx context.Context, m config.Model, body map[string]any) (hooks.ChatResult, error) {
	t, ok := c.env.Routing.Target(m)
	if !ok {
		return hooks.ChatResult{}, fmt.Errorf("model %s has no configured backend", m.ID)
	}
	if t.IsAnthropic() {
		return hooks.ChatResult{}, fmt.Errorf("model %s is on an Anthropic backend, not Chat Completions", m.ID)
	}
	b := make(map[string]any, len(body)+1)
	for k, v := range body {
		b[k] = v
	}
	b["stream"] = false
	resp, err := c.env.Client.Send(ctx, t.ChatRequest(b, false), c.env.HeaderTimeout())
	if err != nil {
		return hooks.ChatResult{}, err
	}
	data, err := resp.Collect(0)
	if err != nil {
		return hooks.ChatResult{}, err
	}
	j, err := jsonx.Parse(data)
	if err != nil {
		j = map[string]any{"error": map[string]any{"message": string(data)}}
	}
	return hooks.ChatResult{Status: resp.Status, JSON: j}, nil
}
