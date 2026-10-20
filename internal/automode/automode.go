// Package automode answers Claude Code's auto-mode classifier requests from
// one logprob read on the decision model (docs/DESIGN.md §4.3). Every
// failure makes Respond return nil, so the core answers the same request
// generatively on the decision model instead.
//
// Port of the Swift Features/AutoMode/DecisionHead.swift.
package automode

import (
	"context"
	"crypto/rand"
	"math"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"codelink/internal/config"
	"codelink/internal/hooks"
	"codelink/internal/jsonx"
)

// Marker is the sentence every classifier system prompt contains.
const Marker = "You are a security monitor for autonomous AI coding agents"

const (
	severityStop = "</severity>"
	blockStop    = "</block>"
	minMass      = 0.5
	topLogprobs  = 20
	// noLogprobsTTL: how long a decision model that answered without
	// logprobs is skipped (straight to the generative fallback).
	noLogprobsTTL = 10 * time.Minute
)

// Head is the decision head. Safe for concurrent use.
type Head struct {
	model     *config.Model
	caller    hooks.ModelCaller
	threshold float64
	log       hooks.EventLog
	now       func() time.Time

	mu              sync.Mutex
	noLogprobsUntil time.Time
	// noPrefill: models whose server refuses a prefilled assistant message
	// (continue_final_message, e.g. Flare's DeepSeek). They are decided by
	// letting the model write the tag and reading the token after it.
	noPrefill map[string]bool
}

// New is a head deciding on model (nil = none can, every Respond is nil).
// log may be nil.
func New(model *config.Model, caller hooks.ModelCaller, log hooks.EventLog) *Head {
	if log == nil {
		log = hooks.NopLog{}
	}
	return &Head{model: model, caller: caller, threshold: 0.5, log: log, now: time.Now, noPrefill: map[string]bool{}}
}

var _ hooks.AutoMode = (*Head)(nil)

// DecisionModel is the model classifier requests run on.
func (h *Head) DecisionModel() (config.Model, bool) {
	if h.model == nil {
		return config.Model{}, false
	}
	return *h.model, true
}

// MARK: Detection

type format int

const (
	severityFormat format = iota
	blockFormat
)

// classified is a recognised classifier request.
type classified struct {
	format format
	stage  int // 1 = has stop sequences, else 2
	system string
	user   string
}

// IsClassifierRequest: whether an Anthropic Messages request is one.
func (h *Head) IsClassifierRequest(request map[string]any) bool {
	_, ok := classify(request)
	return ok
}

func classify(request map[string]any) (classified, bool) {
	if request == nil {
		return classified{}, false
	}
	system := systemText(request["system"])
	if !strings.Contains(system, Marker) {
		return classified{}, false
	}
	hasSeverity, hasBlock := false, false
	stops := jsonx.Arr(request["stop_sequences"])
	for _, s := range stops {
		switch jsonx.Str(s) {
		case severityStop:
			hasSeverity = true
		case blockStop:
			hasBlock = true
		}
	}
	if strings.Contains(system, "<severity>N</severity>") {
		hasSeverity = true
	}
	if strings.Contains(system, "<block>") {
		hasBlock = true
	}
	if !hasSeverity && !hasBlock {
		return classified{}, false
	}
	user := userText(request["messages"])
	if user == "" {
		return classified{}, false
	}
	c := classified{format: severityFormat, stage: 2, system: system, user: user}
	if hasBlock && !hasSeverity {
		c.format = blockFormat
	}
	if len(stops) > 0 {
		c.stage = 1
	}
	return c, true
}

