// Package thinktag separates <think>…</think> reasoning that some backends
// put in the content field from the answer.
package thinktag

import (
	"strings"
	"unicode"
)

// Filter separates <think>…</think> reasoning that some backends put
// in the content field from the answer, across chunk boundaries. A lone
// leading </think> (GLM templates with thinking off) is dropped, so an
// answer that must start with a verdict does.
type Filter struct {
	inside  bool
	pending string
	started bool
}

// Segment is a run of answer text, or of reasoning.
type Segment struct {
	Text      string
	Reasoning bool
}

const (
	thinkOpen  = "<think>"
	thinkClose = "</think>"
)

func trimLeftSpace(s string) string { return strings.TrimLeftFunc(s, unicode.IsSpace) }

// Process takes the next chunk of content.
func (f *Filter) Process(chunk string) []Segment {
	input := f.pending + chunk
	f.pending = ""
	var out []Segment
	for input != "" {
		if !f.inside && !f.started {
			t := trimLeftSpace(input)
			if strings.HasPrefix(t, thinkClose) {
				input = t[len(thinkClose):]
				f.started = true
				continue
			}
			if t != "" && len(t) < len(thinkClose) && strings.HasPrefix(thinkClose, t) {
				f.pending = input
				break
			}
			if t != "" {
				f.started = true
			}
		}
		tag := thinkOpen
		if f.inside {
			tag = thinkClose
		}
		if i := strings.Index(input, tag); i >= 0 {
			if i > 0 {
				out = append(out, Segment{input[:i], f.inside})
			}
			input = input[i+len(tag):]
			f.inside = !f.inside
			if !f.inside {
				f.started = true
			}
			continue
		}
		if n := longestTagSuffix(input, tag); n > 0 {
			if keep := input[:len(input)-n]; keep != "" {
				out = append(out, Segment{keep, f.inside})
			}
			f.pending = input[len(input)-n:]
		} else {
			out = append(out, Segment{input, f.inside})
		}
		input = ""
	}
	return out
}

// flush gives the text held back waiting for a tag to complete.
// Flush gives what is still held back at the end of the stream.
func (f *Filter) Flush() []Segment {
	if f.pending == "" {
		return nil
	}
	s := []Segment{{f.pending, f.inside}}
	f.pending = ""
	return s
}

// longestTagSuffix: the longest proper prefix of tag that s ends with. Tags
// are ASCII, so a byte match is on a character boundary.
func longestTagSuffix(s, tag string) int {
	for l := min(len(tag)-1, len(s)); l > 0; l-- {
		if strings.HasSuffix(s, tag[:l]) {
			return l
		}
	}
	return 0
}

// stripThink is (reasoning, content) of a complete string.
func Strip(s string) (reasoning, content string) {
	if t := trimLeftSpace(s); strings.HasPrefix(t, thinkClose) {
		s = t[len(thinkClose):]
	}
	var r, c strings.Builder
	for s != "" {
		o := strings.Index(s, thinkOpen)
		if o < 0 {
			c.WriteString(s)
			break
		}
		c.WriteString(s[:o])
		s = s[o+len(thinkOpen):]
		e := strings.Index(s, thinkClose)
		if e < 0 {
			r.WriteString(s)
			break
		}
		r.WriteString(s[:e])
		s = s[e+len(thinkClose):]
	}
	return r.String(), strings.TrimSpace(c.String())
}
