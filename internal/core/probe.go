package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"codelink/internal/apierr"
	"codelink/internal/config"
	"codelink/internal/jsonx"
	"codelink/internal/routing"
	"codelink/internal/upstream"
)

// ProbedModel is what a probe learned about one of a backend's models.
type ProbedModel struct {
	Upstream      string `json:"upstream"`
	ContextWindow *int   `json:"contextWindow,omitempty"`
	Logprobs      *bool  `json:"logprobs,omitempty"`
	Vision        *bool  `json:"vision,omitempty"`
}

// Probe is a backend probe's result (Settings → provider → Test).
type Probe struct {
	Reachable bool          `json:"reachable"`
	Error     string        `json:"error,omitempty"`
	Models    []ProbedModel `json:"models"`
	LatencyMS int           `json:"latencyMS,omitempty"`
	// Gateway is what a go-llm-proxy offers besides chat; nil for other
	// backends.
	Gateway *Gateway `json:"gateway,omitempty"`
}

// Gateway is a go-llm-proxy's GET /v1/capabilities: services usable with
// the same key.
type Gateway struct {
	WebSearch bool `json:"webSearch"`
	Documents bool `json:"documents"`
	// Defaults are the gateway's recommended models per role (upstream
	// ids), set by its operator; empty = none.
	Defaults GatewayDefaults `json:"defaults"`
}

// GatewayDefaults are recommended model ids for code-link's roles.
type GatewayDefaults struct {
	Main     string `json:"main,omitempty"`
	Opus     string `json:"opus,omitempty"`
	Small    string `json:"small,omitempty"`
	Decision string `json:"decision,omitempty"`
	Vision   string `json:"vision,omitempty"`
	Codex    string `json:"codex,omitempty"`
}

// ProbeGateway asks a backend for its capabilities; nil when it isn't a
// go-llm-proxy (or doesn't answer).
func ProbeGateway(ctx context.Context, client *upstream.Client, b config.Backend, timeout time.Duration) *Gateway {
	h := http.Header{"Accept": {"application/json"}}
	if b.APIKey != "" {
		h.Set("Authorization", "Bearer "+b.APIKey)
	}
	resp, err := client.Send(ctx, upstream.Request{URL: routing.Target{Backend: b}.URL("/capabilities"), Method: http.MethodGet, Header: h}, timeout)
	if err != nil {
		return nil
	}
	data, err := resp.Collect(64 << 10)
	if err != nil || resp.Status != 200 {
		return nil
	}
	j := jsonx.ParseObject(data)
	if jsonx.Str(j["service"]) != "go-llm-proxy" {
		return nil
	}
	g := &Gateway{WebSearch: jsonx.Str(j["web_search"]) != "", Documents: jsonx.Str(j["documents"]) != ""}
	d := jsonx.Obj(j["defaults"])
	g.Defaults = GatewayDefaults{Main: jsonx.Str(d["main"]), Opus: jsonx.Str(d["opus"]), Small: jsonx.Str(d["small"]),
		Decision: jsonx.Str(d["decision"]), Vision: jsonx.Str(d["vision"]), Codex: jsonx.Str(d["codex"])}
	return g
}

