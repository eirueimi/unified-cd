//go:build unix

// Pause's interesting behavior is entirely signal-driven, which only has
// well-defined, testable semantics on unix - hence this whole file is
// unix-gated (linux/darwin/etc; see the "unix" build tag).
package shim

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// TestPause_ExitsZeroOnSIGTERM exercises Pause() in a real child process
// (re-exec'ing this same test binary, the standard os/exec "helper process"
// pattern) since Pause installs a process-wide signal.Notify and blocks -
// it can't be driven in-process without interfering with the rest of the
// test suite's signal handling.
func TestPause_ExitsZeroOnSIGTERM(t *testing.T) {
	testPauseSignal(t, syscall.SIGTERM)
}

func TestPause_ExitsZeroOnSIGINT(t *testing.T) {
	testPauseSignal(t, syscall.SIGINT)
}

func testPauseSignal(t *testing.T, sig syscall.Signal) {
	t.Helper()

	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcessPause")
	cmd.Env = append(os.Environ(), "UCD_SH_HELPER_PROCESS=1")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("StderrPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}

	// Wait for the helper to signal it has installed its handler, so we
	// don't race sending the signal before Pause's signal.Notify call.
	ready := make([]byte, 5) // "ready"
	if _, err := readFull(stderr, ready); err != nil {
		cmd.Process.Kill()
		t.Fatalf("waiting for helper readiness: %v", err)
	}

	if err := cmd.Process.Signal(sig); err != nil {
		t.Fatalf("signal helper process: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("helper process exited with error, want exit 0: %v", err)
		}
	case <-time.After(30 * time.Second):
		// Generous ceiling, not an expected wait: the helper is a fresh
		// `go test` subprocess, and on a loaded CI runner its startup +
		// signal delivery can exceed a few seconds (a 5s bound flaked twice
		// on the Integration-tests job). Pause exits as soon as the signal
		// lands, so a large timeout costs nothing on the passing path and
		// only guards against runner contention.
		cmd.Process.Kill()
		t.Fatal("helper process did not exit after signal")
	}
}

func readFull(r interface{ Read([]byte) (int, error) }, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// TestHelperProcessPause is not a real test: it's a re-exec target invoked
// by testPauseSignal via `-test.run=TestHelperProcessPause`, gated on an
// env var so it's a no-op under the normal `go test` run.
func TestHelperProcessPause(t *testing.T) {
	if os.Getenv("UCD_SH_HELPER_PROCESS") != "1" {
		return
	}
	// Pre-register a throwaway handler BEFORE announcing readiness: once any
	// signal.Notify registration exists, Go disables the default terminate
	// disposition process-wide, so a signal landing in the window between
	// the ready write and Pause's own Notify call can no longer kill the
	// helper — it is simply delivered to Pause's channel once registered.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGTERM, syscall.SIGINT)
	os.Stderr.WriteString("ready")
	Pause()
	os.Exit(0)
}