// systemText is a string system, or its non-empty text blocks joined by
// newlines.
func systemText(system any) string {
	switch s := system.(type) {
	case string:
		return s
	case []any:
		var parts []string
		for _, b := range s {
			if t := jsonx.Str(jsonx.Get(b, "text")); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// userText is the text of the user messages, verbatim, joined by blank lines.
func userText(messages any) string {
	var parts []string
	for _, m := range jsonx.Arr(messages) {
		if jsonx.Str(jsonx.Get(m, "role")) != "user" {
			continue
		}
		switch c := jsonx.Get(m, "content").(type) {
		case string:
			parts = append(parts, c)
		case []any:
			var texts []string
			for _, b := range c {
				t, typed := jsonx.StrOK(jsonx.Get(b, "type"))
				if t == "text" || !typed {
					texts = append(texts, jsonx.Str(jsonx.Get(b, "text")))
				}
			}
			parts = append(parts, strings.Join(texts, "\n"))
		}
	}
	return strings.Join(parts, "\n\n")
}

// MARK: Score math

type score struct {
	pBlock   float64
	expected float64
	mass     float64
}

type tokenLogprob struct {
	token   string
	logprob float64
}

// trimSpaces trims spaces and tabs (not newlines).
func trimSpaces(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return r == '\t' || unicode.Is(unicode.Zs, r) })
}

// scoreSeverity keeps integer tokens 0-100 (spaces trimmed) and normalises
// them; false when their mass is under 0.5.
func scoreSeverity(top []tokenLogprob) (score, bool) {
	probs := map[int]float64{}
	mass := 0.0
	for _, t := range top {
		v, ok := parseSeverityToken(t.token)
		if !ok {
			continue
		}
		p := math.Exp(t.logprob)
		if math.IsInf(p, 0) || math.IsNaN(p) {
			continue
		}
		probs[v] += p
		mass += p
	}
	if mass < minMass {
		return score{}, false
	}
	s := score{mass: mass}
	for v, p0 := range probs {
		p := p0 / mass
		s.expected += float64(v) * p
		if v > 50 {
			s.pBlock += p
		}
	}
	return s, true
}

func parseSeverityToken(tok string) (int, bool) {
	t := trimSpaces(tok)
	if t == "" || utf8.RuneCountInString(t) > 3 {
		return 0, false
	}
	v := 0
	for _, r := range t {
		if r < '0' || r > '9' {
			return 0, false
		}
		v = v*10 + int(r-'0')
	}
	if v > 100 {
		return 0, false
	}
	return v, true
}

func scoreBlock(top []tokenLogprob) (score, bool) {
	yes, no := 0.0, 0.0
	for _, t := range top {
		p := math.Exp(t.logprob)
		if math.IsInf(p, 0) || math.IsNaN(p) {
			continue
		}
		switch strings.ToLower(trimSpaces(t.token)) {
		case "yes":
			yes += p
		case "no":
			no += p
		}
	}
	mass := yes + no
	if mass < minMass {
		return score{}, false
	}
	return score{pBlock: yes / mass, mass: mass}, true
}

// decide: N = round(E[v]) forced to the side p_block chooses; never 50.
func decide(s score, threshold float64) (n int, block bool) {
	n = int(math.Round(s.expected))
	block = s.pBlock >= threshold
	if block {
		n = max(n, 51)
	} else {
		n = min(n, 49)
	}
	return min(max(n, 0), 100), block
}

// MARK: Requests
//
// No temperature anywhere: the score is read from the distribution, not
// sampled, and some servers (Flare) refuse temperature 0.

func decisionBody(c classified, prefill string) map[string]any {
	return map[string]any{
		"max_tokens": 1,
		"logprobs":   true, "top_logprobs": topLogprobs,
		"continue_final_message": true, "add_generation_prompt": false,
		"messages": messages(c, prefill),
	}
}

// locateBody is for servers that refuse a prefill: the model writes the tag
// itself (thinking off) and the distribution of the token after it is read.
func locateBody(c classified, thinkingOff map[string]any) map[string]any {
	maxTokens := 12
	if c.stage == 2 && c.format == severityFormat {
		maxTokens = 48
	}
	body := map[string]any{
		"max_tokens": maxTokens,
		"logprobs":   true, "top_logprobs": topLogprobs,
		"messages": []any{
			map[string]any{"role": "system", "content": c.system},
			map[string]any{"role": "user", "content": c.user},
		},
	}
	off := thinkingOff
	if off == nil {
		off = map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}}
	}
	for k, v := range off {
		body[k] = jsonx.Clone(v)
	}
	return body
}

func categoryBody(c classified, n int) map[string]any {
	return map[string]any{
		"max_tokens":             16,
		"stop":                   []any{"</category>"},
		"continue_final_message": true, "add_generation_prompt": false,
		"messages": messages(c, "<severity>"+itoa(n)+"</severity><category>"),
	}
}

