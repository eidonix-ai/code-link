// Package jsonx is the engine's working JSON: requests are decoded into
// plain values (map[string]any, []any, string, bool, nil and json.Number,
// so numbers keep their spelling), edited, and written back without HTML
// escaping. Go writes object keys sorted, so the same value always gives
// the same bytes, which keeps backend prefix caches warm across turns.
package jsonx

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// Object is a JSON object.
type Object = map[string]any

// Parse decodes one JSON value, keeping numbers as json.Number.
func Parse(data []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	if d.More() {
		return nil, &json.SyntaxError{Offset: d.InputOffset()}
	}
	return v, nil
}

// ParseObject decodes a JSON object; nil when data isn't one.
func ParseObject(data []byte) Object {
	v, err := Parse(data)
	if err != nil {
		return nil
	}
	o, _ := v.(Object)
	return o
}

// Marshal writes v without HTML escaping and without a trailing newline.
func Marshal(v any) []byte {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return []byte("null")
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

// String is Marshal as a string.
func String(v any) string { return string(Marshal(v)) }

// Obj is v as an object, or nil.
func Obj(v any) Object { o, _ := v.(Object); return o }

// Arr is v as an array, or nil.
func Arr(v any) []any { a, _ := v.([]any); return a }

// Str is v as a string, or "".
func Str(v any) string { s, _ := v.(string); return s }

// StrOK is v as a string and whether it was one.
func StrOK(v any) (string, bool) { s, ok := v.(string); return s, ok }

// Bool is v as a bool, or false.
func Bool(v any) bool { b, _ := v.(bool); return b }

// BoolOK is v as a bool and whether it was one.
func BoolOK(v any) (bool, bool) { b, ok := v.(bool); return b, ok }

// Int is v as an integer (json.Number, float64 or int), and whether it was one.
func Int(v any) (int, bool) {
	switch n := v.(type) {
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i), true
		}
		if f, err := n.Float64(); err == nil && f == float64(int64(f)) {
			return int(f), true
		}
	case float64:
		if n == float64(int64(n)) {
			return int(n), true
		}
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}

// Float is v as a float64, and whether it was a number.
func Float(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// Num is a JSON number written as given.
func Num(n int) json.Number { return json.Number(strconv.Itoa(n)) }

// Get walks objects by key and arrays by index: Get(v, "choices", 0, "message").
func Get(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			o, ok := v.(Object)
			if !ok {
				return nil
			}
			v = o[k]
		case int:
			a, ok := v.([]any)
			if !ok || k < 0 || k >= len(a) {
				return nil
			}
			v = a[k]
		default:
			return nil
		}
	}
	return v
}

// Type is an object's "type" field.
func Type(v any) string { return Str(Get(v, "type")) }

// Clone deep-copies a decoded value.
func Clone(v any) any {
	switch t := v.(type) {
	case Object:
		o := make(Object, len(t))
		for k, x := range t {
			o[k] = Clone(x)
		}
		return o
	case []any:
		a := make([]any, len(t))
		for i, x := range t {
			a[i] = Clone(x)
		}
		return a
	default:
		return v
	}
}
