package shim

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer wraps bytes.Buffer with a mutex so it's safe to use as the
// Stdout/Stderr writer for a script that may run commands in the background
// (`cmd &`). mvdan.cc/sh's interp package documents that Stdout/Stderr may be
// written concurrently from background-command goroutines once a script uses
// `&` — the caller owns synchronization. Production is safe (agent.LogPusher
// is mutex-guarded); a plain bytes.Buffer, which these tests used to hand to
// Run/RunWithHandlers directly, is not, and races under -race for any pin or
// corpus script that backgrounds anything (e.g. TestPin_FanOutJoin's
// `(echo a) & (echo b) & wait`).
//
// runScript (used by every construct pin in corpus_test.go) and
// runCorpusScript (the corpus walker) both route through syncBuffer
// unconditionally — not just the pins that background something today —
// so a future pin or a corpus script gaining a `&` doesn't silently
// reintroduce this race; it's safe by default rather than by audit.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

func runScript(t *testing.T, script string) (stdout, stderr string, code int, err error) {
	t.Helper()
	var outBuf, errBuf syncBuffer
	code, err = Run(context.Background(), script, strings.NewReader(""), &outBuf, &errBuf, nil, "")
	return outBuf.String(), errBuf.String(), code, err
}

func TestRun_EchoExitZero(t *testing.T) {
	stdout, stderr, code, err := runScript(t, "echo hi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	if got := strings.TrimRight(stdout, "\n"); got != "hi" {
		t.Fatalf("stdout = %q, want %q", got, "hi")
	}
}

func TestRun_ExplicitExitCode(t *testing.T) {
	_, _, code, err := runScript(t, "exit 3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 3 {
		t.Fatalf("exit code = %d, want 3", code)
	}
}

func TestRun_SetDashE(t *testing.T) {
	stdout, _, code, err := runScript(t, "set -e; false; echo no")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code == 0 {
		t.Fatalf("exit code = 0, want nonzero under set -e after false")
	}
	if strings.Contains(stdout, "no") {
		t.Fatalf("stdout = %q, must not contain %q (set -e should have stopped the script)", stdout, "no")
	}
}

func TestRun_PipefailPropagatesFailure(t *testing.T) {
	_, _, code, err := runScript(t, "set -o pipefail; false | true")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code == 0 {
		t.Fatalf("exit code = 0, want nonzero under pipefail when the first pipeline command fails")
	}
}

func TestRun_StdinPassthrough(t *testing.T) {
	var outBuf, errBuf bytes.Buffer
	code, err := Run(context.Background(), "cat", strings.NewReader("from stdin"), &outBuf, &errBuf, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errBuf.String())
	}
	if outBuf.String() != "from stdin" {
		t.Fatalf("stdout = %q, want %q", outBuf.String(), "from stdin")
	}
}

func TestRun_EnvPassthrough(t *testing.T) {
	var outBuf, errBuf bytes.Buffer
	code, err := Run(context.Background(), "echo $GREETING", strings.NewReader(""), &outBuf, &errBuf, []string{"GREETING=hello"}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errBuf.String())
	}
	if got := strings.TrimRight(outBuf.String(), "\n"); got != "hello" {
		t.Fatalf("stdout = %q, want %q", got, "hello")
	}
}

func TestRun_DirSetsWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var outBuf, errBuf bytes.Buffer
	code, err := Run(context.Background(), "ls", strings.NewReader(""), &outBuf, &errBuf, nil, dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errBuf.String())
	}
	if !strings.Contains(outBuf.String(), "marker.txt") {
		t.Fatalf("stdout = %q, expected it to list marker.txt (dir not applied?)", outBuf.String())
	}
}

func TestRun_ParseErrorReturnsExitTwo(t *testing.T) {
	_, _, code, err := runScript(t, "if then fi fi (((")
	if err == nil {
		t.Fatal("expected a parse error, got nil")
	}
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 for a parse error", code)
	}
}

func TestRun_TrapExitStillFiresAfterSanitizing(t *testing.T) {
	// TERM is stripped (with a warning), but EXIT must still fire exactly
	// once when the script ends.
	stdout, stderr, code, err := runScript(t, `trap 'echo cleaned' TERM EXIT`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	if got := strings.Count(stdout, "cleaned"); got != 1 {
		t.Fatalf("stdout = %q, want exactly one %q", stdout, "cleaned")
	}
	if !strings.Contains(stderr, "[ucd-sh]") || !strings.Contains(stderr, "TERM") {
		t.Fatalf("stderr = %q, want a [ucd-sh] warning naming TERM", stderr)
	}
}

func TestRun_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var outBuf, errBuf bytes.Buffer

	done := make(chan struct{})
	var code int
	var runErr error
	go func() {
		code, runErr = Run(ctx, "while true; do :; done", strings.NewReader(""), &outBuf, &errBuf, nil, "")
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}

	if runErr == nil {
		t.Fatal("expected a non-nil error after cancellation")
	}
	if code == 0 {
		t.Fatalf("exit code = %d, want nonzero after cancellation", code)
	}
}

func TestInstall_CopiesAndSetsExecBit(t *testing.T) {
	// Build a tiny throwaway "self" binary isn't necessary: Install copies
	// os.Executable() of the running process (the go test binary), which is
	// enough to exercise the copy + chmod + atomic rename path.
	dir := t.TempDir()
	dest := filepath.Join(dir, "ucd-sh-installed")

	if err := Install(dest); err != nil {
		t.Fatalf("Install: %v", err)
	}

	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat installed binary: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("installed binary is empty")
	}
	if runtime.GOOS != "windows" {
		if info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("installed binary mode = %v, want executable bits set", info.Mode())
		}
	}
}

func TestInstall_OverwritesExistingTarget(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "ucd-sh-installed")
	if err := os.WriteFile(dest, []byte("stale placeholder"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Install(dest); err != nil {
		t.Fatalf("Install over existing target: %v", err)
	}

	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat installed binary: %v", err)
	}
	if info.Size() <= int64(len("stale placeholder")) && info.Size() != 0 {
		// Not a strict check (sizes could coincidentally collide), just a
		// smoke check that content changed to something binary-sized.
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "stale placeholder" {
		t.Fatal("installed binary still contains the stale placeholder content")
	}
}
