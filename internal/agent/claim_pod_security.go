package agent

import (
	"context"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/eirueimi/unified-cd/internal/dsl"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
)

// podSecurityContext returns pt's pod-level securityContext map, or nil.
func podSecurityContext(pt *dsl.PodTemplate) map[string]any {
	if pt == nil {
		return nil
	}
	sc, _ := pt.Spec["securityContext"].(map[string]any)
	return sc
}

// groupAdd lists the supplementary groups a container gets: the pod's
// supplementalGroups followed by fsGroup (design 5.1), without duplicates.
// nil when there are none, so a pod with no securityContext adds no flag.
func groupAdd(eff dsl.EffectiveSecurity) []string {
	var out []string
	seen := map[int64]bool{}
	add := func(g int64) {
		if !seen[g] {
			seen[g] = true
			out = append(out, strconv.FormatInt(g, 10))
		}
	}
	for _, g := range eff.SupplementalGroups {
		add(g)
	}
	if eff.FSGroup != nil {
		add(*eff.FSGroup)
	}
	return out
}

// checkMappedRange fails when an ID the pod asks for lies above max, the
// highest ID the runtime's user namespace maps (design 5.4). max 0 = no limit.
func checkMappedRange(eff dsl.EffectiveSecurity, max int64) error {
	if max <= 0 {
		return nil
	}
	check := func(field string, id int64) error {
		if id > max {
			return fmt.Errorf("%s %d is not mapped by this agent's rootless container runtime (mapped container IDs 0-%d); "+
				"use an ID within that range or run the job on a Kubernetes agent", field, id, max)
		}
		return nil
	}
	if eff.UID != nil {
		if err := check("runAsUser", *eff.UID); err != nil {
			return err
		}
	}
	if eff.GID != nil {
		if err := check("runAsGroup", *eff.GID); err != nil {
			return err
		}
	}
	for _, g := range eff.SupplementalGroups {
		if err := check("supplementalGroups", g); err != nil {
			return err
		}
	}
	if eff.FSGroup != nil {
		return check("fsGroup", *eff.FSGroup)
	}
	return nil
}

// resolveUser returns the CreateSpec.User for def, running the runAsNonRoot
// and image-user checks (design 5.3) first. The image config is only read
// when it is needed: runAsNonRoot without runAsUser, or runAsGroup without
// runAsUser. A runtime that cannot report it fails the run rather than guess.
func (m *claimPodManager) resolveUser(ctx context.Context, def containerDef) (string, error) {
	eff := def.Security
	var imageUser string
	if eff.UID == nil && (eff.RunAsNonRoot || eff.GID != nil) {
		insp, ok := m.rt.(crt.ImageInspector)
		if !ok {
			return "", fmt.Errorf("container %q: the %s container runtime cannot inspect image %q to read its user, "+
				"which runAsNonRoot / runAsGroup without runAsUser needs; set runAsUser explicitly or use docker/podman/nerdctl",
				def.Name, m.rt.Name(), def.Image)
		}
		u, err := insp.ImageUser(ctx, def.Image)
		if err != nil {
			return "", fmt.Errorf("container %q: inspect image %q user: %w", def.Name, def.Image, err)
		}
		imageUser = u
	}
	imageUID, _, _ := strings.Cut(imageUser, ":")

	if eff.RunAsNonRoot {
		switch {
		case eff.UID != nil:
			if *eff.UID == 0 {
				return "", fmt.Errorf("container has runAsNonRoot and image will run as root (container: %s)", def.Name)
			}
		case imageUID == "" || imageUID == "0":
			return "", fmt.Errorf("container has runAsNonRoot and image will run as root (container: %s, image: %s)", def.Name, def.Image)
		default:
			if _, err := strconv.ParseInt(imageUID, 10, 64); err != nil {
				return "", fmt.Errorf("container has runAsNonRoot and image has non-numeric user (%s), cannot verify user is non-root (container: %s, image: %s)",
					imageUID, def.Name, def.Image)
			}
		}
	}

	switch {
	case eff.UID != nil && eff.GID != nil:
		return fmt.Sprintf("%d:%d", *eff.UID, *eff.GID), nil
	case eff.UID != nil:
		return strconv.FormatInt(*eff.UID, 10), nil
	case eff.GID != nil:
		if imageUID == "" {
			imageUID = "0"
		}
		return fmt.Sprintf("%s:%d", imageUID, *eff.GID), nil
	}
	return "", nil
}

// lineWriter turns written bytes into whole lines for fn. Safe for the two
// concurrent stdout/stderr copiers of an exec.
type lineWriter struct {
	mu  sync.Mutex
	buf []byte
	fn  func(string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			break
		}
		w.fn(strings.TrimRight(string(w.buf[:i]), "\r"))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

func (w *lineWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.fn(strings.TrimRight(string(w.buf), "\r\n"))
		w.buf = nil
	}
}

// HelperExec runs argv in the root pause container, the agent's privileged
// helper. It reuses the ExecSpec contract (Shell = argv[:n-1], Script =
// argv[n-1], exec'd verbatim) so the runtime interface is unchanged.
func (m *claimPodManager) HelperExec(ctx context.Context, argv []string, stdout, stderr io.Writer) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.helperExecLocked(ctx, argv, stdout, stderr)
}

