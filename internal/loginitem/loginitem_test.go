package loginitem

import (
	"strings"
	"testing"
)

func TestAppBundleAndProgramArguments(t *testing.T) {
	exe := "/Users/d/Apps/code-link.app/Contents/MacOS/code-link"
	if AppBundle(exe) != "/Users/d/Apps/code-link.app" {
		t.Errorf("bundle %q", AppBundle(exe))
	}
	if got := ProgramArguments(exe); strings.Join(got, "|") != "/usr/bin/open|-a|/Users/d/Apps/code-link.app" {
		t.Errorf("bundle args %q", got)
	}
	if got := ProgramArguments("/Users/d/bin/code-link"); len(got) != 1 || got[0] != "/Users/d/bin/code-link" {
		t.Errorf("bare args %q", got)
	}
	if AppBundle("/x/MacOS/code-link") != "" {
		t.Error("not a bundle")
	}
}

func TestPlist(t *testing.T) {
	p := Plist("/Users/d/A & B/code-link.app/Contents/MacOS/code-link")
	for _, want := range []string{"<string>com.eidonix.code-link</string>", "<key>RunAtLoad</key>\n\t<true/>",
		"<string>/usr/bin/open</string>", "<string>/Users/d/A &amp; B/code-link.app</string>", "<string>Aqua</string>"} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q in\n%s", want, p)
		}
	}
}

func TestRunValueIsQuoted(t *testing.T) {
	if RunValue(`C:\Tools\code link\code-link.exe`) != `"C:\Tools\code link\code-link.exe"` {
		t.Error(RunValue(`C:\Tools\code link\code-link.exe`))
	}
}
