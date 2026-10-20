package tools

import (
	"regexp"
	"strconv"
	"strings"
)

var versionRE = regexp.MustCompile(`\d+(?:\.\d+)+(?:-[0-9A-Za-z.-]+)?`)

// ParseVersion finds the version in a `--version` line or a release tag:
// "2.1.296 (Claude Code)" → "2.1.296", "codex-cli 0.163.0" → "0.163.0",
// "rust-v0.163.0" → "0.163.0". "" when there is none.
func ParseVersion(s string) string {
	return versionRE.FindString(s)
}

// CompareVersions compares dotted numeric versions: -1, 0 or 1. Missing
// parts count as 0 (1.2 == 1.2.0); a pre-release sorts before its release
// (0.164.0-alpha.7 < 0.164.0).
func CompareVersions(a, b string) int {
	an, ap, _ := strings.Cut(strings.TrimPrefix(a, "v"), "-")
	bn, bp, _ := strings.Cut(strings.TrimPrefix(b, "v"), "-")
	if c := compareParts(strings.Split(an, "."), strings.Split(bn, "."), true); c != 0 {
		return c
	}
	switch {
	case ap == bp:
		return 0
	case ap == "":
		return 1
	case bp == "":
		return -1
	}
	return compareParts(strings.Split(ap, "."), strings.Split(bp, "."), false)
}

// compareParts compares dot-separated parts: numbers numerically, words
// alphabetically, a number before a word (semver's rule). pad: a missing
// part is 0, else the shorter list sorts first.
func compareParts(a, b []string, pad bool) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		if !pad && (i >= len(a) || i >= len(b)) {
			if len(a) < len(b) {
				return -1
			}
			return 1
		}
		x, y := "0", "0"
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		xn, xerr := strconv.ParseUint(x, 10, 64)
		yn, yerr := strconv.ParseUint(y, 10, 64)
		switch {
		case xerr == nil && yerr == nil:
			if xn != yn {
				if xn < yn {
					return -1
				}
				return 1
			}
		case xerr == nil:
			return -1
		case yerr == nil:
			return 1
		default:
			if c := strings.Compare(x, y); c != 0 {
				return c
			}
		}
	}
	return 0
}
