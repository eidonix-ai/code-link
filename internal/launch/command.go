package launch

import (
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"

	"codelink/internal/config"
)

// ErrNoMainModel: there is nothing to launch with yet.
var ErrNoMainModel = errors.New("no main model is configured. Add a provider and pick a model in Settings first")

// Context is everything a launch command depends on.
type Context struct {
	Config *config.Config
	// Port is the port the engine is actually listening on.
	Port  int
	Token string
	// ClaudeSettingsPath is the --settings file (see ClaudeSettings).
	ClaudeSettingsPath string
	// PermissionMode for Claude Code; "" = auto.
	PermissionMode string
	// Model is this session's model (the launch window's choice); "" = the
	// configured roles. With a Model, Heavy (Opus / Fable) and Background
	// (Haiku, small/fast) default to it.
	Model, Heavy, Background string
}

// Shell is the language a command is written in.
type Shell int

const (
	POSIX      Shell = iota // bash / zsh: macOS
	PowerShell              // Windows
)

// NativeShell is the shell of the platform code-link runs on.
func NativeShell() Shell { return ShellFor(runtime.GOOS) }

// ShellFor is the shell of a GOOS.
func ShellFor(goos string) Shell {
	if goos == "windows" {
		return PowerShell
	}
	return POSIX
}

// Language names a shell for a fenced code block.
func (s Shell) Language() string {
	if s == PowerShell {
		return "powershell"
	}
	return "bash"
}

// Var is one environment variable.
type Var struct{ Name, Value string }

// Invocation is a tool's environment and argument list.
type Invocation struct {
	Env  []Var
	Name string // the executable: claude or codex
	Args []string
}

// CodexProviderID and CodexTokenVariable name code-link's Codex provider.
const (
	CodexProviderID    = "code-link"
	CodexTokenVariable = "CODE_LINK_TOKEN"
)

