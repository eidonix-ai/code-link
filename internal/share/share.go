// Package share writes and reads .codelink files: a setup someone can hand
// to a colleague — providers, models, roles, documents and web search —
// never API keys, the access token, the port or launch settings.
package share

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"codelink/internal/config"
)

// Format and Version identify the file.
const (
	Format    = "code-link-settings"
	Version   = 1
	Extension = ".codelink"
)

// Provider is a provider without its key.
type Provider struct {
	ID      string             `json:"id"`
	Kind    config.BackendKind `json:"kind"`
	BaseURL string             `json:"baseURL"`
}

// Documents is how documents are read (no keys).
type Documents struct {
	Via       string                 `json:"via,omitempty"`
	PaddleOCR string                 `json:"paddleOCR,omitempty"`
	Local     config.LocalExtraction `json:"local,omitempty"`
	LocalOnly bool                   `json:"localOnly,omitempty"`
}

// WebSearch is how web search runs (no key).
type WebSearch struct {
	Via      string                `json:"via,omitempty"`
	Provider config.SearchProvider `json:"provider,omitempty"`
	Off      bool                  `json:"off,omitempty"`
}

// File is a .codelink file.
type File struct {
	Format    string         `json:"format"`
	Version   int            `json:"version"`
	Providers []Provider     `json:"providers"`
	Models    []config.Model `json:"models"`
	Roles     config.Roles   `json:"roles"`
	Documents Documents      `json:"documents"`
	WebSearch WebSearch      `json:"webSearch"`
}

// Export is a configuration as a shareable file.
func Export(c config.Config) File {
	f := File{Format: Format, Version: Version, Providers: []Provider{}, Models: []config.Model{}, Roles: c.Roles,
		Documents: Documents{Via: c.Documents.Via, PaddleOCR: c.Documents.PaddleOCR, Local: c.Documents.Local, LocalOnly: c.Documents.LocalOnly},
		WebSearch: WebSearch{Via: c.WebSearch.Via, Provider: c.WebSearch.Provider, Off: c.WebSearch.Off}}
	for _, b := range c.Backends {
		f.Providers = append(f.Providers, Provider{ID: b.ID, Kind: b.Kind, BaseURL: b.BaseURL})
	}
	f.Models = append(f.Models, c.Models...)
	return f
}

// Marshal is the file's bytes (indented JSON).
func Marshal(f File) []byte {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		panic(err)
	}
	return append(b, '\n')
}

// ErrNotSettings: the file isn't a code-link settings file.
var ErrNotSettings = errors.New("this isn't a code-link settings file")

// Parse reads and checks a file.
func Parse(data []byte) (File, error) {
	var f File
	if err := json.Unmarshal(data, &f); err != nil || f.Format != Format {
		return File{}, ErrNotSettings
	}
	if f.Version > Version {
		return File{}, fmt.Errorf("this settings file is from a newer code-link (version %d); update code-link first", f.Version)
	}
	ids := map[string]bool{}
	for i, p := range f.Providers {
		switch p.Kind {
		case "":
			f.Providers[i].Kind = config.OpenAICompatible
		case config.OpenAICompatible, config.Anthropic, config.GoLLMProxy:
		default:
			return File{}, fmt.Errorf("provider %q has an unknown kind %q", p.ID, p.Kind)
		}
		if strings.TrimSpace(p.ID) == "" || ids[p.ID] {
			return File{}, errors.New("the providers need unique names")
		}
		ids[p.ID] = true
		if u, err := url.Parse(p.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return File{}, fmt.Errorf("provider %q has no usable URL", p.ID)
		}
	}
	models := map[string]bool{}
	for _, m := range f.Models {
		if m.ID == "" || models[m.ID] || !ids[m.BackendID] || m.Upstream == "" {
			return File{}, fmt.Errorf("model %q doesn't fit the providers in the file", m.ID)
		}
		models[m.ID] = true
	}
	return f, nil
}

// Preview is what an import would bring, in words.
type Preview struct {
	Providers []string `json:"providers"`
	Models    []string `json:"models"`
	Roles     []string `json:"roles"`
}

// KindLabel names a provider kind.
func KindLabel(k config.BackendKind) string {
	switch k {
	case config.GoLLMProxy:
		return "Go-LLM-Proxy"
	case config.Anthropic:
		return "Anthropic"
	}
	return "OpenAI-compatible"
}

