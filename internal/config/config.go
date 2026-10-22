// Package config is code-link's one settings file: providers (with their
// keys), models, roles, documents, web search, launch settings and the
// engine's access token. See docs/DESIGN.md §6.
package config

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// DefaultPort is the engine's port (the Swift app uses 18765, so both can run).
const DefaultPort = 18775

// FileName is the settings file's name, in the user's config folder or next
// to the program (portable mode).
const FileName = "code-link.json"

type BackendKind string

const (
	// OpenAICompatible is vLLM, SGLang, Flare: Chat Completions with SSE.
	OpenAICompatible BackendKind = "openai"
	// Anthropic is a native Messages endpoint: requests pass through.
	Anthropic BackendKind = "anthropic"
	// GoLLMProxy is a go-llm-proxy gateway (e.g. llm.eidonix.com): Chat
	// Completions like OpenAICompatible, and with the same key its web
	// search and document reading, which code-link uses unless set up
	// otherwise.
	GoLLMProxy BackendKind = "gollm"
)

// GoLLMProxyURL is the default address of a Go-LLM-Proxy provider.
const GoLLMProxyURL = "https://llm.eidonix.com/v1"

// ChatCompletions: the backend speaks OpenAI Chat Completions.
func (k BackendKind) ChatCompletions() bool { return k == OpenAICompatible || k == GoLLMProxy }

// Backend is a provider.
type Backend struct {
	ID      string      `json:"id"`
	Kind    BackendKind `json:"kind"`
	BaseURL string      `json:"baseURL"`
	APIKey  string      `json:"apiKey,omitempty"`
	// Recommended are the models a gateway's operator recommends per role
	// (upstream names, from its /v1/capabilities), kept from the last sync.
	Recommended *Recommended `json:"recommended,omitempty"`
	// NativeClassifier are the upstream names of the models a gateway's
	// operator has set up to take Claude Code's classifier requests natively
	// (from its /v1/capabilities), kept from the last sync.
	NativeClassifier []string `json:"nativeClassifier,omitempty"`
}

// Recommended are upstream model names per role.
type Recommended struct {
	Main     string `json:"main,omitempty"`
	Opus     string `json:"opus,omitempty"`
	Small    string `json:"small,omitempty"`
	Decision string `json:"decision,omitempty"`
	Vision   string `json:"vision,omitempty"`
	Codex    string `json:"codex,omitempty"`
}

// Model is a model on a backend.
type Model struct {
	// ID is the name Claude Code and Codex see.
	ID        string `json:"id"`
	BackendID string `json:"backend"`
	// Upstream is the name the backend expects.
	Upstream string `json:"upstream"`
	// ContextWindow nil = detect from the backend's /models (max_model_len).
	ContextWindow *int `json:"contextWindow,omitempty"`
	// AutoCompactAt nil = 85% of the context window.
	AutoCompactAt *int `json:"autoCompactAt,omitempty"`
	// Vision nil = probe on the first image.
	Vision *bool `json:"vision,omitempty"`
	// Logprobs nil = probe once.
	Logprobs *bool `json:"logprobs,omitempty"`
	// NativeClassifier sends Claude Code's auto-mode classifier requests to
	// the backend's /v1/messages as they are (a Messages-capable backend that
	// reuses the prompt prefix), instead of the decision head or translation.
	// nil = what the gateway says (Backend.NativeClassifier), else off.
	NativeClassifier *bool `json:"nativeClassifier,omitempty"`
	// ThinkingOff is merged into a Chat Completions request when a client
	// turns thinking off (e.g. {"reasoning_effort":"low"}).
	ThinkingOff map[string]any `json:"thinkingOff,omitempty"`
}

// NativeClassifierOn is whether classifier requests for m go to its backend
// b natively: the user's choice, else the gateway's.
func (m Model) NativeClassifierOn(b Backend) bool {
	if m.NativeClassifier != nil {
		return *m.NativeClassifier
	}
	return slices.Contains(b.NativeClassifier, m.Upstream)
}

// EffectiveAutoCompactAt is where Claude Code should compact.
func (m Model) EffectiveAutoCompactAt() int {
	if m.AutoCompactAt != nil {
		return *m.AutoCompactAt
	}
	if m.ContextWindow != nil {
		return *m.ContextWindow * 85 / 100
	}
	return 0
}

