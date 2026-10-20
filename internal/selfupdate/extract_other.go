//go:build !darwin

package selfupdate

import "errors"

// extractZip is only needed for the macOS update zip.
func extractZip(zip, dest string) error { return errors.New("unpacking an app bundle needs macOS") }
