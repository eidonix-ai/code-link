// Package trayicon picks the tray icon for a status: a monochrome template
// glyph on macOS (the menu bar tints it), a light and a dark glyph on
// Windows; dimmed when stopped or not configured, with a dot for trouble
// (red: backend unreachable or failed).
package trayicon

import (
	"sync"

	"codelink/internal/app"
	"codelink/internal/icon"
)

// Icons for one status.
type Icons struct {
	// Template is the macOS menu bar image.
	Template []byte
	// Light and Dark are for Windows' light and dark taskbars.
	Light, Dark []byte
}

func badge(k app.StatusKind) icon.Badge {
	switch k {
	case app.BackendUnreachable, app.Failed:
		return icon.Error
	}
	return icon.NoBadge
}

func alpha(k app.StatusKind) float64 {
	if k == app.Stopped || k == app.NotConfigured {
		return 0.45
	}
	return 1
}

var (
	mu    sync.Mutex
	cache = map[app.StatusKind]Icons{}
)

// For is the icons for a status kind.
func For(k app.StatusKind) Icons {
	mu.Lock()
	defer mu.Unlock()
	if ic, ok := cache[k]; ok {
		return ic
	}
	b, a := badge(k), alpha(k)
	ic := Icons{
		// The menu bar sizes the image to its thickness (~22 pt): 44 px for
		// Retina, the glyph in the middle two thirds.
		Template: icon.Tray(44, icon.Black, a, 0.62, b, icon.Black),
		Light:    icon.Tray(64, icon.Ink, a, 0.92, b, icon.Red),
		Dark:     icon.Tray(64, icon.White, a, 0.92, b, icon.Red),
	}
	cache[k] = ic
	return ic
}

// All are every icon by name, for a preview.
func All() map[string][]byte {
	out := map[string][]byte{}
	for _, k := range []app.StatusKind{app.Running, app.Stopped, app.BackendUnreachable} {
		ic := For(k)
		out[string(k)+"-mac"] = ic.Template
		out[string(k)+"-win-light"] = ic.Light
		out[string(k)+"-win-dark"] = ic.Dark
	}
	return out
}