// Roles are model ids for each role; empty = the default described.
type Roles struct {
	// Main is Claude Code's default (the Sonnet slot) and ANTHROPIC_MODEL.
	Main string `json:"main"`
	// Opus also serves Claude Code's Fable tier. Empty = main.
	Opus string `json:"opus,omitempty"`
	// Small is the Haiku slot and background tasks. Empty = main.
	Small string `json:"small,omitempty"`
	// Decision answers auto-mode classifier requests; needs logprobs.
	// Empty = main when it has them.
	Decision string `json:"decision,omitempty"`
	// Vision describes images for a model without vision. Empty = none.
	Vision string `json:"vision,omitempty"`
	// Codex is Codex's model. Empty = main.
	Codex string `json:"codex,omitempty"`
}

type LocalExtraction string

const (
	// LocalOCR reads the PDF text layer and OCRs pages without one where
	// the platform has OCR.
	LocalOCR      LocalExtraction = "ocr"
	LocalTextOnly LocalExtraction = "text"
	LocalOff      LocalExtraction = "off"
)

type Documents struct {
	// Via is a provider (a go-llm-proxy gateway) whose document reading is
	// used, with that provider's key; it overrides PaddleOCR.
	Via string `json:"via,omitempty"`
	// PaddleOCR is a PaddleOCR(-VL) /layout-parsing endpoint, tried first.
	PaddleOCR string `json:"paddleOCR,omitempty"`
	// Local is used when PaddleOCR isn't set, is unreachable or fails.
	Local LocalExtraction `json:"local,omitempty"`
	// LocalOnly: never through a gateway, even when one has a key.
	LocalOnly bool `json:"localOnly,omitempty"`
}

type SearchProvider string

const (
	Tavily SearchProvider = "tavily"
	Brave  SearchProvider = "brave"
)

type WebSearch struct {
	// Via is a provider (a go-llm-proxy gateway) whose web search is used,
	// with that provider's key; it overrides Provider.
	Via string `json:"via,omitempty"`
	// Provider empty = off (searches answer "unavailable").
	Provider SearchProvider `json:"provider,omitempty"`
	APIKey   string         `json:"apiKey,omitempty"`
	// Off: no web search, even when a gateway has a key.
	Off bool `json:"off,omitempty"`
}

type Launch struct {
	// Terminal: "iterm" / "terminal" on macOS, "wt" / "powershell" on
	// Windows; empty = the platform's best available.
	Terminal string `json:"terminal,omitempty"`
	// PermissionMode for Claude Code; empty = auto.
	PermissionMode string   `json:"permissionMode,omitempty"`
	OpenAtLogin    bool     `json:"openAtLogin,omitempty"`
	RecentFolders  []string `json:"recentFolders,omitempty"`
	// DefaultModel is the user's default model id; "" = the server's
	// recommendation.
	DefaultModel string `json:"defaultModel,omitempty"`
	// Last are the choices of the last launch per tool ("claude", "codex").
	Last map[string]LaunchChoice `json:"last,omitempty"`
}

// LaunchChoice is what a launch window chose.
type LaunchChoice struct {
	Model string `json:"model,omitempty"`
	// Heavy (Opus / Fable) and Background (Haiku) are Claude Code's other
	// tiers; "" = the same as Model.
	Heavy      string `json:"heavy,omitempty"`
	Background string `json:"background,omitempty"`
	Folder     string `json:"folder,omitempty"`
}

type Config struct {
	Port int `json:"port"`
	// AccessToken is the only credential sessions get; generated once.
	AccessToken string    `json:"accessToken"`
	Backends    []Backend `json:"backends"`
	Models      []Model   `json:"models"`
	Roles       Roles     `json:"roles"`
	Documents   Documents `json:"documents"`
	WebSearch   WebSearch `json:"webSearch"`
	Launch      Launch    `json:"launch"`
	// HeaderTimeoutSeconds is how long to wait for a backend's response
	// headers (for a non-streaming request, the whole answer). 0 = 600 s.
	HeaderTimeoutSeconds float64 `json:"headerTimeoutSeconds,omitempty"`
}

// Default is a fresh configuration.
// DefaultGatewayID is the built-in Go-LLM-Proxy provider's id.
const DefaultGatewayID = "llm.eidonix.com"

