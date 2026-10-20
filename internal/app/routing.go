package app

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"codelink/internal/config"
	"codelink/internal/core"
)

// Slot is a role a picker sets.
type Slot string

const (
	SlotMain     Slot = "main"
	SlotOpus     Slot = "opus"
	SlotSmall    Slot = "small"
	SlotDecision Slot = "decision"
	SlotVision   Slot = "vision"
	SlotCodex    Slot = "codex"
)

// Slots in display order.
var Slots = []Slot{SlotMain, SlotOpus, SlotSmall, SlotDecision, SlotVision, SlotCodex}

// Label is the slot's name in Settings and warnings.
func (s Slot) Label() string {
	switch s {
	case SlotOpus:
		return "Opus"
	case SlotSmall:
		return "Haiku"
	case SlotDecision:
		return "Decision"
	case SlotVision:
		return "Vision"
	case SlotCodex:
		return "Codex"
	}
	return "Main"
}

// RoleID is the model id a slot names ("" = unset).
func RoleID(c *config.Config, s Slot) string {
	r := &c.Roles
	switch s {
	case SlotOpus:
		return r.Opus
	case SlotSmall:
		return r.Small
	case SlotDecision:
		return r.Decision
	case SlotVision:
		return r.Vision
	case SlotCodex:
		return r.Codex
	}
	return r.Main
}

func setRoleID(c *config.Config, s Slot, id string) {
	r := &c.Roles
	switch s {
	case SlotOpus:
		r.Opus = id
	case SlotSmall:
		r.Small = id
	case SlotDecision:
		r.Decision = id
	case SlotVision:
		r.Vision = id
	case SlotCodex:
		r.Codex = id
	default:
		r.Main = id
	}
}

// Choice is a model on a provider, as the pickers see it.
type Choice struct {
	Backend  string `json:"backend"`
	Upstream string `json:"upstream"`
}

const tagSep = "\x01"

// Tag is the choice as one string (a <select> value).
func (c Choice) Tag() string { return c.Backend + tagSep + c.Upstream }

// ParseTag reverses Tag; ok false for "" or a malformed tag.
func ParseTag(tag string) (Choice, bool) {
	b, u, ok := strings.Cut(tag, tagSep)
	if !ok {
		return Choice{}, false
	}
	return Choice{b, u}, true
}

// ChoiceFor is the provider model a slot uses.
func ChoiceFor(c *config.Config, s Slot) (Choice, bool) {
	m, ok := c.Model(RoleID(c, s))
	if !ok {
		return Choice{}, false
	}
	return Choice{m.BackendID, m.Upstream}, true
}

func findModel(c *config.Config, ch Choice) int {
	for i, m := range c.Models {
		if m.BackendID == ch.Backend && m.Upstream == ch.Upstream {
			return i
		}
	}
	return -1
}

// EnsureModel finds or creates the model entry for a provider's model and
// returns its id (the name sessions see). A same-named model on another
// provider gets the provider appended. Facts a probe found fill in what the
// entry doesn't say yet: context window, logprobs and vision.
func EnsureModel(c *config.Config, ch Choice, probed *core.ProbedModel) string {
	if i := findModel(c, ch); i >= 0 {
		m := &c.Models[i]
		if probed != nil {
			if m.ContextWindow == nil {
				m.ContextWindow = probed.ContextWindow
			}
			if m.Logprobs == nil {
				m.Logprobs = probed.Logprobs
			}
			if m.Vision == nil {
				m.Vision = probed.Vision
			}
		}
		return m.ID
	}
	id := ch.Upstream
	for n := 2; ; n++ {
		if _, taken := c.Model(id); !taken {
			break
		}
		id = ch.Upstream + " (" + ch.Backend + ")"
		if n > 2 {
			id = ch.Upstream + " (" + ch.Backend + " " + strconv.Itoa(n-1) + ")"
		}
	}
	m := config.Model{ID: id, BackendID: ch.Backend, Upstream: ch.Upstream}
	if probed != nil {
		m.ContextWindow, m.Logprobs, m.Vision = probed.ContextWindow, probed.Logprobs, probed.Vision
	}
	c.Models = append(c.Models, m)
	return id
}

// SetRole points a role at a provider's model (nil clears it). Picking a
// model for a role enables it.
func SetRole(c *config.Config, s Slot, ch *Choice, probed *core.ProbedModel) {
	if ch == nil || ch.Backend == "" || ch.Upstream == "" {
		setRoleID(c, s, "")
		return
	}
	setRoleID(c, s, EnsureModel(c, *ch, probed))
}

// IsEnabled: the provider's model has an entry.
func IsEnabled(c *config.Config, ch Choice) bool { return findModel(c, ch) >= 0 }

