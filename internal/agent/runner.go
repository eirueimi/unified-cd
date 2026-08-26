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

// buildBashStepCmd builds the *exec.Cmd for running a native step's script with
// bash. On Windows the script travels via the __UCD_STEP_SCRIPT environment
// variable and the argv is a fixed, backslash-free loader (eval "$__UCD_STEP_SCRIPT"):
// Go's Windows argv escaping halves runs of backslashes before MSYS (Git Bash)
// re-parses the command line, which corrupts any script that spells out
// backslashes (e.g. a sed s|\\...|\\...). The environment block is not subject
// to that escaping, so the bytes survive. On every other platform the script is
// passed directly as the -lc argument, unchanged. baseEnv is the caller's
// already-built StepEnv result; the returned cmd has Env set but leaves
// Stdout/Stderr/Dir to the caller.
func buildBashStepCmd(script string, baseEnv []string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		cmd := exec.Command(findShell(), "-lc", `eval "$__UCD_STEP_SCRIPT"`)
		cmd.Env = append(baseEnv, "__UCD_STEP_SCRIPT="+script)
		return cmd
	}
	cmd := exec.Command(findShell(), "-lc", script)
	cmd.Env = baseEnv
	return cmd
}

// RunStep executes the given script with bash, writing stdout/stderr to the provided writers.
// Returns the exit code and any error. The process is interrupted if the context is cancelled.
// On cancellation the whole process tree is killed (not just the shell), so children the
// shell spawned (e.g. `sleep` from `bash -c 'sleep 120'`) don't survive as orphans — see
// runTreeKilled for why exec.CommandContext alone is not enough.
// Extra environment variables can be supplied via extraEnv in "KEY=VALUE" format.
// exposeEnv is the agent's ExposeEnv allowlist (AgentConfig.ExposeEnv): host
// environment variables named there (and not in the credential denylist) are
// made visible to the step. If workDir is non-empty, the command runs with
// that directory as the working directory.
func RunStep(ctx context.Context, script string, stdout, stderr io.Writer, extraEnv []string, exposeEnv []string, workDir string) (int, error) {
	// Env is set inside buildBashStepCmd (never nil): a nil cmd.Env makes
	// os/exec inherit the agent's whole environment, which is exactly the leak
	// StepEnv exists to prevent.
	cmd := buildBashStepCmd(script, StepEnv(exposeEnv, extraEnv))
	cmd.Stdout = stdout
	cmd.Stderr = stderr
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

// RunStepWithShell executes script as a host process using an explicit
// interpreter argv (shell) instead of the host bash findShell() picks: the
// argv is exec'd verbatim as shell + [script] (mirroring the container exec
// contract — never re-parsed or quoted), so a native step's own `shell:`
// (e.g. [python3, -c]) runs the interpreter the author asked for rather than
// always going through bash -lc. shell must be non-empty; callers gate on
// len(step.Shell) > 0 and fall back to RunStep (today's unconditional bash
// path) otherwise. Cancellation/process-tree-kill semantics mirror RunStep
// (see runTreeKilled). exposeEnv is the agent's ExposeEnv allowlist; see
// RunStep's doc comment.
//
// Windows note: the script is passed as a process argument, so a custom
// interpreter script that contains runs of backslashes can be corrupted by
// Windows argv escaping (Go escapes with MSVCRT rules, MSYS bash parses with
// its own). The default bash path (RunStep) avoids this via the
// __UCD_STEP_SCRIPT environment variable; this explicit-shell path does not yet.
func RunStepWithShell(ctx context.Context, shell []string, script string, stdout, stderr io.Writer, extraEnv []string, exposeEnv []string, workDir string) (int, error) {
	argv := append(append([]string{}, shell[1:]...), script)
	cmd := exec.Command(shell[0], argv...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// Always set Env: a nil cmd.Env makes os/exec inherit the agent's whole
	// environment, which is exactly the leak StepEnv exists to prevent.
	cmd.Env = StepEnv(exposeEnv, extraEnv)
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
// exposeEnv is the agent's ExposeEnv allowlist; see RunStep's doc comment.
// If workDir is non-empty, the command runs with that directory as the working directory.
func RunStepCapture(ctx context.Context, script string, stderr io.Writer, extraEnv []string, exposeEnv []string, workDir string) (stdout string, exitCode int, err error) {
	var stdoutBuf bytes.Buffer
	// Env is set inside buildBashStepCmd (never nil): a nil cmd.Env makes
	// os/exec inherit the agent's whole environment, which is exactly the leak
	// StepEnv exists to prevent.
	cmd := buildBashStepCmd(script, StepEnv(exposeEnv, extraEnv))
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = stderr
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

// logPusherAutoFlushEvery is how often StartAutoFlush ships buffered lines.
// Without a timer a LogPusher only flushes on 4KB boundaries and at step end,
// so sparse output would not reach the WebUI while a step runs. A var so
// tests can shrink it.
var logPusherAutoFlushEvery = 2 * time.Second

// logPusherWriteFlushTimeout bounds how long a synchronous flush triggered
// from Write (on crossing the flushBytes threshold) may block holding p.mu.
// Without a bound, a controller partition could stall the writer (and thus
// the running step) for as long as the underlying HTTP client takes to give
// up. The 2s auto-flush ticker remains the steady drain path; this timeout
// only caps the worst case on the write path. A var so tests can shrink it.
var logPusherWriteFlushTimeout = 5 * time.Second

// logPusherAutoFlushTimeout bounds a single auto-flush pass. StartAutoFlush
// holds p.mu for the whole pass, and Write (and therefore the step's own
// stdout pipe) blocks on that mutex, so an unbounded pass freezes the step
// itself: against a controller that accepts connections and never answers,
// each request runs to the HTTP client's 60s timeout and a single pass was
// measured stalling a step for 176.3s. The value must sit comfortably above a
// healthy round trip (milliseconds to a few hundred ms) and above the worst
// single-request controller service time observed for an oversized bulk body
// (~9.4s), so a slow-but-working flush is not abandoned; 15s does that while
// capping the stall at roughly a tenth of the unbounded case. Abandoning a
// pass costs latency, not lines: the backlog stays in p.pending, in order,
// and the next tick resumes it.
//
// This is the DEFAULT ONLY: NewLogPusher snapshots it into the pusher's own
// autoFlushTimeout field, and StartAutoFlush reads that field. A test must
// shrink the field on the pusher it owns, never this var. Mutating it at test
// time is a data race no lock can fix: the auto-flush goroutine of every OTHER
// live pusher reads it under ITS OWN mutex, so there is no lock the writer
// could take that orders the write against those readers.
var logPusherAutoFlushTimeout = 15 * time.Second

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
	// autoFlushTimeout bounds a single auto-flush pass; see
	// logPusherAutoFlushTimeout, which is its default. Per-pusher rather than
	// global so a test can shrink it without writing state that other
	// pushers' auto-flush goroutines are concurrently reading. Set before
	// StartAutoFlush; the goroutine's read is ordered by its own start.
	autoFlushTimeout time.Duration
	// droppedLines counts log lines discarded by appendPendingLocked's
	// drop-oldest eviction (e.g. during a sustained controller partition).
	// Surfaced as a synthetic marker line on the next successful flush, then
	// reset to 0. Guarded by mu.
	droppedLines int
	// maxLineBytes is the hard ceiling on p.buf before Write forces a flush
	// even though the buffer holds no newline — overriding the "only
	// complete lines" discipline that flushCompleteLinesLocked (and, since
	// this field was added, Write below flushBytes) otherwise enforces.
	//
	// Without a ceiling, a step whose output contains one very long line
	// with no newline (a giant single-line JSON blob, a base64-encoded
	// upload, a progress indicator that uses \r instead of \n) would make
	// p.buf grow for as long as that line does — unbounded, and exactly the
	// failure mode "hold until the newline" would otherwise invite.
	//
	// 256KiB (256 << 10) sits two orders of magnitude above flushBytes
	// (4KiB), comfortably above any legitimate single-line output this
	// codebase is known to produce, while still being a small, fixed
	// fraction (1/4) of maxPendingBytes (1MiB) — so if the controller is
	// unreachable at the moment the ceiling forces a flush, the resulting
	// batch adds at most a small, bounded multiple to the pending backlog
	// instead of opening a second, uncapped growth path alongside it.
	//
	// Crossing this ceiling is the ONE remaining case where a size-triggered
	// flush can still split a value across two LogAppendRequests (see
	// flushForcedLocked): the masking gap this file exists to close narrows
	// from "any chunk boundary, on every flush" to "one specific byte
	// offset, and only for a single line that is itself pathologically
	// long." A struct field (not a package var) so a test can shrink it on
	// the one pusher it owns, the same way maxPendingBytes already is.
	maxLineBytes int
}

// NewLogPusher creates a new LogPusher with the given parameters.
func NewLogPusher(client *Client, agentID, runID string, stepIndex int, stream string) *LogPusher {
	return &LogPusher{
		stream:           stream,
		runID:            runID,
		stepIndex:        stepIndex,
		agentID:          agentID,
		client:           client,
		flushBytes:       4 << 10,
		maxPendingBytes:  1 << 20, // 1MB
		maxLineBytes:     256 << 10,
		autoFlushTimeout: logPusherAutoFlushTimeout,
	}
}

// SetMasker sets the stdout masker. Must be called before the first Flush.
func (p *LogPusher) SetMasker(m *secrets.Masker) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.masker = m
}

// StartAutoFlush ships buffered lines every `every` until ctx is cancelled, so
// output reaches the server while a step is still running. Only COMPLETE lines
// are shipped on a tick — a partial trailing line stays buffered so a line is
// never split across two log entries by flush timing. The caller's final
// Flush ships any remainder.
func (p *LogPusher) StartAutoFlush(ctx context.Context, every time.Duration) {
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				// Bound the pass. The timeout is taken after the lock is
				// acquired so a slow predecessor does not eat this pass's
				// budget, and derives from ctx so step cancellation still
				// propagates.
				p.mu.Lock()
				fctx, cancel := context.WithTimeout(ctx, p.autoFlushTimeout)
				p.flushCompleteLinesLocked(fctx)
				cancel()
				p.mu.Unlock()
			}
		}
	}()
}