// ListModels is the ids and context windows a backend's /models lists.
func ListModels(ctx context.Context, client *upstream.Client, b config.Backend, timeout time.Duration) ([]ProbedModel, error) {
	t := routing.Target{Backend: b}
	h := http.Header{"Accept": {"application/json"}}
	if b.APIKey != "" {
		if b.Kind == config.Anthropic {
			h.Set("x-api-key", b.APIKey)
			h.Set("anthropic-version", "2023-06-01")
		} else {
			h.Set("Authorization", "Bearer "+b.APIKey)
		}
	}
	resp, err := client.Send(ctx, upstream.Request{URL: t.URL("/models"), Method: http.MethodGet, Header: h}, timeout)
	if err != nil {
		return nil, err
	}
	data, err := resp.Collect(16 << 20)
	if err != nil {
		return nil, err
	}
	if resp.Status < 200 || resp.Status > 299 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.Status, apierr.Scrub(apierr.ParseUpstream(data).Message))
	}
	j, err := jsonx.Parse(data)
	if err != nil {
		return nil, errors.New("the model list is not JSON")
	}
	items := jsonx.Arr(jsonx.Get(j, "data"))
	if items == nil {
		items = jsonx.Arr(jsonx.Get(j, "models"))
	}
	if items == nil {
		items = jsonx.Arr(j)
	}
	var out []ProbedModel
	for _, m := range items {
		id := jsonx.Str(jsonx.Get(m, "id"))
		if id == "" {
			id = jsonx.Str(jsonx.Get(m, "name"))
		}
		if id == "" {
			continue
		}
		p := ProbedModel{Upstream: id}
		for _, path := range [][]any{{"max_model_len"}, {"context_length"}, {"context_window"}, {"max_input_tokens"}, {"meta", "n_ctx_train"}} {
			if n, ok := jsonx.Int(jsonx.Get(m, path...)); ok && n > 0 {
				p.ContextWindow = &n
				break
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// probeImage is an 8×8 red PNG for the vision probe.
const probeImage = "iVBORw0KGgoAAAANSUhEUgAAAAgAAAAICAIAAABLbSncAAAAEUlEQVR42mP4z8CAFTEMLQkAKP8/wc53yE8AAAAASUVORK5CYII="

// ProbeLogprobs: whether a model returns logprobs (a 1-token request). No
// temperature: Flare refuses temperature 0. nil = could not tell.
func ProbeLogprobs(ctx context.Context, client *upstream.Client, t routing.Target, timeout time.Duration) *bool {
	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "Hi"}},
		"max_tokens": 1, "logprobs": true, "top_logprobs": 2, "stream": false}
	resp, err := client.Send(ctx, t.ChatRequest(body, false), timeout)
	if err != nil {
		return nil
	}
	data, err := resp.Collect(4 << 20)
	if err != nil {
		return nil
	}
	f := false
	if resp.Status < 200 || resp.Status > 299 {
		return &f
	}
	j, _ := jsonx.Parse(data)
	first := jsonx.Get(j, "choices", 0, "logprobs", "content", 0)
	v := first != nil && (len(jsonx.Arr(jsonx.Get(first, "top_logprobs"))) > 0 || jsonx.Get(first, "logprob") != nil)
	return &v
}

// ProbeVision: whether a model reads images (a 1-token request with a tiny
// image; text-only vLLM and SGLang answer 400). nil = could not tell.
func ProbeVision(ctx context.Context, client *upstream.Client, t routing.Target, timeout time.Duration) *bool {
	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + probeImage}},
		map[string]any{"type": "text", "text": "Hi"}}}}, "max_tokens": 1, "stream": false}
	resp, err := client.Send(ctx, t.ChatRequest(body, false), timeout)
	if err != nil {
		return nil
	}
	if _, err := resp.Collect(4 << 20); err != nil {
		return nil
	}
	if resp.Status >= 500 || resp.Status == 429 {
		return nil
	}
	v := resp.Status >= 200 && resp.Status <= 299
	return &v
}

// ProbeBackend: reachability, models with context windows, and logprobs
// and vision for the first 16 models, four at a time.
func ProbeBackend(ctx context.Context, client *upstream.Client, b config.Backend) Probe {
	start := time.Now()
	models, err := ListModels(ctx, client, b, 15*time.Second)
	if err != nil {
		return Probe{Reachable: false, Error: apierr.Scrub(upstream.Describe(err)), Models: []ProbedModel{}}
	}
	p := Probe{Reachable: true, Models: models, LatencyMS: int(time.Since(start) / time.Millisecond)}
	if p.Models == nil {
		p.Models = []ProbedModel{}
	}
	if b.Kind == config.GoLLMProxy || b.Kind == config.OpenAICompatible {
		p.Gateway = ProbeGateway(ctx, client, b, 10*time.Second)
	}
	if !b.Kind.ChatCompletions() {
		return p
	}
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i := range min(len(models), 16) {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer func() { <-sem; wg.Done() }()
			t := routing.Target{Model: config.Model{ID: models[i].Upstream, BackendID: b.ID, Upstream: models[i].Upstream}, Backend: b}
			var lp, vis *bool
			var inner sync.WaitGroup
			inner.Add(2)
			go func() { defer inner.Done(); lp = ProbeLogprobs(ctx, client, t, 30*time.Second) }()
			go func() { defer inner.Done(); vis = ProbeVision(ctx, client, t, 30*time.Second) }()
			inner.Wait()
			p.Models[i].Logprobs, p.Models[i].Vision = lp, vis
		}(i)
	}
	wg.Wait()
	return p
}

