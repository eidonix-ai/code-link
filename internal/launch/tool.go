// Package launch renders the Claude Code and Codex commands of
// docs/DESIGN.md §3, finds the CLIs, and opens them in a terminal: iTerm or
// Terminal on macOS, Windows Terminal or PowerShell on Windows. Only the
// launched process gets the environment; ~/.claude and ~/.codex are never
// written.
package launch

// Tool is a coding tool code-link launches.
type Tool string

const (
	ClaudeCode Tool = "claude"
	Codex      Tool = "codex"
)

// Tools in menu order.
var Tools = []Tool{ClaudeCode, Codex}

func (t Tool) DisplayName() string {
	if t == Codex {
		return "Codex"
	}
	return "Claude Code"
}

// Executable is the command name looked up on PATH.
func (t Tool) Executable() string {
	if t == Codex {
		return "codex"
	}
	return "claude"
}

// InstallURL is where to get the tool when it is missing.
func (t Tool) InstallURL() string {
	if t == Codex {
		return "https://github.com/openai/codex"
	}
	return "https://docs.anthropic.com/en/docs/claude-code/setup"
}