// flushCompleteLinesLocked flushes only up to the last newline in the buffer,
// keeping any partial trailing line buffered. Caller must hold p.mu.
func (p *LogPusher) flushCompleteLinesLocked(ctx context.Context) {
	b := p.buf.Bytes()
	i := bytes.LastIndexByte(b, '\n')
	if i < 0 {
		// No complete line yet; still retry previously failed batches.
		if len(p.pending) > 0 {
			_ = p.flushPendingLocked(ctx)
		}
		return
	}
	tail := append([]byte(nil), b[i+1:]...)
	p.buf.Truncate(i + 1)
	p.flushLocked(ctx)
	p.buf.Write(tail)
}

// flushPendingLocked retries previously failed batches oldest-first and STOPS
// AT THE FIRST FAILURE, returning false. Continuing past a failure is what
// reorders a stored log: a fault that fails only some requests lets a newer
// batch land while an older one is still queued, and the controller assigns
// seq on arrival, so the stored order stops matching the emission order
// permanently and for every reader. Aborting the pass keeps the backlog
// contiguous and in order at the cost of head-of-line blocking, which is the
// correct trade for an ordering guarantee. Caller must hold p.mu.
func (p *LogPusher) flushPendingLocked(ctx context.Context) bool {
	for len(p.pending) > 0 {
		if err := p.client.AppendLogBulk(ctx, p.agentID, p.runID, p.stepIndex, p.pending[0].reqs); err != nil {
			return false
		}
		p.pending = p.pending[1:]
	}
	p.pending = nil
	return true
}

