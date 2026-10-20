package app

import (
	"fmt"
	"sync"
	"time"

	"codelink/internal/hooks"
)

// Entry is one line of Recent activity. Never prompt text.
type Entry struct {
	ID    int       `json:"id"`
	Time  time.Time `json:"time"`
	Kind  string    `json:"kind"`
	Text  string    `json:"text"`
	Level string    `json:"level"` // normal, warning, error
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// Describe words an engine event.
func Describe(e hooks.Event) (kind, text, level string) {
	switch e.Kind {
	case "request":
		level = "normal"
		if e.Status >= 500 {
			level = "error"
		} else if e.Status >= 400 {
			level = "warning"
		}
		return "request", fmt.Sprintf("%s · %s · %d · %d ms", e.Route, e.Model, e.Status, e.LatencyMS), level
	case "decision":
		text = fmt.Sprintf("auto mode stage %d · %s · p(block) %.2f · severity %d · %d ms", e.Stage, e.Model, e.PBlock, e.Severity, e.LatencyMS)
		if e.Fallback != "" {
			return "decision", text + " · fallback: " + e.Fallback, "warning"
		}
		return "decision", text, "normal"
	case "websearch":
		return "search", fmt.Sprintf("web search %q · %s", e.Query, plural(e.Results, "result")), "normal"
	case "document":
		text = e.Name + " · " + plural(e.Pages, "page")
		if e.OCRPages > 0 {
			text += fmt.Sprintf(" · %d read by OCR", e.OCRPages)
		}
		return "document", text, "normal"
	case "error":
		return "error", e.Message, "error"
	}
	if e.Message != "" {
		return e.Kind, e.Message, "normal"
	}
	return e.Kind, e.Kind, "normal"
}

// ActivityLimit caps the list.
const ActivityLimit = 200

// Activity is the Recent activity list, newest first; it is the engine's
// hooks.EventLog and safe from any goroutine.
type Activity struct {
	mu      sync.Mutex
	entries []Entry
	counter int
	// OnAppend is called (outside the lock) with each new entry.
	OnAppend func(Entry)
	now      func() time.Time
}

func (a *Activity) Record(e hooks.Event) {
	kind, text, level := Describe(e)
	a.mu.Lock()
	a.counter++
	now := time.Now
	if a.now != nil {
		now = a.now
	}
	entry := Entry{ID: a.counter, Time: now(), Kind: kind, Text: text, Level: level}
	a.entries = append([]Entry{entry}, a.entries...)
	if len(a.entries) > ActivityLimit {
		a.entries = a.entries[:ActivityLimit]
	}
	notify := a.OnAppend
	a.mu.Unlock()
	if notify != nil {
		notify(entry)
	}
}

// Entries are the lines, newest first.
func (a *Activity) Entries() []Entry {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Entry{}, a.entries...)
}

func (a *Activity) Clear() {
	a.mu.Lock()
	a.entries = nil
	a.mu.Unlock()
}
