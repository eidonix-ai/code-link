package launch

import "strings"

// Terminal choices (config.Launch.Terminal); "" = the platform's best.
const (
	TerminalAuto       = ""
	TerminalITerm      = "iterm"
	TerminalApple      = "terminal"
	TerminalWindows    = "wt"
	TerminalPowerShell = "powershell"
)

// Choice is an option for a settings picker.
type Choice struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// TerminalChoices are the terminals offered on a GOOS.
func TerminalChoices(goos string) []Choice {
	if goos == "windows" {
		return []Choice{
			{TerminalAuto, "Windows Terminal if installed, otherwise PowerShell"},
			{TerminalWindows, "Windows Terminal"},
			{TerminalPowerShell, "PowerShell"},
		}
	}
	return []Choice{
		{TerminalAuto, "iTerm if installed, otherwise Terminal"},
		{TerminalITerm, "iTerm"},
		{TerminalApple, "Terminal"},
	}
}

// PermissionModes are Claude Code's permission modes offered in Settings.
var PermissionModes = []Choice{
	{"auto", "Auto"},
	{"default", "Ask"},
	{"acceptEdits", "Accept edits"},
	{"plan", "Plan"},
	{"bypassPermissions", "Bypass permissions"},
}

// Opener opens a command in a terminal, in a folder.
type Opener struct {
	// Dir is where launch scripts go (LaunchDir).
	Dir string
	// Terminal is the preference (config.Launch.Terminal).
	Terminal string
}

// Open runs command (in the native shell's syntax) in a new terminal window
// or tab in folder. pathDir, when set, goes first on PATH.
func (o Opener) Open(s Session, folder, pathDir string) error {
	return o.open(s, folder, pathDir)
}

// --- macOS ---

// MacApp is a macOS terminal.
type MacApp int

const (
	ITerm MacApp = iota
	AppleTerminal
)

// MacPlan is one way of opening the script: the AppleScript and its argument.
type MacPlan struct {
	App         MacApp
	AppleScript string
	Argument    string
}

// UseITerm: iTerm takes the script path as the session command, so a path
// with spaces or quotes can't be used there.
func UseITerm(pref string, itermInstalled bool, scriptPath string) bool {
	if pref == TerminalApple || !itermInstalled {
		return false
	}
	return !strings.ContainsAny(scriptPath, " \t'\"")
}

// MacPlans are the plans to try in order: iTerm (when wanted, installed and
// the path is plain) with Terminal as the fallback. iTerm gets the bare
// path, Terminal the shell-quoted one.
func MacPlans(pref string, itermInstalled bool, scriptPath string) []MacPlan {
	var out []MacPlan
	if UseITerm(pref, itermInstalled, scriptPath) {
		out = append(out, MacPlan{ITerm, ITermScript, scriptPath})
	}
	return append(out, MacPlan{AppleTerminal, TerminalScript, Quote(scriptPath)})
}

// ITermScript opens a tab in iTerm's current window, or a new window.
const ITermScript = `on run argv
  tell application "iTerm2"
    activate
    if (count of windows) = 0 then
      create window with default profile command (item 1 of argv)
    else
      tell current window to create tab with default profile command (item 1 of argv)
    end if
  end tell
end run`

// TerminalScript opens a new Terminal window running the script.
const TerminalScript = `on run argv
  tell application "Terminal"
    activate
    do script (item 1 of argv)
  end tell
end run`

// --- Windows ---

// WinPlan is one way of opening a .ps1 launch script: a program, its
// arguments and the folder it starts in.
type WinPlan struct {
	Program string
	Args    []string
	Dir     string
	// NewConsole: start in a console window of its own (PowerShell; Windows
	// Terminal makes its own window).
	NewConsole bool
}

// powerShellArgs run a script and keep the window open. -ExecutionPolicy
// Bypass (this process only): the script is unsigned, and npm installs
// claude and codex as .ps1 shims that the default policy refuses.
func powerShellArgs(script string) []string {
	return []string{"-NoLogo", "-NoExit", "-ExecutionPolicy", "Bypass", "-File", script}
}

// WinPlans are the plans to try in order: Windows Terminal (a new tab in the
// most recent window, or a new window) when wanted and found at wtPath, with
// PowerShell in a console of its own as the fallback.
func WinPlans(pref, wtPath, powershell, script, folder string) []WinPlan {
	var out []WinPlan
	if pref != TerminalPowerShell && wtPath != "" {
		args := []string{"-w", "0", "new-tab"}
		if folder != "" {
			args = append(args, "-d", folder)
		}
		args = append(append(args, powershell), powerShellArgs(script)...)
		out = append(out, WinPlan{Program: wtPath, Args: args, Dir: folder})
	}
	return append(out, WinPlan{Program: powershell, Args: powerShellArgs(script), Dir: folder, NewConsole: true})
}
