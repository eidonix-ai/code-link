package responses

import (
	"slices"
	"strings"
	"testing"

	"codelink/internal/jsonx"
)

func obj(s string) any {
	v, err := jsonx.Parse([]byte(s))
	if err != nil {
		panic(err)
	}
	return v
}

func TestFlatteningIsCollisionSafeAndBounded(t *testing.T) {
	tools := jsonx.Arr(obj(`[
		{"type":"function","name":"exec"},
		{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"},{"type":"function","name":"wait"}]},
		{"type":"namespace","name":"mcp.server","tools":[{"type":"function","name":"do thing"},{"type":"function","name":"wait"}]},
		{"type":"namespace","name":"mcp_server","tools":[{"type":"function","name":"do_thing"}]},
		{"type":"web_search"}]`))
	tr := translateTools(tools)
	var names []string
	for _, x := range tr.tools {
		names = append(names, jsonx.Str(jsonx.Get(x, "function", "name")))
	}
	want := []string{"exec", "functions__exec", "wait", "mcp_server__do_thing", "mcp_server__wait", "mcp_server__do_thing_2"}
	if !slices.Equal(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
	if len(tr.stripped) != 1 || jsonx.Type(tr.stripped[0]) != "web_search" {
		t.Errorf("stripped = %v", tr.stripped)
	}
	if b, ok := tr.registry.resolve("functions__exec"); !ok || b != (binding{Namespace: "functions", Name: "exec", Custom: true}) {
		t.Errorf("functions__exec = %+v", b)
	}
	if b, ok := tr.registry.resolve("exec"); !ok || b != (binding{Name: "exec"}) {
		t.Errorf("exec = %+v", b)
	}
	long := sanitize("ns__" + strings.Repeat("a", 100))
	if len(long) != 64 || !strings.HasPrefix(long, "ns__aaaa") {
		t.Errorf("long = %q (%d)", long, len(long))
	}
	if sanitize("ns__"+strings.Repeat("a", 99)) == long {
		t.Errorf("different long names collide")
	}
	choice := translateTools(jsonx.Arr(obj(`[{"type":"namespace","name":"ns","tools":[{"type":"function","name":"f"}]}]`)))
	if got := jsonx.String(choice.registry.toolChoice(obj(`{"type":"function","namespace":"ns","name":"f"}`))); got != `{"function":{"name":"ns__f"},"type":"function"}` {
		t.Errorf("tool choice = %s", got)
	}
	if got := choice.registry.toolChoice("required"); got != "required" {
		t.Errorf("required → %v", got)
	}
	if got := choice.registry.toolChoice(obj(`{"type":"allowed_tools","mode":"auto"}`)); got != "auto" {
		t.Errorf("allowed_tools auto → %v", got)
	}
}

func TestCleanStructuredOutput(t *testing.T) {
	for in, want := range map[string]string{
		`Sure: {"a":1} done`:                   `{"a":1}`,
		"not json":                             "not json",
		"```json\n{\"outcome\":\"deny\"}\n```": `{"outcome":"deny"}`,
		"  [1,2] ":                             "[1,2]",
	} {
		if got := cleanStructuredOutput(in); got != want {
			t.Errorf("clean(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestThinkFilterAcrossChunks(t *testing.T) {
	var f thinkFilter
	var reasoning, content string
	for _, c := range []string{"<thi", "nk>let me see</th", "ink>The answer"} {
		for _, s := range f.process(c) {
			if s.reasoning {
				reasoning += s.text
			} else {
				content += s.text
			}
		}
	}
	for _, s := range f.flush() {
		content += s.text
	}
	if reasoning != "let me see" || content != "The answer" {
		t.Errorf("reasoning %q content %q", reasoning, content)
	}
	// A lone leading </think> is dropped.
	f = thinkFilter{}
	var out string
	for _, c := range []string{" </thi", "nk>allow"} {
		for _, s := range f.process(c) {
			out += s.text
		}
	}
	if out != "allow" {
		t.Errorf("lone close → %q", out)
	}
	if r, c := stripThink("<think>hmm</think>done"); r != "hmm" || c != "done" {
		t.Errorf("strip = %q %q", r, c)
	}
}
