//go:build !darwin && !windows

package launch

import "errors"

func (o Opener) open(s Session, folder, pathDir string) error {
	return errors.New("opening a terminal is supported on macOS and Windows; copy the command instead")
}
