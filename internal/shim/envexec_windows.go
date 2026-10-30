//go:build windows

package shim

import (
	"os"
	"os/exec"
)

func forwardedSignals() []os.Signal { return []os.Signal{os.Interrupt} }

func exitCodeOf(ee *exec.ExitError) int { return ee.ExitCode() }
