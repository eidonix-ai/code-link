// Package images describes an image with the vision-role model for a model
// without vision (docs/DESIGN.md §4.4): one call per image, cached by
// SHA-256.
//
// Port of the Swift Features/Images/ModelImageDescriber.swift.
package images

import (
	"context"
	"encoding/base64"
	"regexp"
	"strings"

	"codelink/internal/config"
	"codelink/internal/hooks"
	"codelink/internal/jsonx"
	"codelink/internal/lru"
)

// Prompt is what the vision model is asked.
const Prompt = "Describe this image in detail for someone who cannot see it, including any text it contains."

const (
	maxTokens = 1500
	// thinkingMaxTokens is room for a thinking model that keeps thinking on.
	thinkingMaxTokens = 8192
)

// Describer is a hooks.ImageDescriber on one model. Safe for concurrent use.
type Describer struct {
	model  config.Model
	caller hooks.ModelCaller
	cache  *lru.Cache[string]
}

var _ hooks.ImageDescriber = (*Describer)(nil)

func New(model config.Model, caller hooks.ModelCaller) *Describer {
	return &Describer{model: model, caller: caller, cache: lru.New[string](128, 4<<20)}
}

// requestBody asks for a description with thinking off (the model's
// ThinkingOff, else enable_thinking false) so the budget goes to the
// description; thinking true asks without it and with a larger budget.
func requestBody(image []byte, mediaType string, thinkingOff map[string]any, thinking bool) map[string]any {
	url := "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(image)
	body := map[string]any{
		"max_tokens": maxTokens,
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}},
				map[string]any{"type": "text", "text": Prompt},
			},
		}},
	}
	if thinking {
		body["max_tokens"] = thinkingMaxTokens
		return body
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

// Describe gives the description, or "" when the model could not.
func (d *Describer) Describe(ctx context.Context, image []byte, mediaType string) string {
	key := lru.SHA256(image)
	if hit, ok := d.cache.Get(key); ok {
		return hit
	}
	// Thinking off first; a server that refuses that, or a model that still
	// spends the budget thinking, is asked again with thinking on.
	text := d.ask(ctx, requestBody(image, mediaType, d.model.ThinkingOff, false))
	if text == "" && ctx.Err() == nil {
		text = d.ask(ctx, requestBody(image, mediaType, nil, true))
	}
	if text != "" {
		d.cache.Put(key, text, len(text))
	}
	return text
}

func (d *Describer) ask(ctx context.Context, body map[string]any) string {
	r, err := d.caller.Chat(ctx, d.model, body)
	if err != nil || r.Status < 200 || r.Status > 299 {
		return ""
	}
	return strings.TrimSpace(stripThinking(jsonx.Str(jsonx.Get(r.JSON, "choices", 0, "message", "content"))))
}

var thinkBlock = regexp.MustCompile(`<think>[\s\S]*?</think>`)

// stripThinking drops <think>…</think> a backend leaves in content, and a
// lone leading </think>.
func stripThinking(s string) string {
	t := thinkBlock.ReplaceAllString(s, "")
	if before, after, ok := strings.Cut(t, "</think>"); ok && (strings.TrimSpace(before) == "" || !strings.Contains(t, "<think>")) {
		t = after
	}
	return t
}
