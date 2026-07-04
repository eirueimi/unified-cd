package agent

import (
	"context"
	"log/slog"
	"os/exec"
)

// runTreeKilled runs cmd to completion, killing the whole process tree
// (not just the direct child) if ctx is cancelled before the command exits.
//
// exec.CommandContext only signals cmd.Process — the direct child (the shell
// launched for a step). When that shell forks children of its own (e.g.
// `bash -c 'sleep 120'` spawning sleep.exe/sleep), killing just the shell
// orphans those children: on Unix they get re-parented and keep running; on
// Windows cmd.Wait() blocks until every process still holding the
// inherited stdout/stderr pipe exits, so the step hangs until the
// grandchild finishes on its own. Using a process group (Unix) or a Job
// Object with KILL_ON_JOB_CLOSE (Windows) via setupProcAttrs/assignJob lets
// killTree take down the entire tree, so Wait returns promptly and no
// process is left behind.
//
// Returns the error from cmd.Wait() (nil on a clean exit). If the context is
// cancelled, the process tree is killed and Wait's error (a "signal: killed"
// or similar) is returned, mirroring the previous exec.CommandContext
// behavior for callers that only check err != nil / *exec.ExitError.
func runTreeKilled(ctx context.Context, cmd *exec.Cmd) error {
	setupProcAttrs(cmd)

	if err := cmd.Start(); err != nil {
		return err
	}

	// Best-effort: if job/group assignment fails, we still run the command
	// (with only the direct child killable on cancel) rather than aborting
	// a step that would otherwise succeed. This is logged (rather than
	// silently swallowed) because it means killTree/cleanupTree will fall
	// back to single-process kill for this step, which can leave
	// grandchildren behind on cancel. Note there is an inherent fork-window
	// race here: assignJob runs after Start, so a child process spawned by
	// cmd in between could, in principle, escape the job/group before
	// assignment completes. Closing that window would require assigning the
	// job before Start (e.g. via a suspended-create + resume dance on
	// Windows), which is out of scope for this fix.
	if err := assignJob(cmd); err != nil {
		slog.Warn("assignJob failed; falling back to single-process kill on cancel", "error", err)
	}

	// cleanupTree releases the platform-specific per-step resource (the
	// Windows Job Object handle; a no-op on Unix) exactly once, regardless
	// of which branch below returns. It is idempotent with killTree: both
	// ultimately call takeJobHandle, so whichever runs first "wins" the
	// handle and the other is a no-op. Without this, every normally
	// completed step would leak one Job Object handle plus its jobHandles
	// map entry (which also pins cmd, preventing GC of its output buffers).
	defer cleanupTree(cmd)

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()

	select {
	case err := <-waitDone:
		return err
	case <-ctx.Done():
		killTree(cmd)
		// Wait for the process (and its Wait goroutine) to actually finish
		// after being killed so we don't leak the goroutine or return before
		// resources are released.
		<-waitDone
		return ctx.Err()
	}
}
