package app

import (
	"context"
	"strings"
	"sync"
	"time"

	"codelink/internal/config"
	"codelink/internal/core"
	"codelink/internal/routing"
)

// Models are kept in step with the providers: each sync puts a provider's
// listed models into config.Models with what the probe learned, and
// drops the ones it no longer lists unless something still names them.
// The roles follow: the user's default model (else the server's
// recommendation, else the first model) is main; Opus, Haiku, Decision
// and Vision come from the gateway's recommendations.

// Syncable: a provider to keep in step — anything but a Go-LLM-Proxy
// still waiting for its key.
func Syncable(b config.Backend) bool {
	return !(b.Kind == config.GoLLMProxy && strings.TrimSpace(b.APIKey) == "")
}

// referenced: model ids the user chose (the default model, the last
// launches). The roles don't count: they are derived again after a sync.
func referenced(c *config.Config) map[string]bool {
	out := map[string]bool{c.Launch.DefaultModel: true}
	for _, l := range c.Launch.Last {
		out[l.Model], out[l.Heavy], out[l.Background] = true, true, true
	}
	delete(out, "")
	return out
}

// SyncModels puts a reachable provider's listed models into c: new ones
// are added (named by their upstream name, made unique), known ones get
// the probe's context window, logprobs and vision (their id, thinkingOff
// and auto-compact point are the user's and stay), and ones the provider
// no longer lists go unless something names them. The gateway's
// recommendations are kept on the provider. Then the roles are set.
func SyncModels(c *config.Config, id string, p core.Probe) {
	if !p.Reachable {
		return
	}
	listed := map[string]bool{}
	for _, pm := range p.Models {
		listed[pm.Upstream] = true
		if i := findModel(c, Choice{id, pm.Upstream}); i >= 0 {
			m := &c.Models[i]
			if pm.MaxOutput != nil {
				m.MaxOutput = pm.MaxOutput
			}
			if pm.ContextWindow != nil {
				m.ContextWindow = pm.ContextWindow
			}
			if pm.Logprobs != nil {
				m.Logprobs = pm.Logprobs
			}
			if pm.Vision != nil {
				m.Vision = pm.Vision
			}
			continue
		}
		pm := pm
		EnsureModel(c, Choice{id, pm.Upstream}, &pm)
	}
	keep := referenced(c)
	models := c.Models[:0:0]
	for _, m := range c.Models {
		if m.BackendID != id || listed[m.Upstream] || keep[m.ID] {
			models = append(models, m)
		}
	}
	c.Models = models
	for i := range c.Backends {
		if c.Backends[i].ID != id {
			continue
		}
		c.Backends[i].Recommended, c.Backends[i].NativeClassifier = nil, nil
		if g := p.Gateway; g != nil {
			d := g.Defaults
			r := config.Recommended{Main: d.Main, Opus: d.Opus, Small: d.Small, Decision: d.Decision, Vision: d.Vision, Codex: d.Codex}
			if r != (config.Recommended{}) {
				c.Backends[i].Recommended = &r
			}
			c.Backends[i].NativeClassifier = g.NativeClassifier
		}
	}
	ApplyRoles(c)
}

// recommended is the model id a provider recommends for a role, if
// configured.
func recommended(c *config.Config, pick func(config.Recommended) string) string {
	for _, b := range c.Backends {
		if b.Recommended == nil {
			continue
		}
		if up := pick(*b.Recommended); up != "" {
			if i := findModel(c, Choice{b.ID, up}); i >= 0 {
				return c.Models[i].ID
			}
		}
	}
	return ""
}

// Recommendation is the server's recommended main model: the first
// gateway recommendation that is configured, else the first model.
func Recommendation(c *config.Config) string {
	if id := recommended(c, func(r config.Recommended) string { return r.Main }); id != "" {
		return id
	}
	if len(c.Models) > 0 {
		return c.Models[0].ID
	}
	return ""
}

// DefaultModel is the model launches start with: the user's default when
// it exists, else the server's recommendation.
func DefaultModel(c *config.Config) string {
	if _, ok := c.Model(c.Launch.DefaultModel); ok {
		return c.Launch.DefaultModel
	}
	return Recommendation(c)
}

