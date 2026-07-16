package agent

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// gcWorkspaces is the opt-in, age-based sweep for per-job host workspace
// directories (audit item 7). Persistent workspaces are a feature — a job's
// working<slot>/<job> directory is an inter-run cache (see claimWorkDir in
// workspace.go) — so this is never run unless an operator opts in via
// AgentConfig.WorkspaceRetentionDays (see agent.go's wiring), and it is
// deliberately conservative:
//
//   - It only ever walks exactly two levels below wsBase:
//     wsBase/working<slot>/<job>. It never removes wsBase itself, never
//     removes a working<slot> directory itself, and never removes (or even
//     descends into) a dot-prefixed entry at either level — in particular
//     wsBase/.ucd-tools, the ucd-sh shim directory InstallShim writes
//     directly under wsBase (see agent.go's InstallShim doc comment), is
//     always skipped because it doesn't match the "working*" glob AND
//     because of the explicit dot-prefix check below.
//   - A <job> directory is removed only if BOTH: its key is absent from
//     active, AND its mtime is strictly older than retention
//     (now.Sub(mtime) > retention — a dir aged exactly at the boundary is
//     kept).
//
// Active-set key: the ABSOLUTE PATH of the working<slot>/<job> directory,
// not a run ID and not a bare sanitized job name. The shared RunSet (see
// runset.go, Task 3) tracks run IDs, which the agent has no way to map back
// to a job/workspace directory without extra bookkeeping. A bare job name
// is also unsafe on its own: claimWorkDir is slot-scoped, so
// working0/foo and working1/foo are two DIFFERENT directories that can
// legitimately be in use by two different concurrent runs of the same job
// — keying by job name alone would conflate them. The caller (agent.go)
// already computes the exact absolute workDir for every in-flight claim, so
// it threads that path straight into a second, workDir-keyed RunSet
// (populated/cleared alongside the existing run-ID activeRuns set) and
// passes its snapshot here as active.
func gcWorkspaces(wsBase string, retention time.Duration, active map[string]struct{}, now time.Time) ([]string, error) {
	var removed []string

	slotDirs, err := filepath.Glob(filepath.Join(wsBase, "working*"))
	if err != nil {
		return nil, fmt.Errorf("glob workspace slot dirs under %s: %w", wsBase, err)
	}

	for _, slotDir := range slotDirs {
		slotInfo, err := os.Stat(slotDir)
		if err != nil {
			slog.Warn("workspace gc: stat slot dir failed", "dir", slotDir, "error", err)
			continue
		}
		if !slotInfo.IsDir() || strings.HasPrefix(filepath.Base(slotDir), ".") {
			// Defensive: the "working*" glob can't match a dot-prefixed name,
			// but never remove/descend into one regardless.
			continue
		}

		entries, err := os.ReadDir(slotDir)
		if err != nil {
			slog.Warn("workspace gc: read slot dir failed", "dir", slotDir, "error", err)
			continue
		}

		for _, entry := range entries {
			if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			jobDir := filepath.Join(slotDir, entry.Name())

			if _, ok := active[jobDir]; ok {
				continue
			}

			jobInfo, err := entry.Info()
			if err != nil {
				slog.Warn("workspace gc: stat job dir failed", "dir", jobDir, "error", err)
				continue
			}
			if now.Sub(jobInfo.ModTime()) <= retention {
				continue
			}

			if err := os.RemoveAll(jobDir); err != nil {
				slog.Warn("workspace gc: remove failed", "dir", jobDir, "error", err)
				continue
			}
			slog.Info("workspace gc: removed aged workspace dir", "dir", jobDir, "age", now.Sub(jobInfo.ModTime()).String())
			removed = append(removed, jobDir)
		}
	}

	return removed, nil
}

// activeWorkDirSet converts a RunSet snapshot (see runset.go) of active
// workspace directory paths into the map shape gcWorkspaces expects.
func activeWorkDirSet(paths []string) map[string]struct{} {
	m := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		m[p] = struct{}{}
	}
	return m
}
