package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunStep_CapturesStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit, err := RunStep(t.Context(), "echo hello", &stdout, &stderr, nil, nil, "")
	require.NoError(t, err)
	assert.Equal(t, 0, exit)
	assert.Contains(t, stdout.String(), "hello")
}

func TestRunStep_NonZeroExit(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit, err := RunStep(t.Context(), "exit 3", &stdout, &stderr, nil, nil, "")
	require.NoError(t, err)
	assert.Equal(t, 3, exit)
}

func TestRunStep_RespectsContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	_, _ = RunStep(ctx, "sleep 10", &stdout, &stderr, nil, nil, "")
}

func TestRunStep_WorkDir(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	exit, err := RunStep(t.Context(), `echo hello > ./marker.txt`, &stdout, &stderr, nil, nil, dir)
	require.NoError(t, err)
	assert.Equal(t, 0, exit)
	content, readErr := os.ReadFile(filepath.Join(dir, "marker.txt"))
	require.NoError(t, readErr)
	assert.Contains(t, string(content), "hello")
}

func TestRunStepCapture_ReturnsStdout(t *testing.T) {
	var stderr bytes.Buffer
	stdout, exit, err := RunStepCapture(t.Context(), `printf "hello\nworld\n"`, &stderr, nil, nil, "")
	require.NoError(t, err)
	assert.Equal(t, 0, exit)
	assert.Contains(t, stdout, "hello")
	assert.Contains(t, stdout, "world")
}

func TestRunStepCapture_WorkDir(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("world"), 0o644))
	stdout, exit, err := RunStepCapture(t.Context(), `cat hello.txt`, nil, nil, nil, dir)
	require.NoError(t, err)
	assert.Equal(t, 0, exit)
	assert.Contains(t, stdout, "world")
}

// TestRunStepWithShell_UsesGivenArgv verifies RunStepWithShell execs the
// declared interpreter argv (not findShell()'s bash) as argv + [script],
// mirroring the container exec contract (shell + [run script], never
// re-parsed). bash is used here as the declared shell (available in this
// dev/CI environment via Git Bash on Windows) purely as a stand-in for "any
// explicit shell:" — the point under test is that the argv is honored
// verbatim, not that it differs from the native default.
func TestRunStepWithShell_UsesGivenArgv(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit, err := RunStepWithShell(t.Context(), []string{"bash", "-c"}, "echo from-explicit-shell", &stdout, &stderr, nil, nil, "")
	require.NoError(t, err)
	assert.Equal(t, 0, exit)
	assert.Contains(t, stdout.String(), "from-explicit-shell")
}

// TestRunStepWithShell_NonZeroExit mirrors TestRunStep_NonZeroExit for the
// explicit-shell path.
func TestRunStepWithShell_NonZeroExit(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit, err := RunStepWithShell(t.Context(), []string{"bash", "-c"}, "exit 7", &stdout, &stderr, nil, nil, "")
	require.NoError(t, err)
	assert.Equal(t, 7, exit)
}

// TestRunStepWithShell_WorkDir mirrors TestRunStep_WorkDir for the
// explicit-shell path.
func TestRunStepWithShell_WorkDir(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	exit, err := RunStepWithShell(t.Context(), []string{"bash", "-c"}, `echo hello > ./marker.txt`, &stdout, &stderr, nil, nil, dir)
	require.NoError(t, err)
	assert.Equal(t, 0, exit)
	content, readErr := os.ReadFile(filepath.Join(dir, "marker.txt"))
	require.NoError(t, readErr)
	assert.Contains(t, string(content), "hello")
}

// TestRunStepWithShell_MultiArgPrefixPreserved verifies extra argv elements
// before the script (e.g. [bash, -euo, pipefail, -c]) are all threaded
// through, not just a two-element [interpreter, flag] pair.
func TestRunStepWithShell_MultiArgPrefixPreserved(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit, err := RunStepWithShell(t.Context(), []string{"bash", "-euo", "pipefail", "-c"}, "false; echo unreachable", &stdout, &stderr, nil, nil, "")
	require.NoError(t, err)
	assert.NotEqual(t, 0, exit, "set -e must abort the script on the first failing command")
	assert.NotContains(t, stdout.String(), "unreachable")
}