func (m *claimPodManager) helperExecLocked(ctx context.Context, argv []string, stdout, stderr io.Writer) (int, error) {
	if len(argv) == 0 {
		return -1, fmt.Errorf("claim pod helper exec: empty argv")
	}
	if m.pause.ID == "" {
		return -1, fmt.Errorf("claim pod helper exec: the pause container is not running")
	}
	n := len(argv)
	return m.rt.Exec(ctx, m.pause, crt.ExecSpec{Shell: argv[:n-1], Script: argv[n-1]}, stdout, stderr)
}

// applyFSGroup runs `ucd-sh fsgroup` in pause over the workspace and every
// volume, copying its output to the run log. The shim warns per failed entry
// and still exits 0 (kubelet parity); a non-zero exit is a real failure.
func (m *claimPodManager) applyFSGroup(ctx context.Context, fsGroup int64, policy string) error {
	if policy == "" {
		policy = "Always"
	}
	argv := []string{"/.ucd/ucd-sh", "fsgroup", "--policy=" + policy, strconv.FormatInt(fsGroup, 10), m.mountPath}
	for _, v := range m.volumeMounts {
		argv = append(argv, v.ContainerPath)
	}
	var w io.Writer = io.Discard
	if m.runLog != nil {
		lw := &lineWriter{fn: m.runLog}
		defer lw.flush()
		w = lw
	}
	code, err := m.helperExecLocked(ctx, argv, w, w)
	if err != nil {
		return fmt.Errorf("apply fsGroup %d: %w", fsGroup, err)
	}
	if code != 0 {
		return fmt.Errorf("apply fsGroup %d: ucd-sh fsgroup exited with code %d", fsGroup, code)
	}
	return nil
}

// agentWriteOwner is who owns what the agent writes into the claim on the job's
// behalf (artifact download, cache restore), as "<uid>:<gid>", or ok=false when
// the pod sets no identity field and the agent keeps writing as itself.
//
// On Kubernetes that work is done by the unified-artifact sidecar, which runs
// as the pod-level runAsUser/runAsGroup, or root when those are unset
// (design 6.4). A container-level runAsUser does not reach the sidecar, so a
// pod whose only identity field is on the job container gets 0:0, exactly as it
// gets root-owned downloads on Kubernetes.
func (m *claimPodManager) agentWriteOwner() (string, bool) {
	m.mu.Lock()
	pt := m.podTemplate
	m.mu.Unlock()
	if !setsIdentity(pt) {
		return "", false
	}
	eff, err := dsl.ResolveSecurity(podSecurityContext(pt), nil)
	if err != nil {
		// Start already resolved the pod-level context successfully.
		return "", false
	}
	var uid, gid int64
	if eff.UID != nil {
		uid = *eff.UID
	}
	if eff.GID != nil {
		gid = *eff.GID
	}
	return fmt.Sprintf("%d:%d", uid, gid), true
}

// setsIdentity reports whether pt sets runAsUser, runAsGroup or fsGroup at pod
// level, or runAsUser/runAsGroup on any container: the fields that make a job
// care who owns its files.
func setsIdentity(pt *dsl.PodTemplate) bool {
	has := func(sc map[string]any, keys ...string) bool {
		for _, k := range keys {
			if sc[k] != nil {
				return true
			}
		}
		return false
	}
	if has(podSecurityContext(pt), "runAsUser", "runAsGroup", "fsGroup") {
		return true
	}
	for _, c := range dsl.PodTemplateContainers(pt) {
		sc, _ := c["securityContext"].(map[string]any)
		if has(sc, "runAsUser", "runAsGroup") {
			return true
		}
	}
	return false
}

// chownBatch bounds one chown-as argv: an extraction can write thousands of
// files, and an exec argument list has a size limit.
const chownBatch = 128

// ChownAsAgentWrite re-owns each of containerPaths as the identity
// agentWriteOwner names, by running `ucd-sh chown-as` in pause. chown-as is
// recursive, so a caller passes only files and directories it created, never a
// directory that already held the job's own files. It does nothing when the
// pod sets no identity field. Paths are cleaned first: chown-as takes a
// trailing-slash path as its own parent.
func (m *claimPodManager) ChownAsAgentWrite(ctx context.Context, containerPaths []string) error {
	owner, ok := m.agentWriteOwner()
	if !ok {
		return nil
	}
	for start := 0; start < len(containerPaths); start += chownBatch {
		end := min(start+chownBatch, len(containerPaths))
		argv := []string{"/.ucd/ucd-sh", "chown-as", owner}
		for _, p := range containerPaths[start:end] {
			argv = append(argv, path.Clean(p))
		}
		var out strings.Builder
		code, err := m.HelperExec(ctx, argv, &out, &out)
		if err != nil {
			return fmt.Errorf("ucd-sh chown-as %s: %w", owner, err)
		}
		if code != 0 {
			return fmt.Errorf("ucd-sh chown-as %s exited with code %d: %s", owner, code, strings.TrimSpace(out.String()))
		}
	}
	return nil
}

// CopyOutFromPause copies containerPath out of the root pause container to
// hostPath. Pause is root and has the same mounts as the job, so it reads what
// the agent's own user cannot.
func (m *claimPodManager) CopyOutFromPause(ctx context.Context, containerPath, hostPath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pause.ID == "" {
		return fmt.Errorf("claim pod copy out: the pause container is not running")
	}
	return m.rt.CopyOut(ctx, m.pause, containerPath, hostPath)
}
