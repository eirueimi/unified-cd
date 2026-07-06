//go:build windows

package k8sagent

import (
	"os/exec"
	"strconv"
)

// setPlatformProcAttrs is a no-op on Windows for this driver: taskkill (used
// by killPlatformProcessTree) kills the whole process tree by PID without
// needing any special process-creation flags.
func setPlatformProcAttrs(cmd *exec.Cmd) {}

// killPlatformProcessTree kills cmd's process and all its descendants via
// `taskkill /T /F /PID <pid>`, which recurses the whole tree Windows tracks
// via parent PID — sufficient for this test driver (the host agent's
// production code instead uses a Job Object, see
// internal/agent/exec_windows.go, which is more robust but unexported and not
// worth duplicating here just for a test driver).
func killPlatformProcessTree(cmd *exec.Cmd) {
	kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	_ = kill.Run()
}
