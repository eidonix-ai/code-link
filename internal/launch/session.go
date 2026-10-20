package launch

import "strings"

// Session is a launch's script content: the command, what to say when the
// tool exits (the window stays open in a shell still connected to
// code-link), and for Codex a shell function so `codex resume` in that
// window keeps code-link's -c overrides (Claude Code needs none: its
// settings are environment variables, which the shell keeps).
type Session struct {
	Command  string
	After    string
	Function string
}

// NewSession renders a launch for the native shell sh.
func NewSession(t Tool, c Context, sh Shell) (Session, error) {
	inv, err := Build(t, c)
	if err != nil {
		return Session{}, err
	}
	s := Session{}
	if sh == PowerShell {
		s.Command = renderPowerShell(t, inv)
	} else {
		s.Command = renderPOSIX(t, inv)
	}
	switch t {
	case Codex:
		s.After = "Codex has ended. This window is still connected to code-link: run \"codex resume --last\" to pick up where you left off."
		s.Function = codexFunction(inv, sh)
	default:
		s.After = "Claude Code has ended. This window is still connected to code-link: run \"claude --continue\" to pick up where you left off."
	}
	return s, nil
}

// codexFunction wraps codex with this launch's arguments, for the shell the
// window is left in.
func codexFunction(inv Invocation, sh Shell) string {
	if sh == PowerShell {
		args := make([]string, len(inv.Args))
		for i, a := range inv.Args {
			args[i] = PSQuote(a)
		}
		return "function global:codex { $c = Get-Command codex -CommandType Application,ExternalScript | Select-Object -First 1; & $c.Source " +
			strings.Join(args, " ") + " @args }"
	}
	args := make([]string, len(inv.Args))
	for i, a := range inv.Args {
		args[i] = QuoteIfNeeded(a)
	}
	return "codex() { command codex " + strings.Join(args, " ") + ` "$@"; }`
}
