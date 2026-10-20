//go:build darwin

package selfupdate

import (
	"fmt"
	"os/exec"
	"strings"
)

// extractZip unpacks a ditto zip, keeping the code signature and extended
// attributes the app needs to start.
func extractZip(zip, dest string) error {
	if out, err := exec.Command("/usr/bin/ditto", "-x", "-k", zip, dest).CombinedOutput(); err != nil {
		return fmt.Errorf("ditto: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
