//go:build !darwin && !windows

package tools

import "runtime"

func nativeArch() string { return runtime.GOARCH }
