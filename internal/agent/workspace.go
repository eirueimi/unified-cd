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
	out := b.String()
	if out == "" {
		return "job"
	}
	// Reject degenerate results that are unsafe as a path segment: dot-only
	// names (".", "..", "...") escape or self-reference, and Windows reserved
	// device names break on a Windows agent. Upstream DSL validation already
	// blocks these, but this function is the defensive last line.
	if strings.Trim(out, ".") == "" {
		return "job"
	}
	if isWindowsReservedName(out) {
		return "job_" + out
	}
	return out
}

// isWindowsReservedName reports whether name (case-insensitive, ignoring any
// extension) is a Windows reserved device name.
func isWindowsReservedName(name string) bool {
	base := strings.ToUpper(name)
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	switch base {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return true
	}
	return false
}

// claimWorkDir is the per-claim workspace directory: slot level (fixed,
// bounded — the claim-loop concurrency dimension) then job level (so two
// jobs never share a directory). The directory is ephemeral: see
// prepareWorkspace. See the 2026-07-08 job-isolation design.
func claimWorkDir(wsBase string, slot int, jobName string) string {
	return filepath.Join(wsBase, fmt.Sprintf("working%d", slot), sanitizeJobName(jobName))
}

// detachedWorkDir is the per-run workspace directory for a detached run
// (spec.detached), decoupled from the slot-keyed claimWorkDir pool so a detached
// claim never contends on a normal execution slot's workspace. Keyed by run ID
// (not job name) because detached runs get a fresh workspace per run and it is
// removed when the run finishes — there is no cross-run reuse to preserve.
func detachedWorkDir(wsBase, runID string) string {
	return filepath.Join(wsBase, "detached", sanitizeJobName(runID))
}

// removeAll is os.RemoveAll; a variable only so a test can inject the failure a
// real removal produces when a rootful container left root-owned files.
var removeAll = os.RemoveAll

// volumeModesDir is the agent's own record, under the workspace base, of which
// execution mode last used each volume that serves as a run's workspace. It is
// kept outside the volumes so nothing the agent writes appears in a volume's
// contents, and it is dot-prefixed so it can never collide with a volume
// (a volume path under a dot-prefixed entry is rejected) or a job workspace.
const volumeModesDir = ".ucd-volume-modes"

// volumeWorkspace identifies the volume that is a run's workspace (a volume
// mounted at "."). A nil *volumeWorkspace means the run uses the ordinary
// per-job directory, which is ephemeral.
type volumeWorkspace struct {
	// Name is the volume's name, for messages.
	Name string
	// ModeMarker is the file holding the mode that last used the volume.
	ModeMarker string
}

// volumeModeMarker is where the mode marker of volume name lives.
func volumeModeMarker(wsBase, name string) string {
	return filepath.Join(wsBase, volumeModesDir, name)
}

// prepareWorkspace readies workDir for a claim running in mode
// ("native"|"isolated").
//
// An ordinary workspace is ephemeral: it is removed and recreated empty before
// every run whatever clean says, so a run starts empty even if the agent died
// mid-run last time, and the result never depends on which agent type ran the
// job. A removal that fails on permission errors (root-owned files a rootful
// container left behind) is retried through a root cleanup container, and a
// workspace that cannot be emptied fails the run, because a run must not start
// on another run's files.
//
// A workspace that is a volume (vol != nil) is the job's persistence, so it is
// never touched unless clean is true. Its mode marker is kept outside the
// volume (vol.ModeMarker), and a native/isolated flip FAILS the run instead of
// resetting the directory: that reset exists so root-owned leftovers of a
// rootful container cannot break a native run, but on a volume it would delete
// everything in it. clean=true is the explicit way past that, and wipes the
// volume's contents (not the directory, which may be a mount point) with the
// same cleanup-container fallback.
//
// toolsDir is threaded through to containerCleanup so the cleanup container
// carries the same /.ucd mount as every other container this agent creates
// (see claim_pod.go's ucdToolsMount); empty means no shim mount.
func prepareWorkspace(ctx context.Context, workDir, mode string, vol *volumeWorkspace, clean bool, rtFn func() (crt.ContainerRuntime, error), toolsDir string) error {
	if vol == nil {
		if err := wipeDir(ctx, workDir, false, rtFn, toolsDir); err != nil {
			return fmt.Errorf("clean workspace %s: %w", workDir, err)
		}
		if err := os.MkdirAll(workDir, 0o755); err != nil {
			return fmt.Errorf("create workspace %s: %w", workDir, err)
		}
		return nil
	}

	prev, _ := os.ReadFile(vol.ModeMarker)
	if len(prev) > 0 && string(prev) != mode {
		if !clean {
			return fmt.Errorf("volume %q was last used by %s run and this run is %s: the agent will not reset a volume itself, because that would delete its contents. To keep the data, run the job as %s again. To wipe the volume once and continue as %s, set spec.cleanWorkspace: true on the job. The agent records the mode in %s; deleting that file also lets the run proceed without wiping",
				vol.Name, withArticle(string(prev)), mode, string(prev), mode, vol.ModeMarker)
		}
		slog.Info("volume mode changed; wiping it because cleanWorkspace is set", "volume", vol.Name, "from", string(prev), "to", mode)
	}
	if clean {
		if err := wipeDir(ctx, workDir, true, rtFn, toolsDir); err != nil {
			return fmt.Errorf("clean volume %q: %w", vol.Name, err)
		}
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return fmt.Errorf("create workspace %s: %w", workDir, err)
	}
	if err := os.MkdirAll(filepath.Dir(vol.ModeMarker), 0o755); err != nil {
		slog.Warn("create volume mode marker directory failed", "volume", vol.Name, "error", err)
	} else if err := os.WriteFile(vol.ModeMarker, []byte(mode), 0o644); err != nil {
		slog.Warn("write volume mode marker failed", "volume", vol.Name, "error", err)
	}
	return nil
}

