package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
)

// volumeLinksFile records, under the workspace base, every link a native run
// has created into a volume. A crash cannot run the run's cleanup, and an
// absolute mount path can be anywhere on the machine, so the startup sweep
// (sweepStaleVolumeLinks) needs a list to find them by.
const volumeLinksFile = ".ucd-volume-links"

// A note on mount paths, because this file deliberately does what
// pathguard.go forbids.
//
// ContainWithinOS and ContainWithinSlash reject absolute paths for artifact
// and cache paths: those come from the job and must not reach outside the
// workspace. A volume mountPath is different. The operator declares which
// volumes exist in the agent's own config, and a job can only name one of
// them, so what an absolute mountPath can reach is bounded by the agent's
// configuration rather than by the job author. Volume mount paths therefore
// do NOT go through those guards. What is still enforced here is narrower:
// a relative path may not climb out of the workspace, an absolute native path
// may not land inside it, and nothing this agent did not create is ever
// replaced. A native job can already run arbitrary commands as the agent user,
// so an absolute mount path grants it no privilege it did not have.

// mountSpec is one volume entry after its name has been chosen.
type mountSpec struct {
	name    string // the volume
	alias   string // key in the resolved map
	volPath string // the volume's host directory
	mp      string // cleaned, slash-separated mountPath
	abs     bool   // mp is absolute
	root    bool   // mp is the workspace root
}

// planMounts validates mounts and resolves each to a mountSpec. Every entry
// must already name exactly one volume: the claim loop picks among candidates
// when it reserves them (reserveRunVolumes), before anything is mounted.
func planMounts(mounts []dsl.VolumeMount, m *VolumeManager) ([]mountSpec, error) {
	if len(mounts) == 0 {
		return nil, nil
	}
	if m == nil {
		return nil, errors.New("this job mounts volumes, but this agent declares none")
	}
	specs := make([]mountSpec, 0, len(mounts))
	for i, v := range mounts {
		if len(v.Name) != 1 {
			return nil, fmt.Errorf("spec.volumes[%d] names %d candidate volumes; exactly one must be chosen before mounting", i, len(v.Name))
		}
		name := v.Name[0]
		volPath := m.PathOf(name)
		if volPath == "" {
			return nil, fmt.Errorf("spec.volumes[%d]: this agent does not declare a volume named %q", i, name)
		}
		alias := dsl.VolumeAlias(v)
		if alias == "" {
			return nil, fmt.Errorf("spec.volumes[%d]: volume %q has no alias", i, name)
		}
		if v.MountPath == "" {
			return nil, fmt.Errorf("spec.volumes[%d]: mountPath is required", i)
		}
		mp := path.Clean(filepath.ToSlash(v.MountPath))
		s := mountSpec{name: name, alias: alias, volPath: volPath, mp: mp, abs: path.IsAbs(mp), root: mp == "."}
		switch {
		case s.root && m.ModeOf(name) == VolumeModeShared:
			// Only the agent can know a volume's mode, so this is rejected
			// here and not when the job is applied.
			return nil, fmt.Errorf("spec.volumes[%d]: volume %q is shared and cannot be mounted at the workspace root (mountPath %q): several runs would work in one checkout with no exclusion. Declare it exclusive in the agent config, or mount it at a subdirectory", i, name, v.MountPath)
		case !s.abs && (mp == ".." || strings.HasPrefix(mp, "../")):
			return nil, fmt.Errorf("spec.volumes[%d]: mountPath %q escapes the workspace", i, v.MountPath)
		}
		specs = append(specs, s)
	}
	return specs, nil
}

// workspaceVolumeDir returns the host directory of the volume mounted at the
// workspace root ("."), or "" when there is none. A run with a root volume
// works in that directory itself: the workspace is the volume, so nothing is
// linked and nothing needs replacing.
func workspaceVolumeDir(mounts []dsl.VolumeMount, m *VolumeManager) string {
	if name := workspaceVolumeName(mounts); name != "" {
		return m.PathOf(name)
	}
	return ""
}

// workspaceVolumeName is the name of the volume mounted at the workspace root
// ("."), or "" when there is none.
func workspaceVolumeName(mounts []dsl.VolumeMount) string {
	for _, v := range mounts {
		if len(v.Name) == 1 && path.Clean(filepath.ToSlash(v.MountPath)) == "." {
			return v.Name[0]
		}
	}
	return ""
}

