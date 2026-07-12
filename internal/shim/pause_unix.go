//go:build unix

package shim

import (
	"os"
	"os/signal"
	"syscall"
)

// reapZombies waits for SIGCHLD and reaps exited children via wait4 until
// stop is closed, as required of a container's PID-1 process (the same
// trick used by tini/dumb-init/Argo's argoexec).
func reapZombies(stop <-chan struct{}) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGCHLD)
	defer signal.Stop(sigCh)

	reapAvailable := func() {
		for {
			var ws syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
			if pid <= 0 || err != nil {
				return
			}
		}
	}

	// Reap anything that exited before we started watching.
	reapAvailable()

	for {
		select {
		case <-stop:
			return
		case <-sigCh:
			reapAvailable()
		}
	}
}
