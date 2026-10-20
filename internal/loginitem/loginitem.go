// Package loginitem opens code-link at login without an installer: a
// LaunchAgent in ~/Library/LaunchAgents on macOS, a value under
// HKCU\Software\Microsoft\Windows\CurrentVersion\Run on Windows. Enable is
// idempotent and records the current program path, so calling it on every
// start follows a portable app that was moved.
package loginitem

import (
	"html"
	"path/filepath"
	"strings"
)

// Label names the LaunchAgent and the Run value.
const Label = "com.eidonix.code-link"

// RunValueName is the Windows Run value.
const RunValueName = "code-link"

// AppBundle is the .app containing exe (…/code-link.app/Contents/MacOS/code-link),
// or "" when exe isn't in a bundle.
func AppBundle(exe string) string {
	dir := filepath.Dir(exe)
	if filepath.Base(dir) != "MacOS" {
		return ""
	}
	contents := filepath.Dir(dir)
	app := filepath.Dir(contents)
	if filepath.Base(contents) != "Contents" || !strings.HasSuffix(app, ".app") {
		return ""
	}
	return app
}

// ProgramArguments start the app at login: an app bundle through `open`
// (so it starts as an app, like from Finder), a bare binary directly.
func ProgramArguments(exe string) []string {
	if app := AppBundle(exe); app != "" {
		return []string{"/usr/bin/open", "-a", app}
	}
	return []string{exe}
}

// Plist is the LaunchAgent for exe.
func Plist(exe string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + Label + `</string>
	<key>ProgramArguments</key>
	<array>
`)
	for _, a := range ProgramArguments(exe) {
		b.WriteString("\t\t<string>" + html.EscapeString(a) + "</string>\n")
	}
	b.WriteString(`	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>ProcessType</key>
	<string>Interactive</string>
	<key>LimitLoadToSessionType</key>
	<string>Aqua</string>
</dict>
</plist>
`)
	return b.String()
}

// RunValue is the Windows Run value for exe: the quoted path.
func RunValue(exe string) string { return `"` + exe + `"` }

// Exe is the running program, symlinks resolved.
func Exe() (string, error) {
	exe, err := executable()
	if err != nil {
		return "", err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	return exe, nil
}
