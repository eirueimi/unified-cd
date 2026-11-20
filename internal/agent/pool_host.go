package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
)

// minPoolEvictionInterval is the floor of the idle-eviction tick.
const minPoolEvictionInterval = 30 * time.Second

// hostReuseWithVolumesMsg is the run failure for a claim that combines
// podTemplate.reuse with spec.volumes. It is the Kubernetes agent's message
// (internal/k8sagent/agent.go) without its "k8s: " prefix.
const hostReuseWithVolumesMsg = "spec.volumes cannot be combined with podTemplate.reuse: a reused pod keeps the volumes it was created with, which contradicts a volume's lifecycle"

// reuseWithVolumes reports whether the claim asks for pod reuse together with
// volumes, a combination that fails the run.
func reuseWithVolumes(c api.ClaimResponse) bool {
	return !c.Native && c.PodTemplate != nil && c.PodTemplate.Reuse && len(c.Volumes) > 0
}

// hostPodFingerprintInput is everything a reused pod's containers were created
// from. Two claims may share a pod only when this is identical.
//
// Defs carry the per-run expanded images (the controller expands the
// podTemplate's images with the run's params and vars), so the fingerprint
// holds the expanded reference and its resolved image ID, never the template
// string.
type hostPodFingerprintInput struct {
	JobName, WorkDir, MountPath string
	Defs                        []containerDef // claimContainerDefs output, incl. Security
	PodSecurity                 dsl.EffectiveSecurity
	ImageIDs                    map[string]string // from pullClaimImages
	PauseImage, RunnerImage     string
	ToolsDir                    string
	MaxMappedID                 int64 // security-context probe result
}

// hostPodFingerprint is the sha256 hex of the input's JSON encoding (map keys
// are sorted by encoding/json, so it is deterministic).
func hostPodFingerprint(in hostPodFingerprintInput) (string, error) {
	b, err := json.Marshal(in)
	if err != nil {
		return "", fmt.Errorf("claim pod fingerprint: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// hostPoolKey identifies the pool entry a claim's pod is released into.
type hostPoolKey struct {
	workDir, jobName, fingerprint string
}

type pooledHostPod struct {
	pod         *claimPodManager
	jobName     string
	fingerprint string
	idleSince   time.Time
}

// hostPodPool keeps idle claim pods of jobs with podTemplate.reuse: true. It
// holds at most one pod per workDir (a job on a slot), so a hit needs the same
// job, the same slot and the same fingerprint.
type hostPodPool struct {
	rt          crt.ContainerRuntime
	idleTimeout time.Duration
	maxIdle     int
	now         func() time.Time

	mu     sync.Mutex
	idle   map[string]*pooledHostPod // keyed by workDir
	closed bool

	evictCancel context.CancelFunc
	evictWG     sync.WaitGroup
}

func newHostPodPool(rt crt.ContainerRuntime, idleTimeout time.Duration, maxIdle int) *hostPodPool {
	return &hostPodPool{rt: rt, idleTimeout: idleTimeout, maxIdle: maxIdle, now: time.Now,
		idle: map[string]*pooledHostPod{}}
}

// podRunning reports whether the pod's pause container and every container it
// holds are running. Any inspection error counts as not running.
func podRunning(ctx context.Context, rt crt.PodRuntime, m *claimPodManager) bool {
	m.mu.Lock()
	handles := make([]crt.ContainerHandle, 0, len(m.open)+1)
	if m.pause.ID == "" {
		m.mu.Unlock()
		return false
	}
	handles = append(handles, m.pause)
	for _, h := range m.open {
		handles = append(handles, h)
	}
	m.mu.Unlock()
	for _, h := range handles {
		ok, err := rt.ContainerRunning(ctx, h)
		if err != nil || !ok {
			return false
		}
	}
	return true
}

// Acquire takes the idle pod for workDir out of the pool. It is a hit only when
// the pod's job and fingerprint match and every container is still running.
// Any other idle pod for workDir is removed and the call is a miss.
func (p *hostPodPool) Acquire(ctx context.Context, workDir, jobName, fp string) (*claimPodManager, bool) {
	p.mu.Lock()
	e := p.idle[workDir]
	delete(p.idle, workDir)
	p.mu.Unlock()
	if e == nil {
		return nil, false
	}
	if e.jobName == jobName && e.fingerprint == fp {
		if pr, ok := p.rt.(crt.PodRuntime); ok && podRunning(ctx, pr, e.pod) {
			return e.pod, true
		}
	}
	e.pod.CloseAll(context.WithoutCancel(ctx))
	return nil, false
}

// Release returns a pod at the end of its run. An unhealthy pod, or one that
// arrives after Close, is removed; otherwise it becomes the idle pod for
// workDir. The oldest idle pod is removed while the pool exceeds maxIdle.
func (p *hostPodPool) Release(ctx context.Context, workDir, jobName, fp string, m *claimPodManager, healthy bool) {
	var evicted []*claimPodManager
	p.mu.Lock()
	if !healthy || p.closed || p.maxIdle <= 0 {
		p.mu.Unlock()
		m.CloseAll(ctx)
		return
	}
	if old := p.idle[workDir]; old != nil {
		evicted = append(evicted, old.pod)
	}
	p.idle[workDir] = &pooledHostPod{pod: m, jobName: jobName, fingerprint: fp, idleSince: p.now()}
	for len(p.idle) > p.maxIdle {
		var oldestKey string
		var oldest *pooledHostPod
		for k, e := range p.idle {
			if oldest == nil || e.idleSince.Before(oldest.idleSince) {
				oldestKey, oldest = k, e
			}
		}
		delete(p.idle, oldestKey)
		evicted = append(evicted, oldest.pod)
	}
	p.mu.Unlock()
	for _, e := range evicted {
		e.CloseAll(ctx)
	}
}

// evictExpired removes every idle pod that has been idle for at least
// idleTimeout as of now.
func (p *hostPodPool) evictExpired(ctx context.Context, now time.Time) {
	if p.idleTimeout <= 0 {
		return
	}
	var expired []*claimPodManager
	p.mu.Lock()
	for k, e := range p.idle {
		if now.Sub(e.idleSince) >= p.idleTimeout {
			delete(p.idle, k)
			expired = append(expired, e.pod)
		}
	}
	p.mu.Unlock()
	for _, m := range expired {
		slog.Info("removing idle pooled claim pod", "workDir", m.workDir)
		m.CloseAll(context.WithoutCancel(ctx))
	}
}

// StartEviction removes idle pods once they pass idleTimeout, checking every
// idleTimeout/2 (at least 30s). It is a no-op when idleTimeout is 0, which
// never evicts by age. Close stops it.
func (p *hostPodPool) StartEviction(ctx context.Context) {
	if p.idleTimeout <= 0 {
		return
	}
	interval := p.idleTimeout / 2
	if interval < minPoolEvictionInterval {
		interval = minPoolEvictionInterval
	}
	ectx, cancel := context.WithCancel(ctx)
	p.mu.Lock()
	p.evictCancel = cancel
	p.mu.Unlock()
	p.evictWG.Add(1)
	go func() {
		defer p.evictWG.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ectx.Done():
				return
			case <-t.C:
				p.evictExpired(ectx, p.now())
			}
		}
	}()
}

// WorkDirs lists the workspace directories whose pods the pool holds, so the
// workspace GC leaves them alone (the pod bind-mounts each one).
func (p *hostPodPool) WorkDirs() []string {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.idle))
	for k := range p.idle {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Len is the number of idle pods.
func (p *hostPodPool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.idle)
}