// Default is a fresh configuration: the built-in Go-LLM-Proxy provider
// (llm.eidonix.com), waiting for the user's key.
func Default() Config {
	return Config{Port: DefaultPort,
		Backends:  []Backend{{ID: DefaultGatewayID, Kind: GoLLMProxy, BaseURL: GoLLMProxyURL}},
		Models:    []Model{},
		Documents: Documents{Local: LocalOCR}}
}

// Model is the configured model with this id.
func (c *Config) Model(id string) (Model, bool) {
	if id == "" {
		return Model{}, false
	}
	for _, m := range c.Models {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}

// Backend is the configured backend with this id.
func (c *Config) Backend(id string) (Backend, bool) {
	for _, b := range c.Backends {
		if b.ID == id {
			return b, true
		}
	}
	return Backend{}, false
}

// Gateway paths, from a go-llm-proxy's root (its GET /v1/capabilities
// says which it offers).
const (
	GatewaySearchPath    = "/v1/search"
	GatewayDocumentsPath = "/layout-parsing"
)

// GatewayURL is path on the root (scheme://host) of a provider's base URL.
func GatewayURL(b Backend, path string) string {
	u, err := url.Parse(strings.TrimSpace(b.BaseURL))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host + path
}

// Gateway is the first Go-LLM-Proxy provider with a key.
func (c *Config) Gateway() (Backend, bool) {
	for _, b := range c.Backends {
		if b.Kind == GoLLMProxy && strings.TrimSpace(b.APIKey) != "" {
			return b, true
		}
	}
	return Backend{}, false
}

// SearchVia is the provider web search goes through: Via, else a
// Go-LLM-Proxy when no search provider of the user's own is set; none
// when web search is Off.
func (c *Config) SearchVia() (Backend, bool) {
	if c.WebSearch.Off {
		return Backend{}, false
	}
	if c.WebSearch.Via != "" {
		return c.Backend(c.WebSearch.Via)
	}
	if c.WebSearch.Provider == "" {
		return c.Gateway()
	}
	return Backend{}, false
}

// DocumentsVia is the provider documents go through: Via, else a
// Go-LLM-Proxy when no PaddleOCR of the user's own is set; none when
// LocalOnly.
func (c *Config) DocumentsVia() (Backend, bool) {
	if c.Documents.LocalOnly {
		return Backend{}, false
	}
	if c.Documents.Via != "" {
		return c.Backend(c.Documents.Via)
	}
	if strings.TrimSpace(c.Documents.PaddleOCR) == "" {
		return c.Gateway()
	}
	return Backend{}, false
}

// Configured: a main model on a known backend.
func (c *Config) Configured() bool {
	m, ok := c.Model(c.Roles.Main)
	if !ok {
		return false
	}
	_, ok = c.Backend(m.BackendID)
	return ok
}

// normalize fills defaults after loading.
func (c *Config) normalize() {
	if c.Port == 0 {
		c.Port = DefaultPort
	}
	if c.Backends == nil {
		c.Backends = []Backend{}
	}
	if c.Models == nil {
		c.Models = []Model{}
	}
	if c.Documents.Local == "" {
		c.Documents.Local = LocalOCR
	}
	for i := range c.Backends {
		if c.Backends[i].Kind == "" {
			c.Backends[i].Kind = OpenAICompatible
		}
	}
}

// NewAccessToken is 32 random bytes, base64url.
func NewAccessToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Path is the settings file: next to the program when a code-link.json is
// there (portable mode), else in the user's config folder
// (~/Library/Application Support/Eidonix/code-link on macOS,
// %AppData%\Eidonix\code-link on Windows).
func Path() (string, error) {
	if exe, err := os.Executable(); err == nil {
		if exe, err = filepath.EvalSymlinks(exe); err == nil {
			p := filepath.Join(filepath.Dir(exe), FileName)
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		}
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "Eidonix", "code-link", FileName), nil
}

// Load reads the file at path; a missing file gives the defaults. A new
// access token is generated (and saved) when there is none.
func Load(path string) (Config, error) {
	c := Default()
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return c, err
	default:
		if err := json.Unmarshal(data, &c); err != nil {
			return Default(), fmt.Errorf("%s: %w", path, err)
		}
	}
	c.normalize()
	if strings.TrimSpace(c.AccessToken) == "" {
		c.AccessToken = NewAccessToken()
		if err := Save(path, c); err != nil {
			return c, err
		}
	}
	return c, nil
}

// Save writes the file atomically, readable only by the user.
func Save(path string, c Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".code-link-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil && !isWindows() {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
