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
	pauseUntilSignal(nil)
}

// pauseUntilSignal is Pause's core. When ready != nil it is called AFTER the
// signal handler is installed but BEFORE blocking on it. That ordering is the
// whole point: a caller (the signal test's helper process) can announce
// readiness only once sigCh is registered, so any signal sent afterwards is
// guaranteed to be delivered to sigCh rather than lost. Announcing readiness
// before registration — the previous test structure did this, with a throwaway
// signal.Notify meant only to suppress the default terminate disposition — left
// a window where a signal arriving between "ready" and registration was
// swallowed by the throwaway channel and never reached Pause, so Pause blocked
// forever (a load-dependent 30s test timeout on busy CI runners).
func pauseUntilSignal(ready func()) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)

	stop := make(chan struct{})
	if os.Getpid() == 1 {
		go reapZombies(stop)
	}

	if ready != nil {
		ready()
	}

	<-sigCh
	close(stop)
}