// Close stops eviction and removes every idle pod. A pod released afterwards
// is removed instead of pooled.
func (p *hostPodPool) Close(ctx context.Context) {
	p.mu.Lock()
	p.closed = true
	cancel := p.evictCancel
	idle := p.idle
	p.idle = map[string]*pooledHostPod{}
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	p.evictWG.Wait()
	for _, e := range idle {
		e.pod.CloseAll(ctx)
	}
}

// gcActiveSet is the workspace GC's active set: the directories of the claims
// in flight plus those held by idle pooled pods. pool may be nil.
func gcActiveSet(running []string, pool *hostPodPool) map[string]struct{} {
	set := activeWorkDirSet(running)
	for _, d := range pool.WorkDirs() {
		set[d] = struct{}{}
	}
	return set
}

// prepareReuse makes a pooled pod ready for another run. It empties the
// workspace from inside the pod, as root in the pause container (the previous
// run's files may belong to any uid), then re-applies the pod's fsGroup, which
// Start does only at creation. A failure means the pod must not be reused.
func (m *claimPodManager) prepareReuse(ctx context.Context, pt *dsl.PodTemplate) error {
	podEff, err := dsl.ResolveSecurity(podSecurityContext(pt), nil)
	if err != nil {
		return fmt.Errorf("claim pod: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.podTemplate = pt
	var lines []string
	lw := &lineWriter{fn: func(l string) { lines = append(lines, l) }}
	argv := append(append([]string{}, ucdDefaultShell...), ClearWorkspaceScript(m.mountPath, nil))
	code, err := m.execHelper(ctx, m.pause, argv, lw, lw)
	lw.flush()
	if err != nil {
		return fmt.Errorf("empty the reused workspace: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("empty the reused workspace: exited with code %d (%s)", code, firstLines(lines, 3))
	}
	if podEff.FSGroup != nil {
		if err := m.applyFSGroup(ctx, *podEff.FSGroup, podEff.FSGroupChangePolicy); err != nil {
			return fmt.Errorf("claim pod: %w", err)
		}
	}
	return nil
}

func firstLines(lines []string, n int) string {
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "; ")
}

// sidecarLogBoundary returns the instant a reused pod's sidecar logs resume
// after, so the previous run's output (and anything the sidecars printed while
// the pod sat idle) never reaches this run's log, which is masked with this
// run's secrets.
//
// The boundary is the newest log line the sidecars already hold, as stamped by
// the container runtime (crt.PodRuntime.LastLogTime), plus one nanosecond. It
// is never the agent's time.Now(): the runtime stamps log lines and compares
// LogsSince's argument with its own clock, which on a VM-backed runtime
// (Docker Desktop, podman machine) or a remote daemon can differ from the
// agent's. An agent clock ahead of the runtime's would drop this run's first
// lines; one behind would replay the previous run's tail. Both ends of the
// comparison come from the runtime, so skew cancels out.
//
// A zero result means no sidecar has logged anything, so the whole log of every
// sidecar belongs to this run.
func (m *claimPodManager) sidecarLogBoundary(ctx context.Context) (time.Time, error) {
	pr, ok := m.rt.(crt.PodRuntime)
	if !ok {
		return time.Time{}, fmt.Errorf("container runtime cannot report log times")
	}
	var latest time.Time
	for _, sc := range m.SidecarHandles() {
		t, err := pr.LastLogTime(ctx, sc.Handle)
		if err != nil {
			return time.Time{}, fmt.Errorf("sidecar %q: %w", sc.Name, err)
		}
		if t.After(latest) {
			latest = t
		}
	}
	if latest.IsZero() {
		return time.Time{}, nil
	}
	return latest.Add(time.Nanosecond), nil
}
