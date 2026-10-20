package tools

import (
	"regexp"
	"strings"
)

// PathRegistry is the user's own PATH (Windows: HKCU\Environment\Path).
type PathRegistry interface {
	// UserPath is the value and whether it is REG_EXPAND_SZ; "" and true
	// when there is none yet.
	UserPath() (value string, expand bool, err error)
	SetUserPath(value string, expand bool) error
	// Expand expands %VARIABLES% the way Windows does.
	Expand(s string) string
	// Broadcast tells running programs (Explorer, so new terminals) that
	// the environment changed: WM_SETTINGCHANGE "Environment".
	Broadcast()
}

// addToUserPath puts dir at the end of the user's PATH unless it is already
// there (in any spelling: case, a trailing backslash, quotes, %VARIABLES%).
// It reports whether it changed anything.
func addToUserPath(r PathRegistry, dir string) (bool, error) {
	cur, expand, err := r.UserPath()
	if err != nil {
		return false, err
	}
	if pathListHas(cur, dir, r.Expand) {
		return false, nil
	}
	next := strings.TrimRight(cur, ";")
	if next != "" {
		next += ";"
	}
	next += dir
	if err := r.SetUserPath(next, expand); err != nil {
		return false, err
	}
	r.Broadcast()
	return true, nil
}

func pathListHas(list, dir string, expand func(string) string) bool {
	want := pathKey(dir, expand)
	for _, e := range strings.Split(list, ";") {
		if pathKey(e, expand) == want && want != "" {
			return true
		}
	}
	return false
}

func pathKey(p string, expand func(string) string) string {
	p = strings.TrimSpace(strings.Trim(strings.TrimSpace(p), `"`))
	if expand != nil {
		p = expand(p)
	}
	return strings.ToLower(strings.TrimRight(strings.ReplaceAll(p, "/", `\`), `\`))
}

var winVar = regexp.MustCompile(`%([^%;]+)%`)

// expandWindowsVars expands %NAME% with lookup (names are
// case-insensitive on Windows; lookup handles that); unknown names stay.
func expandWindowsVars(s string, lookup func(string) (string, bool)) string {
	return winVar.ReplaceAllStringFunc(s, func(m string) string {
		if v, ok := lookup(m[1 : len(m)-1]); ok {
			return v
		}
		return m
	})
}
