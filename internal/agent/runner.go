package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
	"github.com/eirueimi/unified-cd/internal/secrets"
)

// findShell returns the shell executable used for step execution.
// On Windows it searches for git bash; on all other platforms it returns "bash".
func findShell() string {
	if runtime.GOOS != "windows" {
		return "bash"
	}
	if path, ok := locateGitBash(exec.LookPath, fileExists, os.UserHomeDir); ok {
		return path
	}
	return "bash"
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// locateGitBash looks for git for Windows at known installation paths, then falls back to bash on PATH.
// If the bash found on PATH is `System32\bash.exe` (the WSL launcher), it is excluded to
// prevent WSL from being launched unintentionally.
// lookPath/exists/homeDir are injected for testability.
func locateGitBash(lookPath func(string) (string, error), exists func(string) bool, homeDir func() (string, error)) (string, bool) {
	candidates := []string{
		`C:\Program Files\Git\bin\bash.exe`,
		`C:\Program Files (x86)\Git\bin\bash.exe`,
		`C:\Git\bin\bash.exe`,
	}
	if home, err := homeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, `AppData\Local\Programs\Git\bin\bash.exe`))
	}
	for _, c := range candidates {
		if exists(c) {
			return c, true
		}
	}
	if path, err := lookPath("bash"); err == nil && !isWSLLauncher(path) {
		return path, true
	}
	return "", false
}

