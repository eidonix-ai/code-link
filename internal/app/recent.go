package app

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// RecentLimit is how many launch folders are remembered.
const RecentLimit = 8

// NormalizeFolder cleans a folder path (no trailing separator).
func NormalizeFolder(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	return filepath.Clean(p)
}

func sameFolder(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// AddRecent puts a folder first, without duplicates, capped.
func AddRecent(list []string, p string) []string {
	p = NormalizeFolder(p)
	if p == "" {
		return list
	}
	out := []string{p}
	for _, x := range list {
		if x = NormalizeFolder(x); x != "" && !sameFolder(x, p) && len(out) < RecentLimit {
			out = append(out, x)
		}
	}
	return out
}

// ExistingFolders drops folders that no longer exist.
func ExistingFolders(list []string, isDir func(string) bool) []string {
	if isDir == nil {
		isDir = func(p string) bool { st, err := os.Stat(p); return err == nil && st.IsDir() }
	}
	out := []string{}
	for _, p := range list {
		if isDir(p) {
			out = append(out, p)
		}
	}
	return out
}

// FolderTitle is "folder — ~/path/to" for menus.
func FolderTitle(p, home string) string {
	shown := p
	if home != "" {
		if p == home {
			shown = "~"
		} else if rest, ok := strings.CutPrefix(p, home+string(filepath.Separator)); ok {
			shown = "~" + string(filepath.Separator) + rest
		}
	}
	name := filepath.Base(p)
	if name == "" || name == "." || name == string(filepath.Separator) {
		return shown
	}
	return name + " — " + shown
}