// Facts are what the engine learns about models while running: context
// windows from /models, and vision (probed on the first request with an
// image) for models configured without one. Shared across Envs. A failed
// lookup is retried after five minutes.
type Facts struct {
	client *upstream.Client
	mu     sync.Mutex
	window map[string]fact[int]
	vision map[string]fact[bool]
}

type fact[T any] struct {
	value *T
	at    time.Time
}

func NewFacts(client *upstream.Client) *Facts {
	return &Facts{client: client, window: map[string]fact[int]{}, vision: map[string]fact[bool]{}}
}

func key(t routing.Target) string {
	return t.Backend.ID + "|" + t.Backend.BaseURL + "|" + t.Model.Upstream
}

// ContextWindow: the model's configured window, else its backend's
// max_model_len; 0 = unknown.
func (f *Facts) ContextWindow(ctx context.Context, t routing.Target) int {
	if t.Model.ContextWindow != nil {
		return *t.Model.ContextWindow
	}
	if !t.Backend.Kind.ChatCompletions() {
		return 0
	}
	k := key(t)
	f.mu.Lock()
	if v, ok := f.window[k]; ok && (v.value != nil || time.Since(v.at) < 5*time.Minute) {
		f.mu.Unlock()
		if v.value == nil {
			return 0
		}
		return *v.value
	}
	f.window[k] = fact[int]{at: time.Now()} // one lookup at a time
	f.mu.Unlock()
	var w *int
	if models, err := ListModels(ctx, f.client, t.Backend, 5*time.Second); err == nil {
		for _, m := range models {
			if m.Upstream == t.Model.Upstream {
				w = m.ContextWindow
			}
		}
	}
	f.mu.Lock()
	f.window[k] = fact[int]{value: w, at: time.Now()}
	f.mu.Unlock()
	if w == nil {
		return 0
	}
	return *w
}

// HasVision: the model's configured vision, true for Anthropic backends,
// else probed once — only when the request carries an image.
func (f *Facts) HasVision(ctx context.Context, t routing.Target, request any) bool {
	if t.Model.Vision != nil {
		return *t.Model.Vision
	}
	if t.IsAnthropic() {
		return true
	}
	if !ContainsImage(request) {
		return false
	}
	k := key(t)
	f.mu.Lock()
	if v, ok := f.vision[k]; ok && (v.value != nil || time.Since(v.at) < 5*time.Minute) {
		f.mu.Unlock()
		return v.value != nil && *v.value
	}
	f.mu.Unlock()
	v := ProbeVision(ctx, f.client, t, 20*time.Second)
	f.mu.Lock()
	f.vision[k] = fact[bool]{value: v, at: time.Now()}
	f.mu.Unlock()
	return v != nil && *v
}

// ContainsImage: an image block in any of the three APIs' shapes.
func ContainsImage(v any) bool {
	switch t := v.(type) {
	case []any:
		for _, x := range t {
			if ContainsImage(x) {
				return true
			}
		}
	case map[string]any:
		switch jsonx.Str(t["type"]) {
		case "image", "image_url", "input_image":
			return true
		}
		for _, x := range t {
			if ContainsImage(x) {
				return true
			}
		}
	}
	return false
}