// mountVolumes realises mounts for a run whose workspace is workDir, and
// returns each alias's path as the job sees it plus the func that undoes it.
// Every entry must already name one volume (see reserveRunVolumes).
//
// native is true for a run with no container. There every mount path, relative
// or absolute, is a link inside or outside workDir into the volume's directory:
// a bind mount needs root on Linux and does not exist on macOS. The link is
// never allowed to replace anything this agent did not create, and cleanup
// removes only links this agent created.
//
// A volume at the workspace root is not linked at all. The caller runs the job
// in the volume directory (workspaceVolumeDir) and passes that directory as
// workDir, so any other relative mounts land inside it.
//
// When native is false the run is isolated: the container backend bind-mounts
// each volume (isolatedVolumeMounts), so the only host-side work is creating
// the mountpoint directories as the agent user instead of leaving the runtime
// to create them as root, and resolved is nil because the paths the job sees
// are container paths.
func mountVolumes(workDir string, mounts []dsl.VolumeMount, m *VolumeManager, native bool) (map[string]string, func() error, error) {
	noop := func() error { return nil }
	specs, err := planMounts(mounts, m)
	if err != nil {
		return nil, noop, err
	}
	if len(specs) == 0 {
		return map[string]string{}, noop, nil
	}

	if !native {
		for _, s := range specs {
			if s.root || s.abs {
				continue
			}
			if err := os.MkdirAll(filepath.Join(workDir, filepath.FromSlash(s.mp)), 0o755); err != nil {
				return nil, noop, fmt.Errorf("create mountpoint for volume %q: %w", s.name, err)
			}
		}
		return nil, noop, nil
	}

	type createdLink struct {
		target string
		mp     string // the mountPath as the job wrote it, cleaned
		abs    bool
	}
	var created []createdLink
	cleanup := func() error {
		var errs []error
		for i := len(created) - 1; i >= 0; i-- {
			l := created[i]
			if err := m.removeOurLink(l.target); err != nil {
				errs = append(errs, err)
				if l.abs {
					// The caller releases the run's reservations right after
					// this; keep this one. See pinPath for why an absolute
					// link is treated differently from a relative one.
					m.pinPath(l.mp)
					slog.Error("an absolute volume mount path could not be unlinked and stays reserved until the agent restarts", "path", l.target, "error", err)
				}
			}
		}
		created = nil
		return errors.Join(errs...)
	}

	resolved := make(map[string]string, len(specs))
	for _, s := range specs {
		if s.root {
			resolved[s.alias] = s.volPath
			continue
		}
		target, err := nativeLinkPath(workDir, s)
		if err != nil {
			_ = cleanup()
			return nil, noop, err
		}
		if err := m.installLink(target, s.volPath); err != nil {
			_ = cleanup()
			return nil, noop, fmt.Errorf("mount volume %q at %q: %w", s.name, s.mp, err)
		}
		created = append(created, createdLink{target: target, mp: s.mp, abs: s.abs})
		resolved[s.alias] = target
	}
	return resolved, cleanup, nil
}

// nativeLinkPath is where a native run's link for s lives on the host.
func nativeLinkPath(workDir string, s mountSpec) (string, error) {
	if !s.abs {
		return filepath.Join(workDir, filepath.FromSlash(s.mp)), nil
	}
	// A POSIX-rooted path. On Windows that means "rooted at the current
	// drive"; Abs supplies it.
	target, err := filepath.Abs(filepath.FromSlash(s.mp))
	if err != nil {
		return "", fmt.Errorf("resolve mountPath %q: %w", s.mp, err)
	}
	// The apply-time rule ("an absolute mountPath inside the workspace is
	// rejected") could only compare against the container's workspace root. A
	// host agent's workspace is a host directory the job cannot know, so this
	// is where that check is made for a native run.
	resolvedWS, _ := filepath.EvalSymlinks(workDir)
	if pathWithinFold(workDir, target) || (resolvedWS != "" && pathWithinFold(resolvedWS, target)) {
		return "", fmt.Errorf("absolute mountPath %q resolves to %s, which is inside the workspace %s; use a relative mountPath instead", s.mp, target, workDir)
	}
	return target, nil
}

