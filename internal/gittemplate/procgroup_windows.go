//go:build windows

package gittemplate

import (
	"os/exec"
	"strconv"
)

// killTree makes context cancellation terminate the whole process tree rooted at
// cmd (taskkill /T), so helpers git spawns do not outlive a timed-out operation.
func killTree(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		return cmd.Process.Kill()
	}
}