// TestRunStep_CredentialsNotInheritedByChild is the exec-level regression
// guard for StepEnv: it proves a REAL subprocess spawned by RunStep cannot
// read the agent's credentials from its environment, not just that StepEnv's
// builder omits them (see stepenv_test.go for that unit-level coverage).
// The positive control (an ExposeEnv-allowlisted variable reaching the
// child) matters because without it a shell that silently failed to run
// would also satisfy the negative assertion below.
func TestRunStep_CredentialsNotInheritedByChild(t *testing.T) {
	t.Setenv("SOME_HOST_VAR", "super-secret-value")
	t.Setenv("UNIFIED_CACHE_SECRET", "super-secret-value")
	t.Setenv("MY_BUILD_FLAG", "visible-value")

	var stdout, stderr bytes.Buffer
	exit, err := RunStep(t.Context(),
		`echo "token=$SOME_HOST_VAR cache=$UNIFIED_CACHE_SECRET flag=$MY_BUILD_FLAG"`,
		&stdout, &stderr, nil, []string{"MY_BUILD_FLAG"}, "")
	require.NoError(t, err)
	assert.Equal(t, 0, exit)

	assert.NotContains(t, stdout.String(), "super-secret-value",
		"child process must not see the agent's credentials")
	assert.Contains(t, stdout.String(), "flag=visible-value",
		"positive control: an ExposeEnv-allowlisted variable must still reach the child")
}

// TestRunStep_DeniedCredentialStaysHiddenEvenIfExposed is the foot-gun guard
// at the real-subprocess level: an operator naming a denylisted credential in
// ExposeEnv must not make it visible to the child, mirroring
// TestStepEnv_DenylistBeatsExposeEnv but proving it through an actual exec
// rather than only through the StepEnv() builder.
func TestRunStep_DeniedCredentialStaysHiddenEvenIfExposed(t *testing.T) {
	t.Setenv("UNIFIED_CACHE_SECRET", "super-secret-value")

	var stdout, stderr bytes.Buffer
	exit, err := RunStep(t.Context(), `echo "token=$UNIFIED_CACHE_SECRET"`,
		&stdout, &stderr, nil, []string{"UNIFIED_CACHE_SECRET"}, "")
	require.NoError(t, err)
	assert.Equal(t, 0, exit)

	assert.NotContains(t, stdout.String(), "super-secret-value",
		"denylisted credential must stay hidden from the child even when named in ExposeEnv")
}

// TestRunStepWithShell_CredentialsNotInheritedByChild mirrors
// TestRunStep_CredentialsNotInheritedByChild for the explicit-shell exec path.
func TestRunStepWithShell_CredentialsNotInheritedByChild(t *testing.T) {
	t.Setenv("SOME_HOST_VAR", "super-secret-value")
	t.Setenv("UNIFIED_CACHE_SECRET", "super-secret-value")
	t.Setenv("MY_BUILD_FLAG", "visible-value")

	var stdout, stderr bytes.Buffer
	exit, err := RunStepWithShell(t.Context(), []string{"bash", "-c"},
		`echo "token=$SOME_HOST_VAR cache=$UNIFIED_CACHE_SECRET flag=$MY_BUILD_FLAG"`,
		&stdout, &stderr, nil, []string{"MY_BUILD_FLAG"}, "")
	require.NoError(t, err)
	assert.Equal(t, 0, exit)

	assert.NotContains(t, stdout.String(), "super-secret-value",
		"child process must not see the agent's credentials")
	assert.Contains(t, stdout.String(), "flag=visible-value",
		"positive control: an ExposeEnv-allowlisted variable must still reach the child")
}

// TestRunStepCapture_CredentialsNotInheritedByChild mirrors
// TestRunStep_CredentialsNotInheritedByChild for the captured-stdout exec path.
func TestRunStepCapture_CredentialsNotInheritedByChild(t *testing.T) {
	t.Setenv("SOME_HOST_VAR", "super-secret-value")
	t.Setenv("UNIFIED_CACHE_SECRET", "super-secret-value")
	t.Setenv("MY_BUILD_FLAG", "visible-value")

	var stderr bytes.Buffer
	stdout, exit, err := RunStepCapture(t.Context(),
		`echo "token=$SOME_HOST_VAR cache=$UNIFIED_CACHE_SECRET flag=$MY_BUILD_FLAG"`,
		&stderr, nil, []string{"MY_BUILD_FLAG"}, "")
	require.NoError(t, err)
	assert.Equal(t, 0, exit)

	assert.NotContains(t, stdout, "super-secret-value",
		"child process must not see the agent's credentials")
	assert.Contains(t, stdout, "flag=visible-value",
		"positive control: an ExposeEnv-allowlisted variable must still reach the child")
}