// Build is the environment and arguments for a tool (docs/DESIGN.md §3).
func Build(t Tool, c Context) (Invocation, error) {
	if c.Config == nil || (c.Config.Roles.Main == "" && c.Model == "") {
		return Invocation{}, ErrNoMainModel
	}
	if t == Codex {
		return codex(c), nil
	}
	return claude(c), nil
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func claude(c Context) Invocation {
	r := c.Config.Roles
	main, opus, small := r.Main, or(r.Opus, r.Main), or(r.Small, r.Main)
	if c.Model != "" {
		main, opus, small = c.Model, or(c.Heavy, c.Model), or(c.Background, c.Model)
	}
	env := []Var{
		{"ANTHROPIC_BASE_URL", "http://127.0.0.1:" + strconv.Itoa(c.Port)},
		{"ANTHROPIC_AUTH_TOKEN", c.Token},
		{"ANTHROPIC_MODEL", main},
		{"ANTHROPIC_DEFAULT_SONNET_MODEL", main},
		{"ANTHROPIC_DEFAULT_SONNET_MODEL_NAME", main},
		{"ANTHROPIC_DEFAULT_OPUS_MODEL", opus},
		{"ANTHROPIC_DEFAULT_OPUS_MODEL_NAME", opus},
		// Fable is the tier above Opus; without these /model offers Anthropic's own.
		{"ANTHROPIC_DEFAULT_FABLE_MODEL", opus},
		{"ANTHROPIC_DEFAULT_FABLE_MODEL_NAME", opus},
		{"ANTHROPIC_DEFAULT_HAIKU_MODEL", small},
		{"ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME", small},
		{"ANTHROPIC_SMALL_FAST_MODEL", small},
		{"CLAUDE_CODE_AUTO_MODE_SERVER", "0"},
		{"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1"},
		{"API_TIMEOUT_MS", "900000"},
		{"CLAUDE_CODE_ATTRIBUTION_HEADER", "0"},
	}
	if m, ok := c.Config.Model(main); ok {
		if m.ContextWindow != nil && *m.ContextWindow > 0 {
			env = append(env, Var{"CLAUDE_CODE_MAX_CONTEXT_TOKENS", strconv.Itoa(*m.ContextWindow)})
		}
		if n := m.MaxOutputTokens(); n > 0 {
			env = append(env, Var{"CLAUDE_CODE_MAX_OUTPUT_TOKENS", strconv.Itoa(n)})
		}
		if n := m.EffectiveAutoCompactAt(); n > 0 {
			env = append(env, Var{"CLAUDE_CODE_AUTO_COMPACT_WINDOW", strconv.Itoa(n)})
		}
	}
	return Invocation{Env: env, Name: "claude",
		Args: []string{"--permission-mode", or(c.PermissionMode, "auto"), "--settings", c.ClaudeSettingsPath}}
}

func codex(c Context) Invocation {
	model := or(c.Model, or(c.Config.Roles.Codex, c.Config.Roles.Main))
	id := CodexProviderID
	settings := []string{
		"model=" + TOMLString(model),
		"model_provider=" + TOMLString(id),
	}
	if m, ok := c.Config.Model(model); ok && m.ContextWindow != nil && *m.ContextWindow > 0 {
		settings = append(settings, "model_context_window="+strconv.Itoa(*m.ContextWindow))
	}
	settings = append(settings,
		"model_providers."+id+".name="+TOMLString(id),
		"model_providers."+id+".base_url="+TOMLString(fmt.Sprintf("http://127.0.0.1:%d/v1", c.Port)),
		"model_providers."+id+".wire_api="+TOMLString("responses"),
		"model_providers."+id+".env_key="+TOMLString(CodexTokenVariable),
	)
	args := []string{"--approve-for-me"}
	for _, s := range settings {
		args = append(args, "-c", s)
	}
	return Invocation{Env: []Var{{CodexTokenVariable, c.Token}}, Name: "codex", Args: args}
}

// Render is the command as text in a shell: what "Copy command" puts on the
// clipboard and what the launch script runs.
func Render(t Tool, c Context, sh Shell) (string, error) {
	inv, err := Build(t, c)
	if err != nil {
		return "", err
	}
	if sh == PowerShell {
		return renderPowerShell(t, inv), nil
	}
	return renderPOSIX(t, inv), nil
}

// renderPOSIX: export lines, then the command. The launch window's shell
// keeps them after the tool exits, so `claude --continue` there resumes on
// code-link. Claude Code's arguments fit on one line; Codex's -c overrides
// get a line each.
func renderPOSIX(t Tool, inv Invocation) string {
	var lines []string
	for _, v := range inv.Env {
		lines = append(lines, "export "+v.Name+"="+QuoteIfNeeded(v.Value))
	}
	if t == Codex {
		lines = append(lines, inv.Name+" "+inv.Args[0]+` \`)
		rest := inv.Args[1:]
		for i := 0; i+1 < len(rest); i += 2 {
			line := "  " + rest[i] + " " + Quote(rest[i+1])
			if i+2 < len(rest) {
				line += ` \`
			}
			lines = append(lines, line)
		}
		return strings.Join(lines, "\n")
	}
	lines = append(lines, inv.Name+" "+inv.Args[0]+" "+QuoteIfNeeded(inv.Args[1])+" "+inv.Args[2]+" "+Quote(inv.Args[3]))
	return strings.Join(lines, "\n")
}

// renderPowerShell: $env: assignments, then the command; Codex's overrides
// continue with backticks.
func renderPowerShell(t Tool, inv Invocation) string {
	var lines []string
	for _, v := range inv.Env {
		lines = append(lines, "$env:"+v.Name+" = "+PSQuote(v.Value))
	}
	if t == Codex {
		lines = append(lines, inv.Name+" "+inv.Args[0]+" `")
		rest := inv.Args[1:]
		for i := 0; i+1 < len(rest); i += 2 {
			line := "  " + rest[i] + " " + PSQuote(rest[i+1])
			if i+2 < len(rest) {
				line += " `"
			}
			lines = append(lines, line)
		}
		return strings.Join(lines, "\n")
	}
	lines = append(lines, inv.Name+" "+inv.Args[0]+" "+psQuoteIfNeeded(inv.Args[1])+" "+inv.Args[2]+" "+PSQuote(inv.Args[3]))
	return strings.Join(lines, "\n")
}
