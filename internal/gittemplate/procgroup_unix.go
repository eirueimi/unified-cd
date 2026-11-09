//go:build !windows

package gittemplate

import (
	"os/exec"
	"syscall"
)

// killTree makes cmd start in its own process group and, on context
// cancellation, SIGKILLs the whole group so helpers git spawns (ssh,
// git-remote-https, askpass scripts) do not outlive a timed-out operation.
func killTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