func TestFindShell_NonWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("non-Windows test")
	}
	shell := findShell()
	assert.Equal(t, "bash", shell)
}

func alwaysFailLookPath(string) (string, error) { return "", os.ErrNotExist }
func alwaysFailExists(string) bool              { return false }
func noHomeDir() (string, error)                { return "", os.ErrNotExist }

func TestLocateGitBash_FoundOnPath(t *testing.T) {
	lookPath := func(name string) (string, error) {
		assert.Equal(t, "bash", name)
		return `C:\custom\bash.exe`, nil
	}
	path, ok := locateGitBash(lookPath, alwaysFailExists, noHomeDir)
	assert.True(t, ok)
	assert.Equal(t, `C:\custom\bash.exe`, path)
}

func TestLocateGitBash_FoundAtWellKnownPath(t *testing.T) {
	exists := func(p string) bool { return p == `C:\Program Files\Git\bin\bash.exe` }
	path, ok := locateGitBash(alwaysFailLookPath, exists, noHomeDir)
	assert.True(t, ok)
	assert.Equal(t, `C:\Program Files\Git\bin\bash.exe`, path)
}

func TestLocateGitBash_NotFound(t *testing.T) {
	_, ok := locateGitBash(alwaysFailLookPath, alwaysFailExists, noHomeDir)
	assert.False(t, ok)
}

func TestLocateGitBash_SkipsWSLLauncherOnPath(t *testing.T) {
	lookPath := func(name string) (string, error) {
		return `C:\Windows\System32\bash.exe`, nil
	}
	_, ok := locateGitBash(lookPath, alwaysFailExists, noHomeDir)
	assert.False(t, ok, "WSL launcher on PATH must not be treated as git bash")
}

func TestLocateGitBash_PrefersWellKnownPathOverWSLOnPath(t *testing.T) {
	lookPath := func(name string) (string, error) {
		return `C:\Windows\System32\bash.exe`, nil
	}
	exists := func(p string) bool { return p == `C:\Program Files\Git\bin\bash.exe` }
	path, ok := locateGitBash(lookPath, exists, noHomeDir)
	assert.True(t, ok)
	assert.Equal(t, `C:\Program Files\Git\bin\bash.exe`, path)
}

func TestIsWSLLauncher(t *testing.T) {
	assert.True(t, isWSLLauncher(`C:\Windows\System32\bash.exe`))
	assert.True(t, isWSLLauncher(`C:\WINDOWS\SYSTEM32\bash.exe`))
	assert.False(t, isWSLLauncher(`C:\Program Files\Git\bin\bash.exe`))
	assert.False(t, isWSLLauncher(`C:\custom\bash.exe`))
}

// TestRunStep_CancelKillsGrandchild verifies that cancelling RunStep kills the
// whole process tree, not just the direct child shell. The step launches a
// grandchild (`sleep` backgrounded under `bash -c ... & wait`) that
// heartbeats into a file once a second. If only the shell were killed (the
// exec.CommandContext default, and the pre-fix behavior on both Unix and
// Windows), the grandchild would keep running and the heartbeat file would
// keep growing after cancellation — reproducing bug #9c (orphaned children
// surviving a cancelled run).
func TestRunStep_CancelKillsGrandchild(t *testing.T) {
	dir := t.TempDir()
	heartbeat := filepath.Join(dir, "heartbeat")
	// Backgrounding the heartbeat loop under `wait` gives it its own PID
	// distinct from the bash -lc shell, i.e. a grandchild from RunStep's
	// point of view (RunStep's direct child is the outer bash -lc process).
	script := `(for i in $(seq 1 30); do echo tick >> heartbeat; sleep 1; done) & wait`

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		var stdout, stderr bytes.Buffer
		_, _ = RunStep(ctx, script, &stdout, &stderr, nil, nil, dir)
	}()

	// Let the grandchild get going and write at least one heartbeat.
	require.Eventually(t, func() bool {
		info, err := os.Stat(heartbeat)
		return err == nil && info.Size() > 0
	}, 5*time.Second, 50*time.Millisecond, "grandchild did not start heartbeating")

	sizeAtCancel := statSize(t, heartbeat)
	cancel()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("RunStep did not return within 15s of cancellation — process tree likely not killed")
	}

	// Give a killed-but-not-yet-reaped grandchild a couple of heartbeat
	// intervals worth of time to prove it's actually gone, not just slow.
	time.Sleep(2500 * time.Millisecond)
	sizeAfterWait := statSize(t, heartbeat)
	assert.Equal(t, sizeAtCancel, sizeAfterWait,
		"heartbeat file grew after cancellation — grandchild process survived (process tree was not killed)")
}

func statSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return info.Size()
}

func TestRequireShellFor_NonWindows_AlwaysNil(t *testing.T) {
	err := requireShellFor("linux", alwaysFailLookPath, alwaysFailExists, noHomeDir)
	assert.NoError(t, err)
}

func TestRequireShellFor_WindowsFound_Nil(t *testing.T) {
	lookPath := func(string) (string, error) { return `C:\Git\bin\bash.exe`, nil }
	err := requireShellFor("windows", lookPath, alwaysFailExists, noHomeDir)
	assert.NoError(t, err)
}

func TestRequireShellFor_WindowsNotFound_Error(t *testing.T) {
	err := requireShellFor("windows", alwaysFailLookPath, alwaysFailExists, noHomeDir)
	assert.Error(t, err)
}

func TestLogPusherPendingAccumulation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "always fail", http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "tok")
	p := NewLogPusher(client, "a1", "run1", 0, "stdout")
	_, _ = p.Write([]byte("line1\n"))

	p.mu.Lock()
	p.flushLocked(t.Context())
	pendingCount := len(p.pending)
	p.mu.Unlock()

	assert.Equal(t, 1, pendingCount, "after a failed flush, 1 batch should be queued in pending")
}

func TestLogPusherPendingRetry(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= 1 {
			http.Error(w, "fail", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "tok")
	p := NewLogPusher(client, "a1", "run1", 0, "stdout")
	_, _ = p.Write([]byte("line1\n"))

	ctx := t.Context()
	// 1st attempt: fails -> queued in pending
	p.mu.Lock()
	p.flushLocked(ctx)
	pendingAfterFail := len(p.pending)
	p.mu.Unlock()

	// 2nd attempt: retries pending and succeeds
	p.mu.Lock()
	p.flushLocked(ctx)
	pendingAfterRetry := len(p.pending)
	p.mu.Unlock()

	assert.Equal(t, 1, pendingAfterFail, "after 1st failure, 1 batch should be queued in pending")
	assert.Equal(t, 0, pendingAfterRetry, "after 2nd success, pending should be empty")
	assert.Equal(t, 2, calls)
}

func TestLogPusherPendingCapExceeded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "always fail", http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "tok")
	p := NewLogPusher(client, "a1", "run1", 0, "stdout")
	p.maxPendingBytes = 10 // 10-byte cap

	ctx := t.Context()
	for _, line := range []string{"hello world\n", "second batch\n", "third batch!\n"} {
		_, _ = p.Write([]byte(line))
		p.mu.Lock()
		p.flushLocked(ctx)
		p.mu.Unlock()
	}

	p.mu.Lock()
	batches := len(p.pending)
	p.mu.Unlock()
	assert.GreaterOrEqual(t, batches, 1, "even when cap is exceeded, the latest batch should be retained")
	assert.LessOrEqual(t, batches, 2, "when cap is exceeded, old batches should be discarded")
}

