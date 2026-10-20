package app

import (
	"context"
	"net/url"
	"strings"

	"codelink/internal/config"
	"codelink/internal/core"
)

// KeyOnlySetup is the built-in gateway waiting for a key: the settings
// aren't configured and their only provider is a Go-LLM-Proxy without one.
// Settings then opens on "Get started".
func KeyOnlySetup(c *config.Config) (config.Backend, bool) {
	if c.Configured() || len(c.Backends) != 1 {
		return config.Backend{}, false
	}
	b := c.Backends[0]
	if b.Kind != config.GoLLMProxy || strings.TrimSpace(b.APIKey) != "" {
		return config.Backend{}, false
	}
	return b, true
}

// JoinURL is where a gateway hands out keys: its origin + /join.
func JoinURL(b config.Backend) string { return config.GatewayURL(b, "/join") }

// Host is a provider's host name, for messages.
func Host(b config.Backend) string {
	if u, err := url.Parse(strings.TrimSpace(b.BaseURL)); err == nil && u.Host != "" {
		return u.Host
	}
	return b.ID
}

// KeyError words a failed probe of a provider just given a key.
func KeyError(b config.Backend, p core.Probe) string {
	host := Host(b)
	switch {
	case p.Reachable && len(p.Models) == 0:
		return host + " has no models for this key."
	case p.Reachable:
		return ""
	case strings.HasPrefix(p.Error, "HTTP 401"), strings.HasPrefix(p.Error, "HTTP 403"):
		return "That key wasn't accepted. Check that you pasted all of it."
	case strings.HasPrefix(p.Error, "HTTP 5"), strings.HasPrefix(p.Error, "HTTP 429"):
		return host + " isn't answering right now (" + strings.SplitN(p.Error, ":", 2)[0] + "). Try again in a moment."
	case strings.HasPrefix(p.Error, "HTTP "):
		return host + " answered " + strings.SplitN(p.Error, ":", 2)[0] + ". Check the provider's address."
	}
	return "Can't reach " + host + ". Check your internet connection and try again."
}

// KeyNeed is a provider an import (or first run) wants a key for.
type KeyNeed struct {
	ID      string             `json:"id"`
	Kind    config.BackendKind `json:"kind"`
	Host    string             `json:"host"`
	JoinURL string             `json:"joinURL"`
	// Required: a Go-LLM-Proxy can't be used without one; for other
	// providers it is optional.
	Required bool `json:"required"`
}

// Needs describes providers by id for the key step.
func Needs(c *config.Config, ids []string) []KeyNeed {
	out := []KeyNeed{}
	for _, id := range ids {
		b, ok := c.Backend(id)
		if !ok {
			continue
		}
		n := KeyNeed{ID: id, Kind: b.Kind, Host: Host(b), Required: b.Kind == config.GoLLMProxy}
		if b.Kind == config.GoLLMProxy {
			n.JoinURL = JoinURL(b)
		}
		out = append(out, n)
	}
	return out
}

// KeysResult is ApplyKeys' answer.
type KeysResult struct {
	OK bool `json:"ok"`
	// Errors are per provider id, in plain words.
	Errors map[string]string `json:"errors"`
	View   View              `json:"view"`
	// Save is the save's result when one was asked for and the keys worked.
	Save *SaveResult `json:"save"`
}

// ApplyKeys puts keys on providers in a draft. A Go-LLM-Proxy is tested
// with its key at once: when it answers, its models are synced in (with
// its recommendations). With save, a draft whose keys all worked is saved.
func (a *App) ApplyKeys(ctx context.Context, d config.Config, keys map[string]string, save bool) KeysResult {
	errs := map[string]string{}
	for i := range d.Backends {
		k, ok := keys[d.Backends[i].ID]
		if !ok {
			continue
		}
		b := &d.Backends[i]
		b.APIKey = strings.TrimSpace(k)
		if b.Kind != config.GoLLMProxy {
			continue
		}
		if b.APIKey == "" {
			errs[b.ID] = "Paste your API key."
			continue
		}
		p := a.Probe(ctx, *b)
		if msg := KeyError(*b, p); msg != "" {
			errs[b.ID] = msg
			continue
		}
		SyncModels(&d, b.ID, p)
	}
	res := KeysResult{OK: len(errs) == 0, Errors: errs, View: a.Describe(d)}
	if res.OK && save {
		s := a.Save(d)
		res.Save = &s
		res.OK = s.OK
		if s.OK {
			res.View = a.Describe(s.Saved)
		}
	}
	return res
}

