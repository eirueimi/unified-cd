//go:build !windows

package k8sagent

import (
	"os/exec"
	"syscall"
)

// setPlatformProcAttrs starts cmd in its own process group so
// killPlatformProcessTree can signal the whole tree at once (mirrors
// internal/agent/exec_unix.go's setupProcAttrs).
func setPlatformProcAttrs(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killPlatformProcessTree signals the whole process group started for cmd
// (negative PID targets the group), killing any grandchildren (e.g. `sleep`
// spawned by `bash -c`) along with the shell itself.
func killPlatformProcessTree(cmd *exec.Cmd) {
	pgid := cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}
