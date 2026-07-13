//go:build unix

// Pause's interesting behavior is entirely signal-driven, which only has
// well-defined, testable semantics on unix - hence this whole file is
// unix-gated (linux/darwin/etc; see the "unix" build tag).
package shim

import (
	"os"
	"os/exec"
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
	case <-time.After(10 * time.Second):
		// Safety ceiling only. Readiness is now announced from inside
		// pauseUntilSignal (after sigCh is registered), so once we've read
		// "ready" the signal is guaranteed to reach Pause and it exits
		// immediately — this branch should never fire. It previously did (a
		// 30s timeout flaking on the loaded Integration runner) because the
		// helper announced readiness before Pause registered, letting a
		// throwaway handler swallow the signal in the window; that race is now
		// structurally closed, so a hit here means Pause genuinely hung.
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
	// Announce readiness from INSIDE pauseUntilSignal, after it has installed
	// its signal handler and just before it blocks. This closes the
	// signal-theft race: the parent only sends the signal after reading
	// "ready", by which point sigCh is registered, so the signal always
	// reaches Pause instead of being lost in a pre-registration window. (The
	// old structure wrote "ready" before Pause registered and relied on a
	// throwaway signal.Notify to avoid the default kill — but that throwaway
	// channel swallowed any signal arriving in the window, hanging Pause.)
	pauseUntilSignal(func() { os.Stderr.WriteString("ready") })
	os.Exit(0)
}