func TestLogPusherFlushExitRetry(t *testing.T) {
	attempt := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt++
		if attempt < 3 {
			http.Error(w, "fail", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "tok")
	p := NewLogPusher(client, "a1", "run1", 0, "stdout")
	_, _ = p.Write([]byte("important\n"))

	// Pass a cancelled context to simulate stepCtx cancellation at shutdown
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	// Flush: attempt 1 fails -> pending; retry i=0 (1s wait) attempt 2 fails; retry i=1 (1s wait) attempt 3 succeeds
	// exit-time retries use an independent context, so they continue even with cancelledCtx
	p.Flush(cancelledCtx) // ~2s

	p.mu.Lock()
	pendingCount := len(p.pending)
	p.mu.Unlock()
	assert.Equal(t, 0, pendingCount, "after Flush exit-retry succeeds, pending should be empty")
	assert.Equal(t, 3, attempt)
}

// TestLogPusher_StartAutoFlush_ShipsLinesBeforeStepEnds proves that
// StartAutoFlush periodically ships buffered lines to the server without
// waiting for an explicit Flush call — mirroring the k8s agent's need to
// surface sparse stderr output mid-step instead of only at step end.
func TestLogPusher_StartAutoFlush_ShipsLinesBeforeStepEnds(t *testing.T) {
	var mu sync.Mutex
	var received []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqs []api.LogAppendRequest
		_ = json.NewDecoder(r.Body).Decode(&reqs)
		mu.Lock()
		for _, req := range reqs {
			received = append(received, req.Line)
		}
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "tok")
	p := NewLogPusher(client, "a1", "run1", 0, "stderr")

	flushCtx, stop := context.WithCancel(context.Background())
	defer stop()
	p.StartAutoFlush(flushCtx, 20*time.Millisecond)

	_, _ = p.Write([]byte("early-line\n"))

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, l := range received {
			if l == "early-line" {
				return true
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond, "expected StartAutoFlush to ship the buffered line without an explicit Flush call")

	stop()
}

// TestLogPusher_StartAutoFlush_HoldsBackPartialLine proves auto-flush only
// ships complete lines (terminated by '\n'): a partial trailing line with no
// newline yet must not be shipped until it is completed or the step ends
// with an explicit Flush.
func TestLogPusher_StartAutoFlush_HoldsBackPartialLine(t *testing.T) {
	var mu sync.Mutex
	var received []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqs []api.LogAppendRequest
		_ = json.NewDecoder(r.Body).Decode(&reqs)
		mu.Lock()
		for _, req := range reqs {
			received = append(received, req.Line)
		}
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "tok")
	p := NewLogPusher(client, "a1", "run1", 0, "stderr")

	flushCtx, stop := context.WithCancel(context.Background())
	defer stop()
	p.StartAutoFlush(flushCtx, 20*time.Millisecond)

	_, _ = p.Write([]byte("no-newline-yet"))

	// Give auto-flush several ticks to (incorrectly) ship the partial line.
	time.Sleep(150 * time.Millisecond)
	stop()

	mu.Lock()
	defer mu.Unlock()
	for _, l := range received {
		assert.NotEqual(t, "no-newline-yet", l, "auto-flush must not ship an incomplete line before it is newline-terminated")
	}
}

// TestLogPusher_DropMarker proves that once old pending batches are discarded
// (drop-oldest under the pending byte cap, e.g. during a controller
// partition), the next successful flush surfaces exactly one synthetic
// marker line reporting how many lines were silently dropped, instead of
// losing them without a trace.
func TestLogPusher_DropMarker(t *testing.T) {
	var mu sync.Mutex
	var received []api.LogAppendRequest
	failing := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fail := failing
		mu.Unlock()
		if fail {
			http.Error(w, "fail", http.StatusInternalServerError)
			return
		}
		var reqs []api.LogAppendRequest
		_ = json.NewDecoder(r.Body).Decode(&reqs)
		mu.Lock()
		received = append(received, reqs...)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "tok")
	p := NewLogPusher(client, "a1", "run1", 0, "stdout")
	p.maxPendingBytes = 1 // tiny cap so each new failed batch drops the previous one

	ctx := t.Context()

	// Each of these flushes fails, and starting with the 2nd, the byte cap
	// forces the previous batch to be dropped (drop-oldest), discarding 1
	// line each time -> 2 lines dropped in total (l1, then l2).
	for _, line := range []string{"l1\n", "l2\n", "l3\n"} {
		_, _ = p.Write([]byte(line))
		p.mu.Lock()
		p.flushLocked(ctx)
		p.mu.Unlock()
	}

	p.mu.Lock()
	dropped := p.droppedLines
	p.mu.Unlock()
	require.Equal(t, 2, dropped, "sanity check: 2 lines should have been dropped before recovery")

	// Now the controller becomes reachable again; the next successful flush
	// should clear remaining pending batches and surface exactly one marker
	// line with the dropped count.
	mu.Lock()
	failing = false
	mu.Unlock()

	p.mu.Lock()
	p.flushLocked(ctx)
	remainingDropped := p.droppedLines
	p.mu.Unlock()

	assert.Equal(t, 0, remainingDropped, "droppedLines should reset to 0 after the marker is sent")

	mu.Lock()
	defer mu.Unlock()
	var markerLines []string
	for _, r := range received {
		if strings.Contains(r.Line, "dropped") {
			markerLines = append(markerLines, r.Line)
		}
	}
	require.Len(t, markerLines, 1, "expected exactly one dropped-lines marker line")
	assert.Contains(t, markerLines[0], "2 log line(s) dropped", "marker should report the correct dropped count")
}