// SetEnabled turns a provider's model on (creating its entry) or off.
// Turning one off clears every role that used it and returns those roles,
// so the caller can say so.
func SetEnabled(c *config.Config, ch Choice, on bool, probed *core.ProbedModel) []Slot {
	if on {
		EnsureModel(c, ch, probed)
		return nil
	}
	i := findModel(c, ch)
	if i < 0 {
		return nil
	}
	id := c.Models[i].ID
	var cleared []Slot
	for _, s := range Slots {
		if RoleID(c, s) == id {
			setRoleID(c, s, "")
			cleared = append(cleared, s)
		}
	}
	c.Models = append(c.Models[:i:i], c.Models[i+1:]...)
	return cleared
}

// RenameModel changes the name sessions see for a model (an alias) and
// follows it in the roles. Refuses an empty or taken name.
func RenameModel(c *config.Config, old, name string) bool {
	n := strings.TrimSpace(name)
	if n == "" || n == old {
		return false
	}
	if _, taken := c.Model(n); taken {
		return false
	}
	for i := range c.Models {
		if c.Models[i].ID == old {
			c.Models[i].ID = n
			for _, s := range Slots {
				if RoleID(c, s) == old {
					setRoleID(c, s, n)
				}
			}
			return true
		}
	}
	return false
}

// DropOrphanModels removes models whose provider is gone.
func DropOrphanModels(c *config.Config) {
	out := c.Models[:0:0]
	for _, m := range c.Models {
		if _, ok := c.Backend(m.BackendID); ok {
			out = append(out, m)
		}
	}
	c.Models = out
}

// RenameBackend renames a provider and follows it in the models. Refuses an
// empty or taken name.
func RenameBackend(c *config.Config, old, name string) bool {
	n := strings.TrimSpace(name)
	if n == "" || n == old {
		return false
	}
	if _, taken := c.Backend(n); taken {
		return false
	}
	found := false
	for i := range c.Backends {
		if c.Backends[i].ID == old {
			c.Backends[i].ID = n
			found = true
		}
	}
	if !found {
		return false
	}
	for i := range c.Models {
		if c.Models[i].BackendID == old {
			c.Models[i].BackendID = n
		}
	}
	return true
}

// AddBackend appends a new OpenAI-compatible provider named provider-N and
// returns its id.
func AddBackend(c *config.Config) string {
	n := len(c.Backends) + 1
	id := "provider-" + strconv.Itoa(n)
	for {
		if _, taken := c.Backend(id); !taken {
			break
		}
		n++
		id = "provider-" + strconv.Itoa(n)
	}
	c.Backends = append(c.Backends, config.Backend{ID: id, Kind: config.OpenAICompatible, BaseURL: "http://localhost:8000/v1"})
	return id
}

// RemoveBackend removes a provider with its models and returns the roles
// that lost their model.
func RemoveBackend(c *config.Config, id string) []Slot {
	var cleared []Slot
	for _, s := range Slots {
		if m, ok := c.Model(RoleID(c, s)); ok && m.BackendID == id {
			setRoleID(c, s, "")
			cleared = append(cleared, s)
		}
	}
	backends := c.Backends[:0:0]
	for _, b := range c.Backends {
		if b.ID != id {
			backends = append(backends, b)
		}
	}
	c.Backends = backends
	models := c.Models[:0:0]
	for _, m := range c.Models {
		if m.BackendID != id {
			models = append(models, m)
		}
	}
	c.Models = models
	return cleared
}