func messages(c classified, prefill string) []any {
	return []any{
		map[string]any{"role": "system", "content": c.system},
		map[string]any{"role": "user", "content": c.user},
		map[string]any{"role": "assistant", "content": prefill},
	}
}

func parseTop(arr []any) []tokenLogprob {
	var out []tokenLogprob
	for _, e := range arr {
		t, ok := jsonx.StrOK(jsonx.Get(e, "token"))
		lp, isNum := jsonx.Float(jsonx.Get(e, "logprob"))
		if ok && isNum {
			out = append(out, tokenLogprob{t, lp})
		}
	}
	return out
}

// topOf is the first generated token's top logprobs.
func topOf(j any) []tokenLogprob {
	return parseTop(jsonx.Arr(jsonx.Get(j, "choices", 0, "logprobs", "content", 0, "top_logprobs")))
}

// topAfter is the top logprobs of the token right after tag in a generated
// reply.
func topAfter(tag string, j any) []tokenLogprob {
	var text strings.Builder
	for _, entry := range jsonx.Arr(jsonx.Get(j, "choices", 0, "logprobs", "content")) {
		if strings.HasSuffix(strings.TrimSpace(text.String()), tag) {
			return parseTop(jsonx.Arr(jsonx.Get(entry, "top_logprobs")))
		}
		text.WriteString(jsonx.Str(jsonx.Get(entry, "token")))
	}
	return nil
}

// locatedCategory is the category a located stage-2 reply wrote, if any.
func locatedCategory(j any) string {
	var text strings.Builder
	for _, entry := range jsonx.Arr(jsonx.Get(j, "choices", 0, "logprobs", "content")) {
		text.WriteString(jsonx.Str(jsonx.Get(entry, "token")))
	}
	_, rest, ok := strings.Cut(text.String(), "<category>")
	if !ok {
		return ""
	}
	if i := strings.Index(rest, "</"); i >= 0 {
		rest = rest[:i]
	}
	return sanitizeCategory(rest)
}

func refusesPrefill(r hooks.ChatResult) bool {
	if r.Status != 400 && r.Status != 422 {
		return false
	}
	text := strings.ToLower(jsonx.String(r.JSON))
	return strings.Contains(text, "continue_final_message") || strings.Contains(text, "prefill")
}

// generatedText is the reply's content, else its reasoning.
func generatedText(j any) string {
	m := jsonx.Get(j, "choices", 0, "message")
	for _, key := range []string{"content", "reasoning", "reasoning_content"} {
		if s := jsonx.Str(jsonx.Get(m, key)); strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// sanitizeCategory keeps letters, digits and spaces.
func sanitizeCategory(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' {
			return r
		}
		return -1
	}, s)
	return trimSpaces(s)
}

// MARK: Answer

// answer is the classifier reply's text and stop fields (stopSequence ""
// = null).
func answer(c classified, n int, block bool, category string) (text, stopReason, stopSequence string) {
	if c.format == blockFormat {
		a := "no"
		if block {
			a = "yes"
		}
		if c.stage == 1 {
			return "<block>" + a, "stop_sequence", blockStop
		}
		return "<block>" + a + "</block>", "end_turn", ""
	}
	if c.stage == 1 {
		return "<severity>" + itoa(n), "stop_sequence", severityStop
	}
	text = "<severity>" + itoa(n) + "</severity>"
	if n > 50 && category != "" {
		text += "<category>" + category + "</category>"
	}
	return text, "end_turn", ""
}

type usage struct{ prompt, completion, cached int }

func (u *usage) add(j any) {
	us := jsonx.Get(j, "usage")
	if us == nil {
		return
	}
	n := func(path ...any) int { v, _ := jsonx.Float(jsonx.Get(us, path...)); return int(v) }
	u.prompt += n("prompt_tokens")
	u.completion += n("completion_tokens")
	u.cached += n("prompt_tokens_details", "cached_tokens")
}

func (u usage) json() map[string]any {
	return map[string]any{
		"input_tokens":                max(u.prompt-u.cached, 0),
		"output_tokens":               u.completion,
		"cache_creation_input_tokens": 0,
		"cache_read_input_tokens":     u.cached,
	}
}

