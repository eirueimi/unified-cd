package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	crt "github.com/eirueimi/unified-cd/internal/runtime"
)

// modeMarkerFile records which execution mode (native|isolated) last used a
// per-job workspace directory. A mismatch on the next claim (the job
// definition flipped native↔isolated) forces a directory reset so root-owned
// leftovers from a previous isolated run can never break a native run.
const modeMarkerFile = ".ucd-mode"

// sanitizeJobName makes a job name safe as a single path segment. Job names
// are already restricted by DSL validation; this is a defensive escape.
func sanitizeJobName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "job"
	}
	return b.String()
}

// claimWorkDir is the per-claim workspace directory: slot level (fixed,
// bounded — the claim-loop concurrency dimension) then job level (so two
// jobs never share a directory and carry-over is always "this job's own
// previous state"). See the 2026-07-08 job-isolation design.
func claimWorkDir(wsBase string, slot int, jobName string) string {
	return filepath.Join(wsBase, fmt.Sprintf("working%d", slot), sanitizeJobName(jobName))
}

// prepareWorkspace readies workDir for a claim running in mode
// ("native"|"isolated"): resets the directory when cleaning is requested OR
// the recorded mode flipped, falling back to a root cleanup container when a
// plain RemoveAll hits permission errors (root-owned files written by
// rootful-docker containers), then ensures the directory exists and records
// the mode marker.
func prepareWorkspace(ctx context.Context, workDir, mode string, clean bool, rtFn func() (crt.ContainerRuntime, error)) error {
	prev, _ := os.ReadFile(filepath.Join(workDir, modeMarkerFile))
	flipped := len(prev) > 0 && string(prev) != mode
	if clean || flipped {
		if flipped && !clean {
			slog.Info("workspace mode changed; resetting directory", "dir", workDir, "from", string(prev), "to", mode)
		}
		if err := os.RemoveAll(workDir); err != nil {
			slog.Warn("workspace clean failed; retrying via cleanup container", "dir", workDir, "error", err)
			if cerr := containerCleanup(ctx, workDir, rtFn); cerr != nil {
				slog.Warn("cleanup container failed; proceeding with dirty workspace", "dir", workDir, "error", cerr)
			} else if err := os.RemoveAll(workDir); err != nil {
				slog.Warn("workspace clean still failing after cleanup container", "dir", workDir, "error", err)
			}
		}
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return fmt.Errorf("create workspace %s: %w", workDir, err)
	}
	if err := os.WriteFile(filepath.Join(workDir, modeMarkerFile), []byte(mode), 0o644); err != nil {
		slog.Warn("write workspace mode marker failed", "dir", workDir, "error", err)
	}
	return nil
}

// containerCleanup deletes workDir's contents as root via a throwaway
// container, for files a rootful container runtime left owned by root.
func containerCleanup(ctx context.Context, workDir string, rtFn func() (crt.ContainerRuntime, error)) error {
	rt, err := rtFn()
	if err != nil {
		return fmt.Errorf("no container runtime for cleanup: %w", err)
	}
	h, err := rt.Create(ctx, crt.CreateSpec{
		Image:   "busybox",
		WorkDir: "/w",
		Mounts:  []crt.Mount{{HostPath: workDir, ContainerPath: "/w"}},
	})
	if err != nil {
		return err
	}
	defer func() { _ = rt.Remove(ctx, h) }()
	ec, err := rt.Exec(ctx, h, crt.ExecSpec{Script: "rm -rf /w/* /w/.[!.]* /w/..?* 2>/dev/null; true"}, io.Discard, io.Discard)
	if err != nil {
		return err
	}
	if ec != 0 {
		return fmt.Errorf("cleanup container exited %d", ec)
	}
	return nil
}
