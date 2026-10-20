// Package apierr turns backend failures into the error shapes clients act
// on: Anthropic for Claude Code (docs/DESIGN.md §4.2), OpenAI for Codex and
// Chat Completions clients (§5, §5a). Backend messages are scrubbed of
// URLs, addresses and credentials.
package apierr

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"codelink/internal/jsonx"
)

// UpstreamInfo is what a backend error body says.
type UpstreamInfo struct {
	Message      string
	Type         string
	PromptTokens int
	ContextSize  int
}

// ParseUpstream reads a backend error body: OpenAI and vLLM
// {"error":{"message"}}, {"error":"…"}, FastAPI {"detail"}, {"message"} and
// Anthropic {"type":"error","error":{…}}.
func ParseUpstream(body []byte) UpstreamInfo {
	var info UpstreamInfo
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return info
	}
	j := jsonx.ParseObject(body)
	if j == nil {
		info.Message = trimmed
		return info
	}
	if e, ok := j["error"]; ok {
		if s, ok := e.(string); ok {
			info.Message = s
		} else if o := jsonx.Obj(e); o != nil {
			info.Message = jsonx.Str(o["message"])
			if info.Message == "" {
				info.Message = jsonx.Str(o["detail"])
			}
			info.Type = jsonx.Str(o["type"])
			info.PromptTokens, _ = jsonx.Int(o["n_prompt_tokens"])
			info.ContextSize, _ = jsonx.Int(o["n_ctx"])
		}
	}
	if info.Message == "" {
		for _, k := range []string{"message", "detail", "msg"} {
			v, ok := j[k]
			if !ok {
				continue
			}
			if s, ok := v.(string); ok && s != "" {
				info.Message = s
				break
			}
			if items := jsonx.Arr(v); len(items) > 0 {
				var parts []string
				for _, it := range items {
					if m := jsonx.Str(jsonx.Get(it, "msg")); m != "" {
						parts = append(parts, m)
					}
				}
				info.Message = strings.Join(parts, "; ")
				break
			}
		}
	}
	if info.Message == "" {
		info.Message = trimmed
	}
	return info
}

var (
	reURL    = regexp.MustCompile(`(?is)\b(?:https?|wss?|ftp)://[^\s"'<>)]+`)
	reHost   = regexp.MustCompile(`(?is)\b\d{1,3}(?:\.\d{1,3}){3}(?::\d{2,5})?\b`)
	reBearer = regexp.MustCompile(`(?is)\bbearer\s+[A-Za-z0-9._~+/=-]{8,}`)
	reKey    = regexp.MustCompile(`\b(?:sk|pk|rk|key|tok|tvly|ghp|xox[a-z])[-_][A-Za-z0-9_-]{8,}`)
	reKV     = regexp.MustCompile(`(?is)(api[_-]?key|authorization|secret|token|password)(["']?\s*[:=]\s*["']?)[^\s"',}]+`)

	reVLLMRequested = regexp.MustCompile(`(?is)maximum context length is (\d+) tokens.*?requested (\d+) tokens\s*\((\d+) in the messages,\s*(\d+) in the completion`)
	reVLLMPassed    = regexp.MustCompile(`(?is)you passed (\d+) input tokens and requested (\d+) output tokens.*?context length is only (\d+) tokens`)
	reVLLMTooLarge  = regexp.MustCompile(`(?is)too large: (\d+)\.\s*this model's maximum context length is (\d+) tokens and your request has (\d+) input tokens`)
	reSGLang        = regexp.MustCompile(`(?is)maximum context length of (\d+) tokens.*?requested a total of (\d+) tokens:\s*(\d+) tokens from the input.*?(\d+) tokens for the completion`)
	reInputLen      = regexp.MustCompile(`(?is)input length \((\d+)\) exceeds the maximum allowed length \((\d+)\)`)
	reGenericHint   = regexp.MustCompile(`(?is)(context length|context window|context size|too many tokens|prompt is too long|too long)`)
)

// Scrub removes URLs, IPv4 addresses and credential-looking strings from
// a backend message, keeping at most 1000 bytes.
func Scrub(s string) string {
	s = reURL.ReplaceAllString(s, "[url]")
	s = reBearer.ReplaceAllString(s, "Bearer [redacted]")
	s = reKV.ReplaceAllString(s, "$1$2[redacted]")
	s = reKey.ReplaceAllString(s, "[redacted]")
	s = reHost.ReplaceAllString(s, "[host]")
	s = strings.TrimSpace(s)
	if c, cut := Clip(s, 1000); cut {
		return c + "..."
	}
	return s
}

// Clip cuts s to at most max bytes on a character boundary; whether it cut.
func Clip(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	end := max
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end], true
}

func groups(re *regexp.Regexp, s string) []int {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return nil
	}
	out := make([]int, len(m)-1)
	for i := range out {
		out[i], _ = strconv.Atoi(m[i+1])
	}
	return out
}

// RewriteContextLength gives a context overflow in the form Claude Code
// parses to compact; "" when info isn't one.
func RewriteContextLength(info UpstreamInfo) string {
	msg := info.Message
	tooLong := func(n, m int) string { return "prompt is too long: " + strconv.Itoa(n) + " tokens > " + strconv.Itoa(m) + " maximum" }
	overflow := func(n, maxTok, m int) string {
		if n > m {
			return tooLong(n, m)
		}
		return "input length and `max_tokens` exceed context limit: " + strconv.Itoa(n) + " + " + strconv.Itoa(maxTok) + " > " + strconv.Itoa(m)
	}
	if info.PromptTokens > 0 && info.ContextSize > 0 {
		return tooLong(info.PromptTokens, info.ContextSize)
	}
	if g := groups(reVLLMRequested, msg); g != nil {
		return overflow(g[2], g[3], g[0])
	}
	if g := groups(reVLLMPassed, msg); g != nil {
		return overflow(g[0], g[1], g[2])
	}
	if g := groups(reVLLMTooLarge, msg); g != nil {
		return overflow(g[2], g[0], g[1])
	}
	if g := groups(reSGLang, msg); g != nil {
		return overflow(g[2], g[3], g[0])
	}
	if g := groups(reInputLen, msg); g != nil {
		return tooLong(g[0], g[1])
	}
	if info.Type == "exceed_context_size_error" ||
		(reGenericHint.MatchString(msg) && strings.Contains(strings.ToLower(msg), "context")) {
		return "prompt is too long: " + Scrub(msg)
	}
	return ""
}