// installLink creates target as a link to volPath, refusing to replace
// anything this agent did not create. A link left by an earlier run that died
// before cleanup (it points into one of this agent's volumes) is replaced.
func (m *VolumeManager) installLink(target, volPath string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create parent of %s: %w", target, err)
	}
	fi, err := os.Lstat(target)
	switch {
	case err == nil:
		if dest, ok := readOurLink(fi, target); ok && m.ownsLinkTarget(dest) {
			if rerr := os.Remove(target); rerr != nil {
				return fmt.Errorf("replace stale link %s: %w", target, rerr)
			}
		} else {
			return fmt.Errorf("%s already exists and was not created by this agent as a volume link, so it will not be replaced; remove or rename it, or choose a different mountPath", target)
		}
	case !os.IsNotExist(err):
		return fmt.Errorf("inspect %s: %w", target, err)
	}

	// Record before creating: a crash between the two then leaves a registry
	// entry for a link that does not exist (harmless to sweep) rather than a
	// link nothing remembers.
	if err := m.recordLink(target); err != nil {
		return err
	}
	if err := makeDirLink(volPath, target); err != nil {
		m.forgetLink(target)
		return err
	}
	return nil
}

// removeOurLink removes link if, and only if, it is still a link into one of
// this agent's volumes. Something else in its place is left alone.
func (m *VolumeManager) removeOurLink(link string) error {
	fi, err := os.Lstat(link)
	if os.IsNotExist(err) {
		m.forgetLink(link)
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", link, err)
	}
	dest, ok := readOurLink(fi, link)
	if !ok || !m.ownsLinkTarget(dest) {
		slog.Warn("volume link was replaced by something else; leaving it in place", "path", link)
		m.forgetLink(link)
		return nil
	}
	if err := m.removeLink(link); err != nil {
		// Stays in the registry, so the next start's sweep retries it.
		return fmt.Errorf("remove volume link %s: %w", link, err)
	}
	m.forgetLink(link)
	return nil
}

// ownsLinkTarget reports whether dest lies inside one of this agent's volume
// directories. That is how a link is recognised as one this agent made: no
// other link is ever removed or replaced.
func (m *VolumeManager) ownsLinkTarget(dest string) bool {
	for _, v := range m.vols {
		if pathWithinFold(v.path, dest) {
			return true
		}
	}
	return false
}

