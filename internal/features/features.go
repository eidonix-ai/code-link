// Package features builds the feature hooks for one configuration
// (docs/DESIGN.md §4.3–§4.5): documents, images (only when the vision role
// names a model), web search (always; unavailable when off or keyless) and
// the auto-mode decision head.
//
// Port of the Swift Features/FeatureHooks.swift.
package features

import (
	"net/http"
	"strings"

	"codelink/internal/automode"
	"codelink/internal/config"
	"codelink/internal/documents"
	"codelink/internal/hooks"
	"codelink/internal/images"
	"codelink/internal/websearch"
)

var _ hooks.Factory = Factory

// Factory is the engine's hooks.Factory.
func Factory(cfg *config.Config, caller hooks.ModelCaller, log hooks.EventLog) hooks.Hooks {
	return build(cfg, caller, log, deps{})
}

// deps are what tests replace.
type deps struct {
	client              *http.Client // nil = http.DefaultClient
	tavilyURL, braveURL string       // "" = the providers' endpoints
}

func build(cfg *config.Config, caller hooks.ModelCaller, log hooks.EventLog, d deps) hooks.Hooks {
	docs, docsKey := cfg.Documents, ""
	if b, ok := cfg.DocumentsVia(); ok {
		docs.PaddleOCR, docsKey = config.GatewayURL(b, config.GatewayDocumentsPath), b.APIKey
	}
	h := hooks.Hooks{
		Documents: documents.New(docs, docsKey, d.client),
		WebSearch: searcher(cfg, d),
	}
	if m, ok := cfg.Model(cfg.Roles.Vision); ok {
		h.Images = images.New(m, caller)
	}
	var decision *config.Model
	if m, ok := DecisionModel(cfg); ok {
		decision = &m
	}
	h.AutoMode = automode.New(decision, caller, log)
	return h
}

func searcher(cfg *config.Config, d deps) hooks.WebSearcher {
	ws := cfg.WebSearch
	if b, ok := cfg.SearchVia(); ok {
		// A go-llm-proxy gateway answers in Tavily's shape with its own keys.
		if u := config.GatewayURL(b, config.GatewaySearchPath); u != "" && b.APIKey != "" {
			return &websearch.Tavily{Key: b.APIKey, Client: d.client, Endpoint: u}
		}
		return websearch.Unavailable{}
	}
	key := strings.TrimSpace(ws.APIKey)
	switch {
	case key == "", ws.Off:
		return websearch.Unavailable{}
	case ws.Provider == config.Tavily:
		return &websearch.Tavily{Key: key, Client: d.client, Endpoint: d.tavilyURL}
	case ws.Provider == config.Brave:
		return &websearch.Brave{Key: key, Client: d.client, Endpoint: d.braveURL}
	}
	return websearch.Unavailable{}
}

// DecisionModel is roles.decision, else the main model unless it is known
// to lack logprobs.
func DecisionModel(cfg *config.Config) (config.Model, bool) {
	if cfg.Roles.Decision != "" {
		return cfg.Model(cfg.Roles.Decision)
	}
	m, ok := cfg.Model(cfg.Roles.Main)
	if !ok || (m.Logprobs != nil && !*m.Logprobs) {
		return config.Model{}, false
	}
	return m, true
}
