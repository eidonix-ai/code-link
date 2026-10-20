package responses

import (
	"fmt"
	"strings"

	"codelink/internal/apierr"
	"codelink/internal/jsonx"
)

// binding is what a chat function name presented to the model stands for.
type binding struct {
	Namespace string // "" for a top-level tool
	Name      string // the tool's own name inside its namespace
	Custom    bool   // freeform tool (one `input` string) rather than JSON arguments
}

// registry maps between Responses tools and the chat functions the model
// sees, both ways (docs/DESIGN.md §5).
//
// Flattening rule (deterministic, collision-safe):
//   - a top-level tool keeps its name;
//   - a tool inside namespace NS is `NS__NAME`, characters outside
//     `[A-Za-z0-9_-]` replaced by `_`, at most 64 characters (a longer name
//     is cut and ends in `_` + an 8-hex hash of the full name);
//   - the implicit namespace `functions` is dropped when the bare name is
//     free, so Codex's auto-review sees `exec`, not `functions__exec`;
//   - a name already taken (a top-level tool always wins) gets `_2`, `_3`, …
type registry struct {
	byFlat map[string]binding
	byKey  map[string]string
	byName map[string]string
}

func newRegistry() *registry {
	return &registry{byFlat: map[string]binding{}, byKey: map[string]string{}, byName: map[string]string{}}
}

func toolKey(ns, name string) string { return ns + "\x00" + name }

// flatName is the chat function name for a Responses (namespace, name). A
// tool the request does not define (history naming an old one) gets the
// name the flattening rule gives it.
func (r *registry) flatName(ns, name string) string {
	if flat, ok := r.byKey[toolKey(ns, name)]; ok {
		return flat
	}
	if ns == "" {
		if flat, ok := r.byName[name]; ok {
			return flat
		}
		return name
	}
	return sanitize(ns + "__" + name)
}

func (r *registry) resolve(flat string) (binding, bool) {
	b, ok := r.byFlat[flat]
	return b, ok
}

func (r *registry) isCustom(flat string) bool {
	b, ok := r.byFlat[flat]
	return ok && b.Custom
}

func sanitize(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
			b.WriteRune(c)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) > 64 {
		out = out[:55] + "_" + fmt.Sprintf("%08x", fnv32a(s))
	}
	return out
}

func fnv32a(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

// translatedTools is the chat side of a request's tools.
type translatedTools struct {
	tools []any
	// stripped are tool types with no chat equivalent (web_search, …).
	stripped []any
	registry *registry
}

// customParameters is the schema every freeform tool is given.
func customParameters() map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"input": map[string]any{"type": "string", "description": "The raw tool input, passed verbatim."}},
		"required":             []any{"input"},
		"additionalProperties": false,
	}
}

