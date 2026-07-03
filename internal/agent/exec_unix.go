//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
)

// setupProcAttrs configures cmd to start in its own process group so that the
// whole process tree (the shell plus any children/grandchildren it spawns,
// e.g. `bash -c 'sleep 120'`) can be killed together via killTree.
// Without this, killing cmd.Process only signals the direct child (the shell);
// grandchildren re-parent and are orphaned, surviving the cancellation.
func setupProcAttrs(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// assignJob is a no-op on Unix: the process-group setup done by
// setupProcAttrs (before Start) is all that's needed for killTree to work,
// unlike Windows where job-object assignment must happen after Start.
func assignJob(cmd *exec.Cmd) error {
	return nil
}

// killTree kills the entire process group started for cmd, ensuring
// grandchildren (e.g. `sleep` spawned by `bash -c`) are terminated along with
// the shell itself. cmd must have been started with setupProcAttrs applied
// and must have a valid PID (i.e. cmd.Start() succeeded).
func killTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pgid := cmd.Process.Pid
	// Negative pid signals the whole process group (see setpgid(2)/kill(2)).
	// Ignore errors: the group may already be gone (process exited naturally).
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}