// TestLogPusher_DropMarker_ResetsOnlyOnDelivery proves the marker's dropped
// count is preserved across a failed marker-send: droppedLines is reset ONLY
// when the marker is confirmed delivered, and a failed marker is never queued
// into p.pending (where a later cap overflow could evict it and silently
// undercount). It also proves further drops between attempts accumulate.
func TestLogPusher_DropMarker_ResetsOnlyOnDelivery(t *testing.T) {
	var mu sync.Mutex
	var received []api.LogAppendRequest
	failing := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fail := failing
		mu.Unlock()
		if fail {
			http.Error(w, "fail", http.StatusInternalServerError)
			return
		}
		var reqs []api.LogAppendRequest
		_ = json.NewDecoder(r.Body).Decode(&reqs)
		mu.Lock()
		received = append(received, reqs...)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "tok")
	p := NewLogPusher(client, "a1", "run1", 0, "stdout")

	ctx := t.Context()

	// Seed a known dropped count with no pending batches; the client is still
	// failing, so the marker send fails.
	p.mu.Lock()
	p.droppedLines = 5
	p.flushLocked(ctx)
	afterFailDropped := p.droppedLines
	afterFailPending := len(p.pending)
	p.mu.Unlock()

	assert.Equal(t, 5, afterFailDropped, "a failed marker send must NOT reset droppedLines")
	assert.Equal(t, 0, afterFailPending, "a failed marker must NOT be queued into pending (would be evictable and undercount)")

	// Between attempts, more lines are dropped; the count keeps accumulating.
	p.mu.Lock()
	p.droppedLines += 3 // now 8
	p.mu.Unlock()

	// Controller recovers; next flush delivers the marker with the true,
	// accumulated count and resets.
	mu.Lock()
	failing = false
	mu.Unlock()

	p.mu.Lock()
	p.flushLocked(ctx)
	afterSuccessDropped := p.droppedLines
	p.mu.Unlock()

	assert.Equal(t, 0, afterSuccessDropped, "droppedLines should reset to 0 only after confirmed delivery")

	mu.Lock()
	defer mu.Unlock()
	var markerLines []string
	for _, r := range received {
		if strings.Contains(r.Line, "dropped") {
			markerLines = append(markerLines, r.Line)
		}
	}
	require.Len(t, markerLines, 1, "expected exactly one dropped-lines marker line delivered")
	assert.Contains(t, markerLines[0], "8 log line(s) dropped", "marker should report the accumulated count (5 + 3)")
}

// TestRunStep_PreservesBackslashRuns guards the Windows argv-escaping bug: a
// native step script that spells out runs of backslashes (e.g. a sed
// s|\\\\|\\|g) must reach bash intact. On Windows, passing the script as an
// exec argv halves backslash runs (s|\\|\|g), corrupting the script; the fix
// routes the script through an environment variable instead. On Unix there is
// no such corruption, so this test also documents the expected behavior there.
func TestRunStep_PreservesBackslashRuns(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// printf %s of a single-quoted literal: bash echoes the argument verbatim.
	exit, err := RunStep(t.Context(), `printf '%s' 's|\\\\|\\|g'`, &stdout, &stderr, nil, nil, "")
	require.NoError(t, err)
	assert.Equal(t, 0, exit)
	assert.Equal(t, `s|\\\\|\\|g`, stdout.String(), "backslash runs must survive (stderr: %s)", stderr.String())
}

// TestRunStepCapture_PreservesBackslashRuns mirrors
// TestRunStep_PreservesBackslashRuns for the capture path: the returned stdout
// string must contain the un-halved backslash runs.
func TestRunStepCapture_PreservesBackslashRuns(t *testing.T) {
	var stderr bytes.Buffer
	stdout, exit, err := RunStepCapture(t.Context(), `printf '%s' 's|\\\\|\\|g'`, &stderr, nil, nil, "")
	require.NoError(t, err)
	assert.Equal(t, 0, exit)
	assert.Equal(t, `s|\\\\|\\|g`, stdout, "backslash runs must survive (stderr: %s)", stderr.String())
}
