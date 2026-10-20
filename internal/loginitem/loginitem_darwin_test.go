//go:build darwin

package loginitem

import (
	"strings"
	"testing"
)

func TestEnableWritesTheLaunchAgentInHome(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if Enabled() {
		t.Fatal("not yet")
	}
	if err := Enable("/Applications/code-link.app/Contents/MacOS/code-link"); err != nil {
		t.Fatal(err)
	}
	p, _ := PlistPath()
	if !Enabled() || !strings.HasSuffix(p, "/Library/LaunchAgents/com.eidonix.code-link.plist") {
		t.Errorf("enabled %v at %s", Enabled(), p)
	}
	if err := Enable("/Applications/code-link.app/Contents/MacOS/code-link"); err != nil {
		t.Fatal(err)
	}
	if err := Disable(); err != nil || Enabled() {
		t.Errorf("disable: %v", err)
	}
	if err := Disable(); err != nil {
		t.Errorf("disabling twice: %v", err)
	}
}
