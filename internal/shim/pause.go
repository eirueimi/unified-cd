package shim

import (
	"os"
	"os/signal"
	"syscall"
)

// Pause blocks until SIGTERM or SIGINT is received, then returns so the
// caller can exit 0. It is the keep-alive used in place of `sleep
// infinity` for containers whose only job is to stay alive for `container:`
// exec steps.
//
// When running as PID 1 (the container's init process), a bare keep-alive
// process must also reap zombie children, since nothing else in the
// container will; Pause does this for the duration of the wait (build-tag
// gated: real reaping on unix, a no-op elsewhere).
func Pause() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)

	stop := make(chan struct{})
	if os.Getpid() == 1 {
		go reapZombies(stop)
	}

	<-sigCh
	close(stop)
}