// Summarize words a file for the import preview.
func Summarize(f File) Preview {
	p := Preview{Providers: []string{}, Models: []string{}, Roles: []string{}}
	for _, b := range f.Providers {
		p.Providers = append(p.Providers, b.ID+" ("+KindLabel(b.Kind)+")")
	}
	for _, m := range f.Models {
		p.Models = append(p.Models, m.ID+" on "+m.BackendID)
	}
	r := f.Roles
	for _, x := range [][2]string{{"Main", r.Main}, {"Opus", r.Opus}, {"Haiku", r.Small}, {"Decision", r.Decision}, {"Vision", r.Vision}, {"Codex", r.Codex}} {
		if x[1] != "" {
			p.Roles = append(p.Roles, x[0]+": "+x[1])
		}
	}
	return p
}

// Result is what a merge did.
type Result struct {
	// Providers maps the file's provider ids to the ids in the merged config.
	Providers map[string]string `json:"providers"`
	// Models maps the file's model ids likewise.
	Models map[string]string `json:"models"`
	// NeedKey are the merged provider ids without a key yet, in file order.
	NeedKey []string `json:"needKey"`
}

func sameServer(a, b string) bool {
	return strings.TrimRight(strings.TrimSpace(a), "/") == strings.TrimRight(strings.TrimSpace(b), "/")
}

func unique(base string, taken func(string) bool) string {
	if !taken(base) {
		return base
	}
	for n := 2; ; n++ {
		if id := base + " " + strconv.Itoa(n); !taken(id) {
			return id
		}
	}
}

// Merge adds a file to a configuration. A provider already there (same
// kind and URL) is reused with its key; a new one whose name is taken is
// renamed, and the rename follows into its models, the roles and the
// documents and web search "via". A model already there (same provider and
// upstream name) is reused; a new one whose name is taken is renamed. When
// the file names a main model, its roles replace the current ones; its
// documents and web search choices replace the current ones. Keys, the
// token, the port and launch settings are untouched.
func Merge(c config.Config, f File) (config.Config, Result) {
	res := Result{Providers: map[string]string{}, Models: map[string]string{}, NeedKey: []string{}}
	c.Backends = append([]config.Backend{}, c.Backends...)
	c.Models = append([]config.Model{}, c.Models...)
	for _, p := range f.Providers {
		id := ""
		for _, b := range c.Backends {
			if b.Kind == p.Kind && sameServer(b.BaseURL, p.BaseURL) {
				id = b.ID
				break
			}
		}
		if id == "" {
			id = unique(p.ID, func(s string) bool { _, ok := c.Backend(s); return ok })
			c.Backends = append(c.Backends, config.Backend{ID: id, Kind: p.Kind, BaseURL: p.BaseURL})
		}
		res.Providers[p.ID] = id
		if b, _ := c.Backend(id); strings.TrimSpace(b.APIKey) == "" {
			res.NeedKey = append(res.NeedKey, id)
		}
	}
	for _, m := range f.Models {
		fileID := m.ID
		m.BackendID = res.Providers[m.BackendID]
		id := ""
		for _, x := range c.Models {
			if x.BackendID == m.BackendID && x.Upstream == m.Upstream {
				id = x.ID
				break
			}
		}
		if id == "" {
			id = unique(m.ID, func(s string) bool { _, ok := c.Model(s); return ok })
			m.ID = id
			c.Models = append(c.Models, m)
		}
		res.Models[fileID] = id
	}
	mapModel := func(id string) string {
		if id == "" {
			return ""
		}
		return res.Models[id]
	}
	if f.Roles.Main != "" {
		r := f.Roles
		c.Roles = config.Roles{Main: mapModel(r.Main), Opus: mapModel(r.Opus), Small: mapModel(r.Small),
			Decision: mapModel(r.Decision), Vision: mapModel(r.Vision), Codex: mapModel(r.Codex)}
	}
	mapProvider := func(id string) string {
		if id == "" {
			return ""
		}
		return res.Providers[id]
	}
	c.Documents = config.Documents{Via: mapProvider(f.Documents.Via), PaddleOCR: f.Documents.PaddleOCR,
		Local: f.Documents.Local, LocalOnly: f.Documents.LocalOnly}
	if c.Documents.Local == "" {
		c.Documents.Local = config.LocalOCR
	}
	key := c.WebSearch.APIKey
	if f.WebSearch.Provider != c.WebSearch.Provider {
		key = ""
	}
	c.WebSearch = config.WebSearch{Via: mapProvider(f.WebSearch.Via), Provider: f.WebSearch.Provider, APIKey: key, Off: f.WebSearch.Off}
	return c, res
}
