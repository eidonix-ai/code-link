package images

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"codelink/internal/config"
	"codelink/internal/hooks"
	"codelink/internal/jsonx"
)

type fakeCaller struct {
	mu      sync.Mutex
	bodies  []map[string]any
	models  []config.Model
	handler func(i int) (hooks.ChatResult, error)
}

func (f *fakeCaller) Chat(_ context.Context, m config.Model, body map[string]any) (hooks.ChatResult, error) {
	f.mu.Lock()
	f.bodies = append(f.bodies, body)
	f.models = append(f.models, m)
	i := len(f.bodies) - 1
	f.mu.Unlock()
	return f.handler(i)
}

func (f *fakeCaller) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.bodies) }

func fixed(r hooks.ChatResult) *fakeCaller {
	return &fakeCaller{handler: func(int) (hooks.ChatResult, error) { return r, nil }}
}

func t_(b bool) *bool { return &b }

var vmodel = config.Model{ID: "V", BackendID: "b", Upstream: "vl", Vision: t_(true)}

func reply(content any) hooks.ChatResult {
	return hooks.ChatResult{Status: 200, JSON: map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}}}}}
}

func canon(v any) string {
	p, err := jsonx.Parse(jsonx.Marshal(v))
	if err != nil {
		panic(err)
	}
	return jsonx.String(p)
}

func TestDescribeSendsOneImageRequestAndReturnsText(t *testing.T) {
	c := fixed(reply("  A red square with the text HELLO.  "))
	got := New(vmodel, c).Describe(context.Background(), []byte{1, 2, 3, 4}, "image/png")
	if got != "A red square with the text HELLO." {
		t.Fatalf("%q", got)
	}
	if !reflect.DeepEqual(c.models[0], vmodel) {
		t.Fatal("model")
	}
	want := `{"max_tokens":1500,"chat_template_kwargs":{"enable_thinking":false},"messages":[{"role":"user","content":[
	  {"type":"image_url","image_url":{"url":"data:image/png;base64,AQIDBA=="}},
	  {"type":"text","text":"Describe this image in detail for someone who cannot see it, including any text it contains."}]}]}`
	w, _ := jsonx.Parse([]byte(want))
	if canon(c.bodies[0]) != jsonx.String(w) {
		t.Fatalf("body %s", canon(c.bodies[0]))
	}
	if c.bodies[0]["temperature"] != nil {
		t.Fatal("temperature")
	}
}

// A server that refuses thinking off, or a reply that is all thinking, is
// asked again with thinking on and a larger budget.
func TestDescribeRetriesWithThinkingOn(t *testing.T) {
	c := &fakeCaller{handler: func(i int) (hooks.ChatResult, error) {
		if i == 0 {
			return reply(nil), nil
		}
		return reply("A red square."), nil
	}}
	if got := New(vmodel, c).Describe(context.Background(), []byte{1}, "image/png"); got != "A red square." {
		t.Fatalf("%q", got)
	}
	if c.count() != 2 {
		t.Fatalf("%d calls", c.count())
	}
	second := c.bodies[1]
	if second["chat_template_kwargs"] != nil || second["max_tokens"] != thinkingMaxTokens {
		t.Fatalf("second body %s", canon(second))
	}
}

func TestDescribeUsesTheModelsThinkingOff(t *testing.T) {
	m := config.Model{ID: "G", BackendID: "b", Upstream: "g", Vision: t_(true), ThinkingOff: map[string]any{"reasoning_effort": "low"}}
	c := fixed(reply("ok"))
	New(m, c).Describe(context.Background(), []byte{1}, "image/png")
	if c.bodies[0]["reasoning_effort"] != "low" || c.bodies[0]["chat_template_kwargs"] != nil {
		t.Fatalf("body %s", canon(c.bodies[0]))
	}
}

func TestDescribeCachesBySHA256(t *testing.T) {
	c := fixed(reply("cat"))
	d := New(vmodel, c)
	a := []byte{9, 9, 9}
	if d.Describe(context.Background(), a, "image/png") != "cat" || d.Describe(context.Background(), a, "image/png") != "cat" {
		t.Fatal("describe")
	}
	if c.count() != 1 {
		t.Fatalf("%d calls", c.count())
	}
	d.Describe(context.Background(), []byte{8}, "image/png")
	if c.count() != 2 {
		t.Fatalf("%d calls", c.count())
	}
}

func TestDescribeFailuresAreEmptyAndNotCached(t *testing.T) {
	// Each describe asks twice (thinking off, then on).
	c := &fakeCaller{handler: func(i int) (hooks.ChatResult, error) {
		switch {
		case i == 0:
			return hooks.ChatResult{}, errors.New("fake")
		case i < 3:
			return hooks.ChatResult{Status: 500}, nil
		case i < 6:
			return reply("  "), nil
		}
		return reply("<think>hmm</think>A dog."), nil
	}}
	d := New(vmodel, c)
	img := []byte{7}
	for i := range 3 {
		if got := d.Describe(context.Background(), img, "image/jpeg"); got != "" {
			t.Fatalf("try %d: %q", i, got)
		}
	}
	if got := d.Describe(context.Background(), img, "image/jpeg"); got != "A dog." {
		t.Fatalf("%q", got)
	}
}

func TestStripThinking(t *testing.T) {
	cases := map[string]string{
		"<think>a</think>B":          "B",
		"</think>B":                  "B",
		"  </think> B":               " B",
		"<think>a</think>B</think>C": "C",
		"A <think>x":                 "A <think>x",
		"plain":                      "plain",
	}
	for in, want := range cases {
		if got := stripThinking(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
