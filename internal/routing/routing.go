// Package routing maps the model names clients send to configured models
// and their backends (docs/DESIGN.md §3).
package routing

import (
	"net/http"
	"strings"

	"codelink/internal/config"
	"codelink/internal/jsonx"
	"codelink/internal/upstream"
)

// Target is a model with the backend it runs on.
type Target struct {
	Model   config.Model
	Backend config.Backend
}

func (t Target) IsAnthropic() bool { return t.Backend.Kind == config.Anthropic }

// URL is the backend base URL joined with path ("/chat/completions").
func (t Target) URL(path string) string {
	return strings.TrimRight(t.Backend.BaseURL, "/") + path
}

// ChatRequest is a Chat Completions request on this model; body's "model"
// is replaced by the upstream name.
func (t Target) ChatRequest(body map[string]any, stream bool) upstream.Request {
	b := make(map[string]any, len(body)+1)
	for k, v := range body {
		b[k] = v
	}
	b["model"] = t.Model.Upstream
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	if stream {
		h.Set("Accept", "text/event-stream")
	} else {
		h.Set("Accept", "application/json")
	}
	if t.Backend.APIKey != "" {
		h.Set("Authorization", "Bearer "+t.Backend.APIKey)
	}
	return upstream.Request{URL: t.URL("/chat/completions"), Header: h, Body: jsonx.Marshal(b)}
}

// AnthropicRequest is a native Anthropic request (/messages,
// /messages/count_tokens) with the client's version and beta headers.
func (t Target) AnthropicRequest(path string, body []byte, client http.Header) upstream.Request {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	if a := client.Get("Accept"); a != "" {
		h.Set("Accept", a)
	}
	v := client.Get("anthropic-version")
	if v == "" {
		v = "2023-06-01"
	}
	h.Set("anthropic-version", v)
	if b := client.Get("anthropic-beta"); b != "" {
		h.Set("anthropic-beta", b)
	}
	if t.Backend.APIKey != "" {
		h.Set("x-api-key", t.Backend.APIKey)
	}
	return upstream.Request{URL: t.URL(path), Header: h, Body: body}
}

// Routing resolves models for one configuration.
type Routing struct{ Config *config.Config }

func (r Routing) Model(id string) (config.Model, bool) { return r.Config.Model(id) }

func (r Routing) Main() (config.Model, bool) { return r.Config.Model(r.Config.Roles.Main) }

func (r Routing) role(id string) (config.Model, bool) {
	if m, ok := r.Config.Model(id); ok {
		return m, true
	}
	return r.Main()
}

// Resolve is the model a request names: a configured id (exact, without a
// "[1m]"-style suffix, or case-insensitive), else a Claude model name mapped
// to its role (opus/fable/mythos → opus, haiku → small), else main.
func (r Routing) Resolve(requested string) (config.Model, bool) {
	if m, ok := r.Config.Model(requested); ok {
		return m, true
	}
	name := strings.TrimSpace(requested)
	if i := strings.LastIndex(name, "["); i >= 0 && strings.HasSuffix(name, "]") {
		name = name[:i]
	}
	if m, ok := r.Config.Model(name); ok {
		return m, true
	}
	lower := strings.ToLower(name)
	for _, m := range r.Config.Models {
		if strings.ToLower(m.ID) == lower {
			return m, true
		}
	}
	switch {
	case strings.Contains(lower, "opus"), strings.Contains(lower, "fable"), strings.Contains(lower, "mythos"):
		return r.role(r.Config.Roles.Opus)
	case strings.Contains(lower, "haiku"):
		return r.role(r.Config.Roles.Small)
	}
	return r.Main()
}

// Target is the model with its backend; ok false when the backend is missing.
func (r Routing) Target(m config.Model) (Target, bool) {
	b, ok := r.Config.Backend(m.BackendID)
	if !ok {
		return Target{}, false
	}
	return Target{Model: m, Backend: b}, true
}

// Alias is a role name listed by /v1/models.
type Alias struct {
	Alias string
	Model config.Model
}

// Aliases: sonnet, opus and haiku with the models they run.
func (r Routing) Aliases() []Alias {
	var out []Alias
	if m, ok := r.Main(); ok {
		out = append(out, Alias{"sonnet", m})
	}
	if m, ok := r.role(r.Config.Roles.Opus); ok {
		out = append(out, Alias{"opus", m})
	}
	if m, ok := r.role(r.Config.Roles.Small); ok {
		out = append(out, Alias{"haiku", m})
	}
	return out
}