// ValidURL: an http:// or https:// URL with a host.
func ValidURL(s string) bool {
	u, err := url.Parse(strings.TrimSpace(s))
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// Problems stop a save; empty when fine.
func Problems(c *config.Config) []string {
	var out []string
	seen := map[string]bool{}
	blank, dup := false, false
	for _, b := range c.Backends {
		if strings.TrimSpace(b.ID) == "" {
			blank = true
		}
		if seen[b.ID] {
			dup = true
		}
		seen[b.ID] = true
	}
	if blank {
		out = append(out, "Every provider needs a name.")
	}
	if dup {
		out = append(out, "Provider names must be unique.")
	}
	if c.Port < 1024 || c.Port > 65535 {
		out = append(out, "The port must be between 1024 and 65535.")
	}
	for _, b := range c.Backends {
		if !ValidURL(b.BaseURL) {
			out = append(out, fmt.Sprintf("Provider %s: the base URL must be http:// or https://.", b.ID))
		}
	}
	for _, m := range c.Models {
		if _, ok := c.Backend(m.BackendID); !ok {
			out = append(out, fmt.Sprintf("Model %s refers to a provider that no longer exists.", m.ID))
		}
	}
	for _, s := range Slots {
		if id := RoleID(c, s); id != "" {
			if _, ok := c.Model(id); !ok {
				out = append(out, fmt.Sprintf("The %s model (%s) is not available on any provider.", strings.ToLower(s.Label()), id))
			}
		}
	}
	if u := c.Documents.PaddleOCR; u != "" && !ValidURL(u) {
		out = append(out, "The PaddleOCR URL must be http:// or https://.")
	}
	if c.WebSearch.Provider != "" && strings.TrimSpace(c.WebSearch.APIKey) == "" {
		out = append(out, "Web search needs an API key (or turn it off).")
	}
	return out
}

// ChoiceItem is one model in a picker or a provider's checklist.
type ChoiceItem struct {
	Tag      string `json:"tag"`
	Backend  string `json:"backend"`
	Upstream string `json:"upstream"`
	// ModelID is the enabled model's id (its alias); "" when not enabled.
	ModelID       string `json:"modelID"`
	Enabled       bool   `json:"enabled"`
	Title         string `json:"title"`
	ContextWindow *int   `json:"contextWindow"`
	Logprobs      *bool  `json:"logprobs"`
	Vision        *bool  `json:"vision"`
	// Facts is "262k · logprobs · vision".
	Facts string `json:"facts"`
}

// ChoiceGroup is a provider's models, for a grouped picker.
type ChoiceGroup struct {
	Backend string       `json:"backend"`
	Items   []ChoiceItem `json:"items"`
}

// Groups are the providers in order, each with its enabled models first
// and (unless enabledOnly) the models its last probe found that aren't
// enabled.
func Groups(c *config.Config, probes map[string]core.Probe, enabledOnly bool) []ChoiceGroup {
	out := []ChoiceGroup{}
	for _, b := range c.Backends {
		var items []ChoiceItem
		for _, m := range c.Models {
			if m.BackendID != b.ID {
				continue
			}
			items = append(items, item(Choice{b.ID, m.Upstream}, m.ID, m.ContextWindow, m.Logprobs, m.Vision))
		}
		if !enabledOnly {
			for _, p := range probes[b.ID].Models {
				if !IsEnabled(c, Choice{b.ID, p.Upstream}) {
					items = append(items, item(Choice{b.ID, p.Upstream}, "", p.ContextWindow, p.Logprobs, p.Vision))
				}
			}
		}
		if len(items) > 0 {
			out = append(out, ChoiceGroup{Backend: b.ID, Items: items})
		}
	}
	return out
}

func item(ch Choice, id string, window *int, logprobs, vision *bool) ChoiceItem {
	it := ChoiceItem{Tag: ch.Tag(), Backend: ch.Backend, Upstream: ch.Upstream, ModelID: id, Enabled: id != "",
		Title: id, ContextWindow: window, Logprobs: logprobs, Vision: vision}
	if it.Title == "" {
		it.Title = ch.Upstream
	}
	it.Facts = Facts(it)
	return it
}

// ShortContext is "1M" / "262k" / "512" for a context window.
func ShortContext(n int) string {
	switch {
	case n >= 1_000_000:
		return strconv.Itoa((n+500_000)/1_000_000) + "M"
	case n >= 1000:
		return strconv.Itoa(n/1000) + "k"
	}
	return strconv.Itoa(n)
}

// Facts is the "262k · logprobs · vision" line.
func Facts(it ChoiceItem) string {
	var parts []string
	if it.ContextWindow != nil {
		parts = append(parts, ShortContext(*it.ContextWindow))
	}
	if it.Logprobs != nil && *it.Logprobs {
		parts = append(parts, "logprobs")
	}
	if it.Vision != nil && *it.Vision {
		parts = append(parts, "vision")
	}
	return strings.Join(parts, " · ")
}

// ProbedFor is what the last probe found about a provider's model.
func ProbedFor(probes map[string]core.Probe, ch Choice) *core.ProbedModel {
	for _, p := range probes[ch.Backend].Models {
		if p.Upstream == ch.Upstream {
			p := p
			return &p
		}
	}
	return nil
}

// DecisionNote says in plain words whether auto mode's decision model
// (the decision role, else main) can answer in a single token.
func DecisionNote(c *config.Config, probes map[string]core.Probe) string {
	id := c.Roles.Decision
	if id == "" {
		id = c.Roles.Main
	}
	m, ok := c.Model(id)
	if !ok {
		return ""
	}
	lp := m.Logprobs
	if lp == nil {
		if p := ProbedFor(probes, Choice{m.BackendID, m.Upstream}); p != nil {
			lp = p.Logprobs
		}
	}
	switch {
	case lp == nil:
		return "Not checked yet whether " + m.ID + " returns logprobs. Test its provider to find out."
	case *lp:
		return m.ID + " returns logprobs, so auto mode approves actions with a single-token decision."
	}
	return m.ID + " has no logprobs, so auto mode approvals use slower generated answers."
}

// ClearedWarning is the note shown when roles lost their model.
func ClearedWarning(cleared []Slot, reason string) string {
	if len(cleared) == 0 {
		return ""
	}
	names := make([]string, len(cleared))
	for i, s := range cleared {
		names[i] = s.Label()
	}
	verb := "has"
	if len(cleared) > 1 {
		verb = "have"
	}
	return fmt.Sprintf("%s %s no model now: %s. Pick another.", strings.Join(names, ", "), verb, reason)
}