// readOurLink returns a link's destination. ok is false when p is not a link
// at all (a directory, a file) or its destination cannot be read.
func readOurLink(fi os.FileInfo, p string) (dest string, ok bool) {
	mode := fi.Mode()
	isLink := mode&os.ModeSymlink != 0
	// Since Go 1.23 a Windows directory junction reports ModeIrregular, not
	// ModeSymlink. Readlink still resolves it, and fails for any other
	// irregular file, which is then not a link.
	if !isLink && runtime.GOOS == "windows" && mode&os.ModeIrregular != 0 {
		isLink = true
	}
	if !isLink {
		return "", false
	}
	d, err := os.Readlink(p)
	if err != nil {
		return "", false
	}
	// Windows can report a link destination with a namespace prefix.
	d = strings.TrimPrefix(d, `\??\`)
	d = strings.TrimPrefix(d, `\\?\`)
	if !filepath.IsAbs(d) {
		d = filepath.Join(filepath.Dir(p), d)
	}
	return filepath.Clean(d), true
}

// pathWithinFold is pathWithin that ignores case where the filesystem does
// (Windows, and macOS by default). Declining to match on a case difference
// elsewhere would make the agent refuse to recognise its own links.
func pathWithinFold(parent, child string) bool {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		parent, child = strings.ToLower(parent), strings.ToLower(child)
	}
	return pathWithin(parent, child)
}

// makeDirLink makes link a directory link to target. It tries a symlink, and
// on Windows falls back to a directory junction: creating a symlink there needs
// a privilege an ordinary agent account lacks (SeCreateSymbolicLinkPrivilege,
// or Developer Mode), while a junction to a local directory does not. If
// neither works the error says so, and the run fails at mount time rather than
// the agent refusing to start: whether an agent can mount at all depends on the
// path a job asks for, which is not known until then.
func makeDirLink(target, link string) error {
	err := os.Symlink(target, link)
	if err == nil {
		return nil
	}
	if runtime.GOOS != "windows" {
		return fmt.Errorf("create symlink %s -> %s: %w", link, target, err)
	}
	if jerr := makeJunction(target, link); jerr != nil {
		return fmt.Errorf("cannot link %s to %s on Windows: creating a symlink failed (%v; it needs administrator rights or Developer Mode) and so did creating a directory junction (%v; it needs an NTFS volume and a path without cmd.exe metacharacters)", link, target, err, jerr)
	}
	return nil
}

// makeJunction creates a directory junction with mklink /J, which exists only
// as a cmd.exe builtin.
func makeJunction(target, link string) error {
	const meta = `&|<>^%!"`
	if strings.ContainsAny(target+link, meta) {
		return fmt.Errorf("a path contains one of %s", meta)
	}
	out, err := exec.Command("cmd.exe", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mklink /J: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// --- the link registry ---

func (m *VolumeManager) linkRegistryPath() string {
	return filepath.Join(m.base, volumeLinksFile)
}

// readLinkRegistry loads the registry; a missing file is an empty registry.
// m.linkMu must be held.
func (m *VolumeManager) readLinkRegistry() ([]string, error) {
	b, err := os.ReadFile(m.linkRegistryPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var links []string
	if err := json.Unmarshal(b, &links); err != nil {
		return nil, fmt.Errorf("parse %s: %w", m.linkRegistryPath(), err)
	}
	return links, nil
}

// writeLinkRegistry replaces the registry atomically (write aside, rename), so
// a crash mid-write cannot leave it truncated. m.linkMu must be held.
func (m *VolumeManager) writeLinkRegistry(links []string) error {
	p := m.linkRegistryPath()
	if len(links) == 0 {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	b, err := json.Marshal(links)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// recordLink adds link to the registry. Failing to record is an error: a link
// the sweep cannot find would outlive a crash.
func (m *VolumeManager) recordLink(link string) error {
	m.linkMu.Lock()
	defer m.linkMu.Unlock()
	links, err := m.readLinkRegistry()
	if err != nil {
		// A damaged registry cannot be trusted, but dropping it would forget
		// links it lists. Start a fresh one and say so.
		slog.Warn("volume link registry unreadable; starting a new one", "error", err)
		links = nil
	}
	for _, l := range links {
		if l == link {
			return nil
		}
	}
	if err := m.writeLinkRegistry(append(links, link)); err != nil {
		return fmt.Errorf("record volume link %s: %w", link, err)
	}
	return nil
}

// forgetLink removes link from the registry, best effort.
func (m *VolumeManager) forgetLink(link string) {
	m.linkMu.Lock()
	defer m.linkMu.Unlock()
	links, err := m.readLinkRegistry()
	if err != nil {
		return
	}
	kept := links[:0]
	for _, l := range links {
		if l != link {
			kept = append(kept, l)
		}
	}
	if err := m.writeLinkRegistry(kept); err != nil {
		slog.Warn("could not update the volume link registry", "error", err)
	}
}

// sweepStaleVolumeLinks removes the links an earlier agent process created and
// never cleaned up, because it died mid-run. It runs once at startup, before
// the first claim, when every volume is free: the agent has lost all its runs.
//
// Only registry entries that are still links into one of this agent's volumes
// are removed. An entry something else has since replaced (a directory, a link
// pointing elsewhere) is left alone: unified-cd does not delete what it did not
// create. Entries that could not be removed stay in the registry for the next
// start.
func sweepStaleVolumeLinks(m *VolumeManager) error {
	if m == nil {
		return nil
	}
	m.linkMu.Lock()
	links, err := m.readLinkRegistry()
	m.linkMu.Unlock()
	if err != nil {
		return err
	}
	var errs []error
	var removed []string
	for _, l := range links {
		fi, serr := os.Lstat(l)
		if serr == nil {
			if dest, ok := readOurLink(fi, l); ok && m.ownsLinkTarget(dest) {
				if rerr := m.removeLink(l); rerr != nil {
					errs = append(errs, fmt.Errorf("remove stale volume link %s: %w", l, rerr))
					continue
				}
				removed = append(removed, l)
			}
		}
		m.forgetLink(l)
	}
	if len(removed) > 0 {
		slog.Info("removed volume links left by a previous agent process", "count", len(removed), "links", removed)
	}
	return errors.Join(errs...)
}

// --- reservation for a run ---

// reserveRunVolumes holds, for runID, every volume the run mounts and (for a
// native run) every absolute host path it links, all at once, waiting until the
// whole set can be taken. It returns mounts with each entry's candidate list
// narrowed to the one volume chosen, and a release func for everything it took.
// The caller releases once, when the run ends: one volume named by two entries
// is held by one reservation, so releasing per entry would free it while the run
// still uses its other mount path.
//
// Acquisition is atomic (VolumeManager.ReserveSet): a run that is waiting holds
// no volume and no path, so two runs cannot each wait for what the other holds,
// however their candidate lists overlap and whether the contended resources are
// volumes, absolute paths, or one of each. That is why no ordering of entries is
// needed. Each entry gets a distinct volume when the candidates allow; one is
// reused for a second entry only when every candidate of that entry is already
// chosen (the same single name written twice).
//
// The wait ends, holding nothing, when ctx is done. onWait, if non-nil, is
// called once, with a description, the first time the run has to wait.
func reserveRunVolumes(ctx context.Context, m *VolumeManager, runID string, mounts []dsl.VolumeMount, native bool, onWait func(what string)) ([]dsl.VolumeMount, func(), error) {
	chosen := make([]dsl.VolumeMount, len(mounts))
	sets := make([][]string, len(mounts))
	for i, v := range mounts {
		chosen[i] = v
		sets[i] = append([]string(nil), v.Name...)
	}
	var abs []string
	if native {
		seen := map[string]bool{}
		for _, v := range mounts {
			if p := path.Clean(filepath.ToSlash(v.MountPath)); path.IsAbs(p) && !seen[p] {
				seen[p] = true
				abs = append(abs, p)
			}
		}
		sort.Strings(abs)
	}
	var wait func()
	if onWait != nil {
		wait = func() { onWait(fmt.Sprintf("volumes %q and paths %q", sets, abs)) }
	}
	names, release, err := m.ReserveSet(ctx, runID, sets, abs, wait)
	if err != nil {
		return nil, nil, err
	}
	for i, n := range names {
		chosen[i].Name = dsl.StringOrSlice{n}
	}
	return chosen, release, nil
}

// errRunTerminal is returned by reserveClaimVolumes when, while the run waited,
// the controller reported it terminal (cancelled by a user, or failed by the
// reaper). There is nothing left to report for such a run.
var errRunTerminal = errors.New("the controller reports the run finished while it waited for its volumes")

// reserveClaimVolumes holds every volume the claim mounts, waiting while another
// run on this agent holds one, and narrows c.Volumes to the volumes chosen. The
// returned func measures each volume's usage (advisory) and releases the lot; it
// is called once, when the run and its unmounts are done.
//
// The wait ends, and the run is failed with the reason, when the agent starts
// draining (claimCtx) or shuts down (runCtx). A drain lets running work finish,
// but this run has not started, and waiting out a holder (a Unity build runs for
// tens of minutes) would hold a slot and stall the drain. Nothing proceeds
// unmounted.
//
// The wait also ends, with errRunTerminal, when the controller reports the run
// terminal. RunClaim's own cancel poller does not exist yet at this point (the
// run context carries agent-level cancellation only), so a user cancelling a
// run that is waiting would otherwise leave it holding its slot until the
// holder finished. The poll starts only if the run actually has to wait.
func (a *Agent) reserveClaimVolumes(claimCtx, runCtx context.Context, c *api.ClaimResponse) (func(), error) {
	waitCtx, cancel := context.WithCancel(runCtx)
	var pollers sync.WaitGroup
	defer pollers.Wait() // registered first, so it runs after cancel
	defer cancel()
	stop := context.AfterFunc(claimCtx, cancel)
	defer stop()

	pollInterval := CancelPollInterval
	var terminal atomic.Bool
	var pollOnce sync.Once
	onWait := func(what string) {
		msg := fmt.Sprintf("waiting for %s, which another run on this agent is using", what)
		slog.Info(msg, "runId", c.RunID)
		// Best effort: without this line the run looks stuck, but its absence
		// must not change what the run does. waitCtx, so a hung controller
		// cannot hold up the drain this wait exists to obey.
		_ = a.Client.AppendLogBulk(waitCtx, a.ID, c.RunID, -1, []api.LogAppendRequest{{
			RunID: c.RunID, StepIndex: -1, Stream: "stderr", Timestamp: time.Now().UTC(), Line: msg,
		}})
		pollOnce.Do(func() {
			pollers.Add(1)
			go func() {
				defer pollers.Done()
				ticker := time.NewTicker(pollInterval)
				defer ticker.Stop()
				for {
					select {
					case <-waitCtx.Done():
						return
					case <-ticker.C:
						run, err := a.Client.GetRun(waitCtx, c.RunID)
						if err != nil {
							continue
						}
						if isTerminalRunStatus(run.Status) {
							terminal.Store(true)
							cancel()
							return
						}
					}
				}
			}()
		})
	}
	chosen, release, err := reserveRunVolumes(waitCtx, a.Volumes, c.RunID, c.Volumes, c.Native, onWait)
	if err != nil {
		if terminal.Load() {
			return nil, errRunTerminal
		}
		if claimCtx.Err() != nil {
			return nil, fmt.Errorf("run abandoned: the agent is draining and the run was still waiting for its volumes: %w", err)
		}
		return nil, fmt.Errorf("could not reserve the run's volumes: %w", err)
	}
	c.Volumes = chosen
	return func() {
		measured := map[string]bool{}
		for _, v := range chosen {
			if n := v.Name[0]; !measured[n] {
				measured[n] = true
				a.Volumes.CheckUsage(n)
			}
		}
		release()
	}, nil
}

// --- isolated runs ---

// volumeMountRef names a volume an isolated run bind-mounts at a path inside
// the workspace.
type volumeMountRef struct {
	Volume    string
	MountPath string // cleaned, slash-separated, relative to the workspace
	HostPath  string // the volume's host directory: what the container sees at MountPath
}

// relativeVolumeMounts lists the volumes mounted at a non-root path inside the
// workspace. A root volume is the host workspace itself and an absolute path is
// a container path outside it, so neither can hide files from a host-side read.
func relativeVolumeMounts(mounts []dsl.VolumeMount, m *VolumeManager) ([]volumeMountRef, error) {
	specs, err := planMounts(mounts, m)
	if err != nil {
		return nil, err
	}
	var refs []volumeMountRef
	for _, s := range specs {
		if !s.root && !s.abs {
			refs = append(refs, volumeMountRef{Volume: s.name, MountPath: s.mp, HostPath: s.volPath})
		}
	}
	return refs, nil
}

// resolveInVolumeMount maps a host-side artifact or cache path (resolved, which
// ContainWithinOS has already confined to workDir) that lies inside a volume
// bind-mounted into the container onto that volume's own host directory.
// Artifact and cache steps read and write the HOST side, where the mount path
// is an empty directory; the files are in the volume, which is what the
// container sees there and what a Kubernetes sidecar reads.
//
// When a path is inside more than one mount (a volume at a sub-path and another
// nested under it), the deepest wins, as it does inside the container. A path
// inside none is returned unchanged.
func resolveInVolumeMount(workDir, resolved string, refs []volumeMountRef) string {
	if len(refs) == 0 {
		return resolved
	}
	rel, err := filepath.Rel(workDir, resolved)
	if err != nil {
		return resolved
	}
	rel = path.Clean(filepath.ToSlash(rel))
	var best *volumeMountRef
	for i := range refs {
		r := &refs[i]
		if rel != r.MountPath && !strings.HasPrefix(rel, r.MountPath+"/") {
			continue
		}
		if best == nil || len(r.MountPath) > len(best.MountPath) {
			best = r
		}
	}
	if best == nil {
		return resolved
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(rel, best.MountPath), "/")
	return filepath.Join(best.HostPath, filepath.FromSlash(rest))
}

// isolatedVolumeMounts maps mounts onto the container backend. rootHost is the
// host directory of the volume mounted at the workspace root ("" if none): the
// claim pod bind-mounts it as the workspace in place of the per-run directory.
// extra are the remaining volumes, each bind-mounted at its own path inside the
// container: relative to containerRoot, or absolute as written, which is a
// container path and so touches nothing on the host. resolved maps each alias
// to the path the job sees inside the container.
func isolatedVolumeMounts(containerRoot string, mounts []dsl.VolumeMount, m *VolumeManager) (rootHost string, extra []crt.Mount, resolved map[string]string, err error) {
	specs, err := planMounts(mounts, m)
	if err != nil {
		return "", nil, nil, err
	}
	resolved = make(map[string]string, len(specs))
	for _, s := range specs {
		switch {
		case s.root:
			rootHost = s.volPath
			resolved[s.alias] = containerRoot
		case s.abs:
			extra = append(extra, crt.Mount{HostPath: s.volPath, ContainerPath: s.mp})
			resolved[s.alias] = s.mp
		default:
			cp := path.Join(containerRoot, s.mp)
			extra = append(extra, crt.Mount{HostPath: s.volPath, ContainerPath: cp})
			resolved[s.alias] = cp
		}
	}
	return rootHost, extra, resolved, nil
}