// Write writes bytes into the buffer and flushes if the buffer exceeds the
// threshold.
//
// Below maxLineBytes, a size-triggered flush ships only COMPLETE lines — the
// same discipline StartAutoFlush's ticker already applies via
// flushCompleteLinesLocked (see its doc comment) — so a chunk boundary from
// the step's stdout/stderr pipe can never land inside a line. That matters
// because secrets.Masker.Mask matches per LINE (see its doc comment in
// internal/secrets/masker.go): a chunk boundary that fell inside a value
// being masked would split that value across two LogAppendRequests, neither
// fragment would match the masker's pattern on its own, and both would ship
// unmasked — reconstructable by concatenating the stored log. Real process
// stdout arrives in pipe-sized chunks with no relation to log line
// boundaries, so this was not a theoretical gap: flushCompleteLinesLocked
// already closed it for the auto-flush ticker, but Write's own
// threshold flush used to run unconditionally the instant p.buf.Len()
// crossed flushBytes, without regard for whether a newline had been seen.
// That was the leak this fix closes.
//
// Once the buffer grows past maxLineBytes with still no newline to flush up
// to, Write gives up waiting and force-flushes anyway (flushForcedLocked) —
// see maxLineBytes's doc comment for why that ceiling exists and for why
// crossing it is the one remaining case that can still split a value across
// a flush boundary.
func (p *LogPusher) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n, _ := p.buf.Write(b)
	switch {
	case p.buf.Len() >= p.maxLineBytes:
		fctx, cancel := context.WithTimeout(context.Background(), logPusherWriteFlushTimeout)
		p.flushForcedLocked(fctx)
		cancel()
	case p.buf.Len() >= p.flushBytes:
		fctx, cancel := context.WithTimeout(context.Background(), logPusherWriteFlushTimeout)
		p.flushCompleteLinesLocked(fctx)
		cancel()
	}
	return n, nil
}