// MARK: Respond

func ok(r hooks.ChatResult) bool { return r.Status >= 200 && r.Status < 300 }

// Respond gives the complete Anthropic Messages response (non-streaming
// shape) for a classifier request, or nil to fall back.
func (h *Head) Respond(ctx context.Context, request map[string]any) map[string]any {
	c, isClassifier := classify(request)
	if !isClassifier || h.model == nil {
		return nil
	}
	model := *h.model
	started := h.now()
	fail := func(why string) map[string]any { h.record(c, model, 0, 0, started, why); return nil }
	if (model.Logprobs != nil && !*model.Logprobs) || h.logprobsKnownMissing() {
		return fail("no logprobs")
	}
	var u usage

	prefill := "<severity>"
	if c.format == blockFormat {
		prefill = "<block>"
	}
	located := h.usesLocate(model)
	body := decisionBody(c, prefill)
	if located {
		body = locateBody(c, model.ThinkingOff)
	}
	first, err := h.caller.Chat(ctx, model, body)
	if err == nil && !located && refusesPrefill(first) {
		h.markNoPrefill(model)
		located = true
		first, err = h.caller.Chat(ctx, model, locateBody(c, model.ThinkingOff))
	}
	if err != nil {
		return fail("upstream error")
	}
	if !ok(first) {
		return fail("upstream HTTP " + itoa(first.Status))
	}
	u.add(first.JSON)

	var top []tokenLogprob
	if located {
		top = topAfter(prefill, first.JSON)
	} else {
		top = topOf(first.JSON)
	}
	if len(top) == 0 {
		// Logprobs came back but the tag wasn't written (located): not a
		// missing-logprobs server, just no usable answer this time.
		if !located || jsonx.Get(first.JSON, "choices", 0, "logprobs", "content") == nil {
			h.markNoLogprobs()
		}
		if located {
			return fail("tag not found")
		}
		return fail("no logprobs")
	}
	var s score
	var scored bool
	if c.format == blockFormat {
		s, scored = scoreBlock(top)
	} else {
		s, scored = scoreSeverity(top)
	}
	if !scored {
		return fail("low token mass")
	}
	n, block := decide(s, h.threshold)

	category := ""
	if c.format == severityFormat && c.stage == 2 && block {
		if located {
			category = locatedCategory(first.JSON)
		} else if r, err := h.caller.Chat(ctx, model, categoryBody(c, n)); err == nil && ok(r) {
			u.add(r.JSON)
			category = sanitizeCategory(generatedText(r.JSON))
		}
	}

	text, stopReason, stopSequence := answer(c, n, block, category)
	h.record(c, model, s.pBlock, n, started, "")
	requested := jsonx.Str(request["model"])
	if requested == "" {
		requested = model.ID
	}
	var stop any
	if stopSequence != "" {
		stop = stopSequence
	}
	return map[string]any{
		"id":            "msg_" + randomID(),
		"type":          "message",
		"role":          "assistant",
		"content":       []any{map[string]any{"type": "text", "text": text}},
		"model":         requested,
		"stop_reason":   stopReason,
		"stop_sequence": stop,
		"usage":         u.json(),
	}
}

func (h *Head) record(c classified, m config.Model, pBlock float64, n int, started time.Time, fallback string) {
	h.log.Record(hooks.Event{Kind: "decision", Stage: c.stage, Model: m.ID, PBlock: pBlock, Severity: n,
		LatencyMS: int(h.now().Sub(started) / time.Millisecond), Fallback: fallback})
}

func (h *Head) logprobsKnownMissing() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now().Before(h.noLogprobsUntil)
}

func (h *Head) markNoLogprobs() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.noLogprobsUntil = h.now().Add(noLogprobsTTL)
}

func (h *Head) usesLocate(m config.Model) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.noPrefill[m.ID]
}

func (h *Head) markNoPrefill(m config.Model) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.noPrefill[m.ID] = true
}

func randomID() string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 24)
	rand.Read(b)
	for i := range b {
		b[i] = chars[int(b[i])%len(chars)]
	}
	return string(b)
}

func itoa(n int) string { return jsonx.Num(n).String() }
