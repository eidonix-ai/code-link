package launch

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Found is where a CLI was found.
type Found struct {
	// Path is the executable or shim; "" when the CLI was found by the login
	// shell only (an alias or function).
	Path string
	// PathDir goes first on PATH in the launch script: the CLI was found
	// outside the PATH the launched shell will have. "" = nothing to add.
	PathDir string
}

// SearchEnv is what a search looks at; tests fill it in for either OS.
type SearchEnv struct {
	GOOS string
	// Paths are PATH lists to search, in order (on Windows the process PATH,
	// then the user's and the machine's from the registry, which are fresh
	// when the CLI was installed after code-link started).
	Paths        []string
	Home         string
	AppData      string // %AppData% (Roaming)
	LocalAppData string // %LocalAppData%
	Exists       func(string) bool
	// Glob lists matches of a pattern (nvm versions); nil = none.
	Glob func(string) []string
}

func (e SearchEnv) join(parts ...string) string {
	if e.GOOS == "windows" {
		var out []string
		for _, p := range parts {
			if p == "" {
				continue
			}
			if len(out) > 0 {
				p = strings.Trim(p, `\/`)
			} else {
				p = strings.TrimRight(p, `\/`)
			}
			out = append(out, p)
		}
		return strings.Join(out, `\`)
	}
	return filepath.Join(parts...)
}

func (e SearchEnv) dir(p string) string {
	if e.GOOS == "windows" {
		if i := strings.LastIndexAny(p, `\/`); i >= 0 {
			return p[:i]
		}
		return ""
	}
	return filepath.Dir(p)
}

// extensions a command may have on PATH: PATHEXT's usual ones, then .ps1
// (npm installs claude.ps1 and codex.ps1 next to the .cmd shims).
func (e SearchEnv) extensions() []string {
	if e.GOOS == "windows" {
		return []string{".exe", ".cmd", ".bat", ".ps1"}
	}
	return []string{""}
}

// pathDirs are the entries of PATH lists, in order, without duplicates.
func (e SearchEnv) pathDirs() []string {
	sep := ":"
	if e.GOOS == "windows" {
		sep = ";"
	}
	seen := map[string]bool{}
	var out []string
	for _, list := range e.Paths {
		for _, d := range strings.Split(list, sep) {
			d = strings.TrimSpace(strings.Trim(d, `"`))
			key := d
			if e.GOOS == "windows" {
				key = strings.ToLower(d)
			}
			if d == "" || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, d)
		}
	}
	return out
}

// InstallLocations are where the installers put a tool, outside PATH
// lookups: the native installers, npm, pnpm, bun, Volta, Homebrew, WinGet,
// Scoop and Cargo.
func InstallLocations(t Tool, e SearchEnv) []string {
	name := t.Executable()
	var out []string
	if e.GOOS == "windows" {
		add := func(base string, parts ...string) {
			if base != "" {
				out = append(out, e.join(append([]string{base}, parts...)...))
			}
		}
		add(e.Home, ".local", "bin", name+".exe")
		add(e.AppData, "npm", name+".cmd")
		add(e.AppData, "npm", name+".ps1")
		add(e.LocalAppData, "Microsoft", "WinGet", "Links", name+".exe")
		add(e.LocalAppData, "pnpm", name+".cmd")
		add(e.Home, "scoop", "shims", name+".exe")
		add(e.Home, ".bun", "bin", name+".exe")
		add(e.LocalAppData, "Volta", "bin", name+".exe")
		if t == Codex {
			add(e.Home, ".cargo", "bin", name+".exe")
		}
		return out
	}
	if e.Home != "" {
		for _, d := range [][]string{{".local", "bin"}, {".claude", "local"}, {".npm-global", "bin"}, {".bun", "bin"}, {".volta", "bin"}, {"Library", "pnpm"}, {".cargo", "bin"}} {
			if d[0] == ".claude" && t != ClaudeCode || d[0] == ".cargo" && t != Codex {
				continue
			}
			out = append(out, e.join(append([]string{e.Home}, append(d, name)...)...))
		}
	}
	out = append(out, "/opt/homebrew/bin/"+name, "/usr/local/bin/"+name)
	if e.Home != "" && e.Glob != nil {
		matches := e.Glob(e.join(e.Home, ".nvm", "versions", "node", "*", "bin", name))
		sort.Sort(sort.Reverse(sort.StringSlice(matches))) // newest version first, roughly
		out = append(out, matches...)
	}
	return out
}

// Candidates are every place to look, in order: PATH, then the install
// locations.
func Candidates(t Tool, e SearchEnv) []string {
	var out []string
	for _, d := range e.pathDirs() {
		for _, ext := range e.extensions() {
			out = append(out, e.join(d, t.Executable()+ext))
		}
	}
	return append(out, InstallLocations(t, e)...)
}

// Search finds a tool by its files; the found folder becomes PathDir.
func Search(t Tool, e SearchEnv) (Found, bool) {
	for _, p := range Candidates(t, e) {
		if e.Exists(p) {
			return Found{Path: p, PathDir: e.dir(p)}, true
		}
	}
	return Found{}, false
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func regularFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