// withArticle prefixes a mode with the right indefinite article for a message
// ("an isolated", "a native").
func withArticle(mode string) string {
	if mode != "" && strings.ContainsRune("aeiou", rune(mode[0])) {
		return "an " + mode
	}
	return "a " + mode
}

// finishWorkspace sweeps an ordinary workspace once its run has ended, so the
// run's output does not hold disk until that job's next run. It is best-effort:
// the guarantee of an empty start is prepareWorkspace's, so a failure here is a
// warning and changes nothing about the run. A volume-backed workspace is never
// swept, whatever clean was.
func finishWorkspace(ctx context.Context, workDir string, vol *volumeWorkspace, rtFn func() (crt.ContainerRuntime, error), toolsDir string) {
	if vol != nil {
		return
	}
	if err := wipeDir(ctx, workDir, false, rtFn, toolsDir); err != nil {
		slog.Warn("workspace sweep after the run failed; the next run cleans it before starting", "dir", workDir, "error", err)
	}
}

// wipeDir deletes dir (or, with contentsOnly, everything in it but not dir
// itself), retrying through a root cleanup container when a plain removal
// fails. A missing dir is not an error. Links inside dir are removed, never
// followed.
func wipeDir(ctx context.Context, dir string, contentsOnly bool, rtFn func() (crt.ContainerRuntime, error), toolsDir string) error {
	remove := func() error {
		if !contentsOnly {
			return removeAll(dir)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		var first error
		for _, e := range entries {
			if err := removeAll(filepath.Join(dir, e.Name())); err != nil && first == nil {
				first = err
			}
		}
		return first
	}
	err := remove()
	if err == nil {
		return nil
	}
	slog.Warn("workspace clean failed; retrying via cleanup container", "dir", dir, "error", err)
	if cerr := containerCleanup(ctx, dir, rtFn, toolsDir); cerr != nil {
		return fmt.Errorf("%w (cleanup container: %v)", err, cerr)
	}
	return remove()
}

// containerCleanup deletes workDir's contents as root via a throwaway
// container, for files a rootful container runtime left owned by root.
func containerCleanup(ctx context.Context, workDir string, rtFn func() (crt.ContainerRuntime, error), toolsDir string) error {
	rt, err := rtFn()
	if err != nil {
		return fmt.Errorf("no container runtime for cleanup: %w", err)
	}
	// Entrypoint is the explicit ucd-sh pause keep-alive: this container must
	// stay running between Create and the Exec below (see
	// crt.CreateSpec.Entrypoint) — busybox's default entrypoint would
	// otherwise read stdin, hit EOF immediately, and exit before Exec runs.
	// The /.ucd mount is what makes that keep-alive binary available.
	mounts := append([]crt.Mount{{HostPath: workDir, ContainerPath: "/w"}}, ucdToolsMount(toolsDir)...)
	h, err := rt.Create(ctx, crt.CreateSpec{
		Image:      "busybox",
		WorkDir:    "/w",
		Mounts:     mounts,
		Entrypoint: ucdShPause,
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