// --- Web search and documents choices ---

// Option is a choice in a picker.
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// SearchChoice is the web search picker's value: off, auto (through a
// keyed gateway because nothing else is set), via:<provider>, tavily or
// brave.
func SearchChoice(c *config.Config) string {
	w := c.WebSearch
	switch {
	case w.Off:
		return "off"
	case w.Via != "":
		return "via:" + w.Via
	case w.Provider != "":
		return string(w.Provider)
	}
	if _, ok := c.Gateway(); ok {
		return "auto"
	}
	return "off"
}

// SearchOptions are the web search picker's choices.
func SearchOptions(c *config.Config, probes map[string]core.Probe) []Option {
	var out []Option
	if g, ok := c.Gateway(); ok {
		out = append(out, Option{"auto", "Automatic, through " + g.ID})
	}
	if cur := SearchChoice(c); strings.HasPrefix(cur, "via:") {
		out = append(out, Option{cur, "Through " + strings.TrimPrefix(cur, "via:")})
	}
	return append(out, Option{"tavily", "Tavily"}, Option{"brave", "Brave"}, Option{"off", "Off"})
}

// SetSearchChoice applies a picker value to the draft.
func SetSearchChoice(c *config.Config, v string) {
	w := &c.WebSearch
	w.Off, w.Via, w.Provider = false, "", ""
	switch {
	case v == "off":
		w.Off = true
	case strings.HasPrefix(v, "via:"):
		w.Via = strings.TrimPrefix(v, "via:")
	case v == string(config.Tavily), v == string(config.Brave):
		w.Provider = config.SearchProvider(v)
	}
}

// DocumentsChoice is the documents picker's value: auto, via:<provider>,
// paddle (a PaddleOCR server of the user's own) or local (this computer
// only).
func DocumentsChoice(c *config.Config) string {
	d := c.Documents
	switch {
	case d.LocalOnly:
		return "local"
	case d.Via != "":
		return "via:" + d.Via
	case strings.TrimSpace(d.PaddleOCR) != "":
		return "paddle"
	}
	if _, ok := c.Gateway(); ok {
		return "auto"
	}
	return "local"
}

// DocumentsOptions are the documents picker's choices.
func DocumentsOptions(c *config.Config, probes map[string]core.Probe) []Option {
	var out []Option
	if g, ok := c.Gateway(); ok {
		out = append(out, Option{"auto", "Automatic, through " + g.ID})
	}
	if cur := DocumentsChoice(c); strings.HasPrefix(cur, "via:") {
		out = append(out, Option{cur, "Through " + strings.TrimPrefix(cur, "via:")})
	}
	return append(out, Option{"paddle", "A PaddleOCR server"}, Option{"local", "On this computer only"})
}

// SetDocumentsChoice applies a picker value to the draft. Choosing a
// PaddleOCR server starts its URL off with "http://".
func SetDocumentsChoice(c *config.Config, v string) {
	d := &c.Documents
	d.Via, d.LocalOnly = "", false
	switch {
	case strings.HasPrefix(v, "via:"):
		d.Via = strings.TrimPrefix(v, "via:")
	case v == "paddle":
		if strings.TrimSpace(d.PaddleOCR) == "" {
			d.PaddleOCR = "http://"
		}
	case v == "local":
		d.LocalOnly, d.PaddleOCR = true, ""
	default: // auto
		d.PaddleOCR = ""
	}
}