// flushForcedLocked flushes the buffer via flushLocked even though it may
// still hold a partial trailing line with no newline — the one case Write
// allows once p.buf has crossed maxLineBytes (see that field's doc comment
// for why the ceiling exists). Caller must hold p.mu.
//
// If a partial line is indeed about to ship, this also emits a short,
// best-effort marker naming the fact, so an operator who later finds an
// unmasked secret fragment in a stored log has a concrete mechanism to look
// for instead of having to rediscover this ceiling from scratch: a value
// that straddles the maxLineBytes offset into an otherwise-unbroken line is
// the only way a per-line masker can still miss it after this fix.
//
// The marker is sent directly and only when p.pending is already empty after
// the forced flush (i.e. the forced content itself was fully delivered), and
// it is never retried on failure. This is deliberately weaker than the
// droppedLines marker's guarantee (which retries every subsequent flush
// until delivered — see flushLocked's step 3): droppedLines tracks actual,
// permanent data loss and must eventually be reported, whereas a forced
// split loses no data at all — it only widens, for this one pathologically
// long line, the same narrow masking gap described above. A missed marker
// costs discoverability, not log content, so no retry state is worth adding
// for it.
func (p *LogPusher) flushForcedLocked(ctx context.Context) {
	b := p.buf.Bytes()
	partial := len(b) > 0 && b[len(b)-1] != '\n'

	p.flushLocked(ctx)

	if !partial || len(p.pending) > 0 {
		return
	}
	line := fmt.Sprintf("[unified-cd: a log line exceeded %d bytes without a newline and was flushed early; if a secret value straddles this boundary it may not have been masked]", p.maxLineBytes)
	if p.masker != nil {
		// The template above carries no user content, but every other
		// synthetic line this file emits (the droppedLines marker, and
		// recordPhaseTruncated's line in orchestrator.go) is masked before
		// it ships; matching that keeps this call site from being the one
		// place someone has to reason about separately.
		line = p.masker.Mask(line)
	}
	_ = p.client.AppendLogBulk(ctx, p.agentID, p.runID, p.stepIndex, []api.LogAppendRequest{{
		RunID:     p.runID,
		StepIndex: p.stepIndex,
		Stream:    "stderr",
		Timestamp: time.Now().UTC(),
		Line:      line,
	}})
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
//
// The pass aborts at the first failed batch (see flushPendingLocked) and the
// current buffer is then queued BEHIND the backlog instead of being sent, so
// nothing ever overtakes an older batch on the wire.
func (p *LogPusher) flushLocked(ctx context.Context) {
	// 1. Retry pending batches first, oldest-first, stopping at the first
	//    failure so a later batch cannot overtake an earlier one.
	drained := p.flushPendingLocked(ctx)

	// 2. Flush the current buffer. Note this runs even when the backlog did
	//    NOT drain: the buffer is still emptied, just into p.pending rather
	//    than onto the wire. Returning early here instead — leaving the lines
	//    in p.buf — would preserve order too, but p.buf is unbounded, while
	//    p.pending carries the maxPendingBytes cap, drop-oldest eviction and
	//    the droppedLines accounting. Skipping the drain would therefore trade
	//    a bounded, accounted 1MB backlog for unbounded agent memory growth
	//    that lasts as long as the outage, and would silence the drop marker.
	if p.buf.Len() > 0 {
		chunk := p.buf.String()
		p.buf.Reset()

		lines := splitLines(chunk)
		if len(lines) > 0 {
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
			if !drained {
				// A batch older than this one is still queued; sending now
				// would reorder the stored log.
				p.appendPendingLocked(pendingBatch{reqs: reqs})
			} else if err := p.client.AppendLogBulk(ctx, p.agentID, p.runID, p.stepIndex, reqs); err != nil {
				p.appendPendingLocked(pendingBatch{reqs: reqs})
			}
		}
	}

	// 3. If nothing is left queued (all retries and the current buffer, if
	// any, were sent successfully) and lines were previously discarded by
	// appendPendingLocked's drop-oldest eviction, surface a single synthetic
	// marker line so operators see that logs were lost instead of the gap
	// passing silently.
	if len(p.pending) == 0 && p.droppedLines > 0 {
		dropped := p.droppedLines

		line := fmt.Sprintf("[%d log line(s) dropped: controller unreachable]", dropped)
		if p.masker != nil {
			line = p.masker.Mask(line)
		}
		markerReqs := []api.LogAppendRequest{{
			RunID:     p.runID,
			StepIndex: p.stepIndex,
			Stream:    "stderr",
			Timestamp: time.Now().UTC(),
			Line:      line,
		}}
		// Reset ONLY on confirmed delivery. On failure, leave droppedLines
		// unchanged and do NOT queue the marker in p.pending: a queued marker
		// would become the oldest entry and a later cap overflow would evict
		// it, counting it as a single dropped line and permanently losing the
		// true N. Instead the marker is re-attempted on the next successful
		// flush, still carrying the accurate (and possibly larger) count.
		if err := p.client.AppendLogBulk(ctx, p.agentID, p.runID, p.stepIndex, markerReqs); err == nil {
			p.droppedLines = 0
		}
	}
}

// appendPendingLocked appends a pending batch and discards old batches if the cap is exceeded.
// At least one (the latest) batch is always retained even if it alone exceeds the cap.
func (p *LogPusher) appendPendingLocked(b pendingBatch) {
	p.pending = append(p.pending, b)
	for len(p.pending) > 1 && p.pendingSizeBytes() > p.maxPendingBytes {
		p.droppedLines += len(p.pending[0].reqs)
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