// ApplyRoles sets the engine's roles from the default model and the
// gateway's recommendations (they are used for requests that name no
// configured model, the auto-mode decision and image descriptions).
func ApplyRoles(c *config.Config) {
	c.Roles = config.Roles{
		Main:     DefaultModel(c),
		Opus:     recommended(c, func(r config.Recommended) string { return r.Opus }),
		Small:    recommended(c, func(r config.Recommended) string { return r.Small }),
		Decision: recommended(c, func(r config.Recommended) string { return r.Decision }),
		Vision:   recommended(c, func(r config.Recommended) string { return r.Vision }),
	}
}

// QuickProbe is a probe for keeping models in step: the model list, the
// gateway's capabilities, and logprobs and vision only for models whose
// facts aren't known yet (so opening a launch window stays cheap).
func (a *App) QuickProbe(ctx context.Context, b config.Backend, known *config.Config) core.Probe {
	start := time.Now()
	models, err := core.ListModels(ctx, a.client, b, 15*time.Second)
	if err != nil {
		return core.Probe{Error: err.Error(), Models: []core.ProbedModel{}}
	}
	p := core.Probe{Reachable: true, Models: models, LatencyMS: int(time.Since(start) / time.Millisecond)}
	if p.Models == nil {
		p.Models = []core.ProbedModel{}
	}
	if b.Kind.ChatCompletions() {
		p.Gateway = core.ProbeGateway(ctx, a.client, b, 10*time.Second)
	} else {
		return p
	}
	p.Models = core.OfferedModels(p.Models, p.Gateway)
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i := range p.Models {
		if i >= 16 {
			break
		}
		pm := &p.Models[i]
		if j := findModel(known, Choice{b.ID, pm.Upstream}); j >= 0 && known.Models[j].Logprobs != nil && known.Models[j].Vision != nil {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			t := routing.Target{Model: config.Model{ID: pm.Upstream, BackendID: b.ID, Upstream: pm.Upstream}, Backend: b}
			pm.Logprobs = core.ProbeLogprobs(ctx, a.client, t, 30*time.Second)
			pm.Vision = core.ProbeVision(ctx, a.client, t, 30*time.Second)
		}()
	}
	wg.Wait()
	return p
}

// SyncAll brings every provider's models into the saved settings (app
// start, a launch window opening) and saves and applies them when
// anything changed.
func (a *App) SyncAll(ctx context.Context) {
	cfg := a.Config()
	before := string(mustJSON(cfg))
	for _, b := range cfg.Backends {
		if !Syncable(b) {
			continue
		}
		p := a.QuickProbe(ctx, b, &cfg)
		a.mu.Lock()
		if p.Reachable {
			a.probes[b.ID] = mergeProbe(a.probes[b.ID], p)
		}
		a.mu.Unlock()
		SyncModels(&cfg, b.ID, p)
	}
	if string(mustJSON(cfg)) == before {
		return
	}
	a.mu.Lock()
	cur := a.cfg
	a.mu.Unlock()
	// Only the models, roles and recommendations change; anything saved
	// meanwhile stays.
	cur.Models, cur.Roles = cfg.Models, cfg.Roles
	for i := range cur.Backends {
		if b, ok := cfg.Backend(cur.Backends[i].ID); ok {
			cur.Backends[i].Recommended, cur.Backends[i].NativeClassifier = b.Recommended, b.NativeClassifier
		}
	}
	a.saveSynced(cur)
}

// mergeProbe keeps facts an earlier full probe learned that a quick one
// skipped.
func mergeProbe(old, p core.Probe) core.Probe {
	for i := range p.Models {
		for _, o := range old.Models {
			if o.Upstream == p.Models[i].Upstream {
				if p.Models[i].Logprobs == nil {
					p.Models[i].Logprobs = o.Logprobs
				}
				if p.Models[i].Vision == nil {
					p.Models[i].Vision = o.Vision
				}
			}
		}
	}
	return p
}