// translateTools gives chat tools for Responses tools: functions map one
// to one, namespaces are flattened, custom (freeform) tools become a
// function with one string parameter `input`, anything else is stripped.
func translateTools(tools []any) translatedTools {
	type def struct {
		ns   string
		tool map[string]any
	}
	var defs []def
	out := translatedTools{registry: newRegistry()}
	for _, t := range tools {
		switch jsonx.Type(t) {
		case "namespace":
			for _, sub := range jsonx.Arr(jsonx.Get(t, "tools")) {
				if ty := jsonx.Type(sub); ty == "function" || ty == "custom" {
					defs = append(defs, def{jsonx.Str(jsonx.Get(t, "name")), jsonx.Obj(sub)})
				}
			}
		case "function", "custom":
			defs = append(defs, def{"", jsonx.Obj(t)})
		default:
			out.stripped = append(out.stripped, t)
		}
	}
	// Top-level names are reserved first: they always keep their own.
	used := map[string]bool{}
	for _, d := range defs {
		if d.ns == "" {
			used[jsonx.Str(d.tool["name"])] = true
		}
	}
	reg := out.registry
	for _, d := range defs {
		name := jsonx.Str(d.tool["name"])
		custom := jsonx.Type(d.tool) == "custom"
		k := toolKey(d.ns, name)
		if _, dup := reg.byKey[k]; dup {
			continue
		}
		flat := name
		if d.ns != "" {
			flat = sanitize(d.ns + "__" + name)
			if d.ns == "functions" {
				if bare := sanitize(name); !used[bare] {
					flat = bare
				}
			}
			if used[flat] {
				base := flat
				for i := 2; used[flat]; i++ {
					flat = sanitize(fmt.Sprintf("%s_%d", base, i))
				}
			}
			used[flat] = true
		}
		reg.byKey[k] = flat
		reg.byFlat[flat] = binding{Namespace: d.ns, Name: name, Custom: custom}
		if _, ok := reg.byName[name]; !ok {
			reg.byName[name] = flat
		}

		fn := map[string]any{"name": flat}
		if custom {
			fn["description"] = customDescription(jsonx.Str(d.tool["description"]), d.tool["format"])
			fn["parameters"] = customParameters()
		} else {
			if desc := jsonx.Str(d.tool["description"]); desc != "" {
				fn["description"] = desc
			}
			if p, ok := d.tool["parameters"]; ok && p != nil {
				fn["parameters"] = p
			}
			if s, ok := jsonx.BoolOK(d.tool["strict"]); ok {
				fn["strict"] = s
			}
		}
		out.tools = append(out.tools, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// customDescription explains the freeform contract to the model and, for a
// grammar tool, appends the grammar (the backend cannot enforce it, the
// model can follow it).
func customDescription(desc string, format any) string {
	note := "This tool takes freeform text: pass it verbatim as the `input` string argument."
	if jsonx.Type(format) == "grammar" {
		if def := strings.TrimSpace(jsonx.Str(jsonx.Get(format, "definition"))); def != "" {
			clipped, _ := apierr.Clip(def, 4000)
			note += "\nThe input must match this " + jsonx.Str(jsonx.Get(format, "syntax")) + " grammar:\n" + clipped
		}
	}
	if desc == "" {
		return note
	}
	return desc + "\n\n" + note
}

// toolChoice is a Responses tool_choice as a chat one: strings pass
// through, {type: function|custom, name} gets the flattened name,
// allowed_tools collapses to its mode, anything else is "auto".
func (r *registry) toolChoice(tc any) any {
	if s, ok := tc.(string); ok {
		return s
	}
	switch jsonx.Type(tc) {
	case "function", "custom":
		name := jsonx.Str(jsonx.Get(tc, "name"))
		if name == "" {
			name = jsonx.Str(jsonx.Get(tc, "function", "name"))
		}
		if name == "" {
			return "auto"
		}
		return map[string]any{"type": "function", "function": map[string]any{"name": r.flatName(jsonx.Str(jsonx.Get(tc, "namespace")), name)}}
	case "allowed_tools":
		if jsonx.Str(jsonx.Get(tc, "mode")) == "required" {
			return "required"
		}
	}
	return "auto"
}

// customInput is the freeform text of a custom tool call from the
// arguments the model wrote ({"input": "…"}). Anything of another shape
// passes as it is, so no model output is lost.
func customInput(args string) string {
	if j, err := jsonx.Parse([]byte(args)); err == nil {
		switch v := j.(type) {
		case map[string]any:
			if s, ok := v["input"].(string); ok {
				return s
			}
			return args
		case string:
			return v
		}
	}
	return args
}

// outputItem is the Responses output item for a chat tool call: a
// function_call (with namespace when the tool lives in one) or, for a
// freeform tool, a custom_tool_call. inProgress items carry no arguments yet.
func (r *registry) outputItem(itemID, callID, flatName, args string, inProgress bool) map[string]any {
	status := "completed"
	if inProgress {
		status = "in_progress"
	}
	b, ok := r.resolve(flatName)
	if ok && b.Custom {
		input := ""
		if !inProgress {
			input = customInput(args)
		}
		item := map[string]any{"id": itemID, "type": "custom_tool_call", "call_id": callID, "name": b.Name, "input": input, "status": status}
		if b.Namespace != "" {
			item["namespace"] = b.Namespace
		}
		return item
	}
	item := map[string]any{"id": itemID, "type": "function_call", "call_id": callID, "arguments": args, "status": status, "name": flatName}
	if ok {
		item["name"] = b.Name
		if b.Namespace != "" {
			item["namespace"] = b.Namespace
		}
	}
	return item
}
