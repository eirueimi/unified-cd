package main

import "testing"

// The "idle" command blocks on <-ctx.Done() to keep the artifact sidecar
// resident (run.go). If the root context's Done() is nil (e.g.
// context.Background()), that receive is a nil-channel receive; once it is the
// only runnable goroutine the Go runtime kills the process with
// "fatal error: all goroutines are asleep - deadlock!", crashing the sidecar
// container ~60s after start. The root context must therefore have a non-nil
// Done channel (and a signal goroutine keeps the deadlock detector from ever
// firing). Regression test for the unified-artifact deadlock crash.
func TestRootContextHasNonNilDone(t *testing.T) {
	ctx, stop := rootContext()
	defer stop()
	if ctx.Done() == nil {
		t.Fatal("rootContext Done() is nil — idle's <-ctx.Done() would be a nil-channel receive and the Go runtime crashes the sidecar with 'all goroutines are asleep - deadlock!'")
	}
}
