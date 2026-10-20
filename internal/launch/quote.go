package launch

import "strings"

// Quote is POSIX single quoting: it's becomes 'it'"'"'s'.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

const shellSafe = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_@%+=:,./-"

// QuoteIfNeeded leaves a value bare when every character is shell-safe.
func QuoteIfNeeded(s string) string {
	if s != "" && strings.Trim(s, shellSafe) == "" {
		return s
	}
	return Quote(s)
}

// PSQuote is a PowerShell single-quoted string: it's becomes 'it”s'.
func PSQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// TOMLString is a TOML basic string, for Codex -c key="value" overrides.
func TOMLString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// psQuoteIfNeeded leaves a plain word (letters, digits, - _ .) bare.
func psQuoteIfNeeded(s string) string {
	if s != "" && strings.Trim(s, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.") == "" {
		return s
	}
	return PSQuote(s)
}