// isWSLLauncher reports whether path is the WSL launcher (%SystemRoot%\System32\bash.exe).
// Since Windows 10, System32 ships a bash.exe that launches WSL; if it appears on PATH before
// Git Bash, WSL would be started unintentionally.
// Because the path is always in Windows format (backslash-separated), string operations are used
// instead of filepath to keep the check host-OS-independent.
func isWSLLauncher(path string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(path, "/", `\`))
	return strings.HasSuffix(normalized, `\system32\bash.exe`)
}

// requireShellFor returns an error when goos is "windows" and git bash cannot be found.
// On all other platforms it always returns nil (bash is assumed to be available from the OS).
// lookPath/exists/homeDir are injected for testability.
func requireShellFor(goos string, lookPath func(string) (string, error), exists func(string) bool, homeDir func() (string, error)) error {
	if goos != "windows" {
		return nil
	}
	if _, ok := locateGitBash(lookPath, exists, homeDir); ok {
		return nil
	}
	return fmt.Errorf("git bash not found — install Git for Windows (https://git-scm.com/download/win) or add bash.exe to PATH")
}

// RequireShell returns an error if git bash cannot be found on Windows.
// Call it once at agent startup to surface the failure early rather than only at the first step execution.
func RequireShell() error {
	return requireShellFor(runtime.GOOS, exec.LookPath, fileExists, os.UserHomeDir)
}

// RunStep executes the given script with bash, writing stdout/stderr to the provided writers.
// Returns the exit code and any error. The process is interrupted if the context is cancelled.
// On cancellation the whole process tree is killed (not just the shell), so children the
// shell spawned (e.g. `sleep` from `bash -c 'sleep 120'`) don't survive as orphans — see
// runTreeKilled for why exec.CommandContext alone is not enough.
// Extra environment variables can be supplied via extraEnv in "KEY=VALUE" format.
// If workDir is non-empty, the command runs with that directory as the working directory.
func RunStep(ctx context.Context, script string, stdout, stderr io.Writer, extraEnv []string, workDir string) (int, error) {
	cmd := exec.Command(findShell(), "-lc", script)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	if workDir != "" {
		cmd.Dir = workDir
	}
	err := runTreeKilled(ctx, cmd)
	if err == nil {
		return 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	return -1, err
}

// RunStepCapture executes a script and returns the captured stdout string and exit code.
// stderr is written to the provided writer (for log shipping).
// On cancellation the whole process tree is killed (not just the shell) — see runTreeKilled.
// Extra environment variables can be supplied via extraEnv in "KEY=VALUE" format.
// If workDir is non-empty, the command runs with that directory as the working directory.
func RunStepCapture(ctx context.Context, script string, stderr io.Writer, extraEnv []string, workDir string) (stdout string, exitCode int, err error) {
	var stdoutBuf bytes.Buffer
	cmd := exec.Command(findShell(), "-lc", script)
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = stderr
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	if workDir != "" {
		cmd.Dir = workDir
	}
	runErr := runTreeKilled(ctx, cmd)
	stdout = stdoutBuf.String()
	if runErr == nil {
		return stdout, 0, nil
	}
	if ee, ok := runErr.(*exec.ExitError); ok {
		return stdout, ee.ExitCode(), nil
	}
	return stdout, -1, runErr
}

// RunStepContainer runs script inside a fresh container via rt, capturing
// stdout (like RunStepCapture) and streaming stderr to the provided writer.
// No host workspace is mounted — this is the isolated runsIn.image path.
func RunStepContainer(ctx context.Context, rt crt.ContainerRuntime, image, script string, stderr io.Writer, extraEnv []string, cpuLimit, memLimit string) (stdout string, exitCode int, err error) {
	var buf bytes.Buffer
	code, runErr := rt.Run(ctx, crt.RunSpec{
		Image:    image,
		Script:   script,
		Env:      extraEnv,
		CPULimit: cpuLimit,
		MemLimit: memLimit,
	}, &buf, stderr)
	return buf.String(), code, runErr
}

// pendingBatch holds a batch of log requests that failed to send.
type pendingBatch struct {
	reqs []api.LogAppendRequest
}

// LogPusher is a Writer that buffers log lines and asynchronously ships them to the master server.
// Batches that fail to send are queued in pending and retried on the next flush.
type LogPusher struct {
	mu              sync.Mutex
	buf             bytes.Buffer
	pending         []pendingBatch
	maxPendingBytes int
	stream          string
	runID           string
	stepIndex       int
	agentID         string
	client          *Client
	flushBytes      int
	masker          *secrets.Masker
}

// NewLogPusher creates a new LogPusher with the given parameters.
func NewLogPusher(client *Client, agentID, runID string, stepIndex int, stream string) *LogPusher {
	return &LogPusher{
		stream:          stream,
		runID:           runID,
		stepIndex:       stepIndex,
		agentID:         agentID,
		client:          client,
		flushBytes:      4 << 10,
		maxPendingBytes: 1 << 20, // 1MB
	}
}

// SetMasker sets the stdout masker. Must be called before the first Flush.
func (p *LogPusher) SetMasker(m *secrets.Masker) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.masker = m
}

// Write writes bytes into the buffer and flushes if the buffer exceeds the threshold.
func (p *LogPusher) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n, _ := p.buf.Write(b)
	if p.buf.Len() >= p.flushBytes {
		p.flushLocked(context.Background())
	}
	return n, nil
}

// Flush sends all remaining buffered logs to the master server.
// On failure it performs up to 3 exit-time retries (1 second apart).
// Exit-time retries use an independent context so they continue even when stepCtx is cancelled
// (preventing log loss on shutdown).
func (p *LogPusher) Flush(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.flushLocked(ctx)
	if len(p.pending) == 0 {
		return
	}
	// exit-time retry: use an independent context that does not depend on stepCtx cancellation
	retryCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := 0; i < 3 && len(p.pending) > 0; i++ {
		select {
		case <-retryCtx.Done():
			return
		case <-time.After(time.Second):
		}
		p.flushLocked(retryCtx)
	}
}

// flushLocked flushes the buffer via the bulk API while holding the lock.
// It retries pending batches first, then sends the current buffer.
// Batches that fail to send are queued in pending and retried on the next flush.
func (p *LogPusher) flushLocked(ctx context.Context) {
	// 1. Retry pending batches first
	var stillPending []pendingBatch
	for _, b := range p.pending {
		if err := p.client.AppendLogBulk(ctx, p.agentID, p.runID, p.stepIndex, b.reqs); err != nil {
			stillPending = append(stillPending, b)
		}
	}
	p.pending = stillPending

	// 2. Flush the current buffer
	if p.buf.Len() == 0 {
		return
	}
	chunk := p.buf.String()
	p.buf.Reset()

	lines := splitLines(chunk)
	if len(lines) == 0 {
		return
	}

	reqs := make([]api.LogAppendRequest, 0, len(lines))
	now := time.Now().UTC()
	for _, line := range lines {
		maskedLine := line
		if p.masker != nil {
			maskedLine = p.masker.Mask(line)
		}
		reqs = append(reqs, api.LogAppendRequest{
			RunID:     p.runID,
			StepIndex: p.stepIndex,
			Stream:    p.stream,
			Timestamp: now,
			Line:      maskedLine,
		})
	}
	if err := p.client.AppendLogBulk(ctx, p.agentID, p.runID, p.stepIndex, reqs); err != nil {
		p.appendPendingLocked(pendingBatch{reqs: reqs})
	}
}

// appendPendingLocked appends a pending batch and discards old batches if the cap is exceeded.
// At least one (the latest) batch is always retained even if it alone exceeds the cap.
func (p *LogPusher) appendPendingLocked(b pendingBatch) {
	p.pending = append(p.pending, b)
	for len(p.pending) > 1 && p.pendingSizeBytes() > p.maxPendingBytes {
		p.pending = p.pending[1:]
	}
}

// pendingSizeBytes returns the total byte count of all pending batches.
func (p *LogPusher) pendingSizeBytes() int {
	total := 0
	for _, b := range p.pending {
		for _, r := range b.reqs {
			total += len(r.Line)
		}
	}
	return total
}

// splitLines splits a string on newlines and returns a slice of lines.
func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
