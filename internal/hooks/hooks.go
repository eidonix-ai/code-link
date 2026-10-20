// Package hooks is the contract between the engine core (server and
// translation) and the feature modules (docs/DESIGN.md §4.3–§4.5). The core
// calls these; each feature implements one and is tested on its own. A nil
// hook means the feature is off.
package hooks

import (
	"context"
	"errors"

	"codelink/internal/config"
)

// ChatResult is a non-streaming Chat Completions answer.
type ChatResult struct {
	Status int
	// JSON is the decoded body (jsonx values).
	JSON any
}

// ModelCaller makes a non-streaming Chat Completions call on one
// configured model; the core provides it (it knows backends, keys and
// timeouts). body is a Chat Completions request without "model".
type ModelCaller interface {
	Chat(ctx context.Context, model config.Model, body map[string]any) (ChatResult, error)
}

// DocumentInput is a document to read.
type DocumentInput struct {
	Data      []byte
	MediaType string // application/pdf, image/png, …
	Title     string
}

// DocumentOutput is what reading gave.
type DocumentOutput struct {
	Text     string // "" with Error set = could not be read
	Error    string
	Pages    int
	OCRPages int
	Via      string // "paddleocr", "pdf", "ocr", …
}

// DocumentReader turns a document into text for the model (§4.4).
type DocumentReader interface {
	Read(ctx context.Context, doc DocumentInput) DocumentOutput
}

// ImageDescriber describes an image for a model without vision (§4.4);
// "" = it could not.
type ImageDescriber interface {
	Describe(ctx context.Context, image []byte, mediaType string) string
}

// SearchHit is one web search result.
type SearchHit struct {
	Title   string
	URL     string
	Snippet string
	PageAge string
}

var (
	ErrSearchUnavailable     = errors.New("web search unavailable")
	ErrSearchTooManyRequests = errors.New("web search rate limited")
)

// WebSearcher is a web search provider (§4.5). Errors are
// ErrSearchUnavailable or ErrSearchTooManyRequests.
type WebSearcher interface {
	Search(ctx context.Context, query string, allowedDomains, blockedDomains []string) ([]SearchHit, error)
}

// AutoMode answers Claude Code's auto-mode classifier requests (§4.3).
type AutoMode interface {
	// IsClassifierRequest: whether an Anthropic Messages request body is one.
	IsClassifierRequest(request map[string]any) bool
	// Respond gives the complete Anthropic Messages response body
	// (non-streaming shape), or nil to fall back to ordinary translation on
	// the decision model.
	Respond(ctx context.Context, request map[string]any) map[string]any
	// DecisionModel is the model classifier requests run on, for the
	// fallback; ok false when there is none.
	DecisionModel() (config.Model, bool)
}

// Event is one line for the activity log. Never prompt text.
type Event struct {
	Kind string // request, decision, document, websearch, error
	// Request
	Route     string `json:",omitempty"`
	Model     string `json:",omitempty"`
	Status    int    `json:",omitempty"`
	LatencyMS int    `json:",omitempty"`
	// Decision
	Stage    int     `json:",omitempty"`
	PBlock   float64 `json:",omitempty"`
	Severity int     `json:",omitempty"`
	Fallback string  `json:",omitempty"`
	// Document / web search / error
	Name     string `json:",omitempty"`
	Pages    int    `json:",omitempty"`
	OCRPages int    `json:",omitempty"`
	Query    string `json:",omitempty"`
	Results  int    `json:",omitempty"`
	Message  string `json:",omitempty"`
}

// EventLog receives activity events.
type EventLog interface{ Record(Event) }

// NopLog drops events.
type NopLog struct{}

func (NopLog) Record(Event) {}

// Hooks are the feature modules for one configuration.
type Hooks struct {
	Documents DocumentReader
	Images    ImageDescriber
	WebSearch WebSearcher
	AutoMode  AutoMode
}

// Factory builds the hooks for a configuration; the engine calls it on
// start and on every Apply.
type Factory func(cfg *config.Config, caller ModelCaller, log EventLog) Hooks
