//go:build !unix

package shim

// reapZombies is a no-op on non-unix platforms: PID-1 zombie reaping is a
// unix process-model concept that doesn't apply (e.g. on Windows).
func reapZombies(stop <-chan struct{}) {
	<-stop
}
