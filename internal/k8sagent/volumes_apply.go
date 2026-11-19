package k8sagent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

// annotationReclaimPolicy records on a PVC the reclaim policy its AgentVolume
// had when this agent last applied it. Reclaim reads it back from the cluster,
// so a volume that left the desired set while the agent was down is still
// reclaimed after a restart (no state file).
const annotationReclaimPolicy = "unified-cd/reclaim-policy"

// annotationLeftDesiredAt records on a reclaimPolicy Delete PVC when an agent
// first saw its volume absent from the delivered set (RFC 3339). The PVC is
// reclaimed only once the volume has stayed absent for reclaimGrace; any agent
// (replica) that is still delivered the volume removes the annotation, which
// restarts the clock. Being in the cluster, it survives agent restarts.
const annotationLeftDesiredAt = "unified-cd/left-desired-at"

// reclaimGrace is how long a Delete volume must stay continuously absent from
// the delivered set before its PVC is deleted. A volume that comes back within
// it (an AgentVolume moved between AppSources, deleted and re-applied, a
// controller database restored, a label edit reverted, or replicas whose
// delivered sets briefly disagree) keeps its PVC.
var reclaimGrace = 10 * time.Minute

// now is the clock the reclaim grace is measured with.
func (a *K8sAgent) now() time.Time {
	if a.clock != nil {
		return a.clock()
	}
	return time.Now()
}

// declaredVolume is one volume this agent currently offers.
type declaredVolume struct {
	Name string
	Mode string
}

// volumeEntry is what the agent remembers about one delivered volume.
type volumeEntry struct {
	// mode is the mode the volume is offered under (valid while offered).
	mode string
	// offered says the volume is advertised and may be mounted.
	offered bool
	// mismatchWarned stops the existing-PVC mismatch warning repeating every beat.
	mismatchWarned bool
}

// volumeState is the volume set the controller delivered, as applied here. It
// is guarded by its own mutex and read by the claim loop and the heartbeat.
type volumeState struct {
	mu       sync.Mutex
	known    bool
	revision string
	vols     map[string]*volumeEntry
	status   []api.AgentVolumeStatus
	// reserved maps PVC claim name -> volume name for every claim the agent
	// knows to be a named volume's (delivered, or labelled as its own), so the
	// job pod policy can refuse a job that mounts one directly.
	reserved map[string]string
	// orphanLogged remembers the PVCs already reported as left behind.
	orphanLogged map[string]bool
	// reclaimLogged remembers, by volume, that "reclaim scheduled" was logged for
	// the volume's current absence from the set; it is cleared when the volume is
	// delivered again or reclaimed.
	reclaimLogged map[string]bool
}

// offered returns the volumes currently offered, by name.
func (s *volumeState) offered() map[string]declaredVolume {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]declaredVolume, len(s.vols))
	for n, e := range s.vols {
		if e.offered {
			out[n] = declaredVolume{Name: n, Mode: e.mode}
		}
	}
	return out
}

// declaredVolumes is the set of volumes this agent offers right now.
func (a *K8sAgent) declaredVolumes() map[string]declaredVolume { return a.volState.offered() }

// volumeNames returns the names of the offered volumes, sorted, as advertised
// on every claim.
func (a *K8sAgent) volumeNames() []string {
	m := a.declaredVolumes()
	var names []string
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (a *K8sAgent) volumesKnown() bool {
	a.volState.mu.Lock()
	defer a.volState.mu.Unlock()
	return a.volState.known
}

// namedVolumeOfClaim returns the volume whose PVC is claim, if any.
func (a *K8sAgent) namedVolumeOfClaim(claim string) (string, bool) {
	a.volState.mu.Lock()
	defer a.volState.mu.Unlock()
	v, ok := a.volState.reserved[claim]
	return v, ok
}

// buildHeartbeat makes the per-beat request: the common providers plus the
// status the last apply reported.
func (a *K8sAgent) buildHeartbeat(activeRunIDs func() []string) func() *api.HeartbeatRequest {
	base := agentlib.BuildHeartbeat(activeRunIDs, a.podBindings.Snapshot, a.storageReading)
	return func() *api.HeartbeatRequest {
		req := base()
		if req == nil {
			return nil
		}
		a.volState.mu.Lock()
		req.VolumeStatus = append([]api.AgentVolumeStatus(nil), a.volState.status...)
		a.volState.mu.Unlock()
		return req
	}
}

// volumeApplyTimeout bounds one apply, which runs on the heartbeat goroutine.
const volumeApplyTimeout = 30 * time.Second

// onHeartbeat applies the AgentVolumes a heartbeat answer delivered. It is
// called only with a successfully decoded answer that carries a volume set
// (agentlib.StartHeartbeat guarantees both): applying an empty set reclaims
// every Delete volume, so it must never run on an error, an empty 204, an
// answer without a set, or before the first answer.
func (a *K8sAgent) onHeartbeat(ctx context.Context, resp *api.HeartbeatResponse) {
	if resp == nil || a.kube == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, volumeApplyTimeout)
	defer cancel()
	statuses := a.applyVolumes(ctx, resp.Volumes)
	a.volState.mu.Lock()
	changed := resp.Revision != a.volState.revision
	a.volState.revision = resp.Revision
	a.volState.status = statuses
	a.volState.known = true
	a.volState.mu.Unlock()
	if changed {
		slog.Info("volume set changed", "revision", resp.Revision, "volumes", len(resp.Volumes))
	}
}

// validateDesired checks one delivered volume; the error text is the reason
// reported after "invalid: ".
func (a *K8sAgent) validateDesired(d api.DesiredVolume) (resource.Quantity, error) {
	var zero resource.Quantity
	if a.cfg.AgentName == "" {
		return zero, fmt.Errorf("agentName is not set in the agent config; it prefixes the PVC names")
	}
	if errs := validation.IsDNS1123Label(d.Name); len(errs) > 0 {
		return zero, fmt.Errorf("name %q is not usable in a PVC name: %s", d.Name, strings.Join(errs, "; "))
	}
	if errs := validation.IsDNS1123Label(a.cfg.AgentName); len(errs) > 0 {
		return zero, fmt.Errorf("agentName %q is not usable in a PVC name: %s", a.cfg.AgentName, strings.Join(errs, "; "))
	}
	switch d.Mode {
	case agentlib.VolumeModeExclusive, agentlib.VolumeModeShared:
	default:
		return zero, fmt.Errorf("mode %q is not %q or %q", d.Mode, agentlib.VolumeModeExclusive, agentlib.VolumeModeShared)
	}
	q, err := resource.ParseQuantity(d.Size)
	if err != nil || q.Sign() <= 0 {
		return zero, fmt.Errorf("size %q is not a positive quantity such as 200Gi", d.Size)
	}
	return q, nil
}

func accessModeFor(mode string) corev1.PersistentVolumeAccessMode {
	if mode == agentlib.VolumeModeShared {
		return corev1.ReadWriteMany
	}
	return corev1.ReadWriteOnce
}

func reclaimPolicyOrDefault(p string) string {
	if p == dsl.ReclaimDelete {
		return dsl.ReclaimDelete
	}
	return dsl.ReclaimRetain
}

// applyVolumes brings the agent in line with the delivered set and returns the
// status of each volume. It must be called only with the Volumes of a
// successfully decoded heartbeat answer (see onHeartbeat).
//
//   - A volume is validated, its PVC created if missing (an existing PVC is
//     used as it is, with a warning on a mismatch) and annotated with its
//     reclaim policy. One that fails is reported not ready and does not block
//     the rest; it is retried on the next call. An existing PVC of this agent
//     is annotated with the delivered reclaim policy, and loses any
//     left-desired-at annotation, even when the volume fails to apply.
//   - A volume that left the set stops being offered at once. Its PVC is never
//     deleted here unless it carries reclaimPolicy Delete, has been absent for
//     reclaimGrace, and no pod or lock holds it (see reclaim).
//   - A mode change is applied only while no pod holds the volume; until then
//     the volume is not offered. The existing PVC's access mode is not changed.
//
// Decisions that depend on which pods hold a volume (a mode change, the reclaim
// of an absent volume and the delete itself) are taken only while volMu is held
// (the lock createClaimPod chooses volumes under) and only when the pods could be
// listed; otherwise they wait for the next call, and the current offers stay as
// they are. volMu is NOT held across the calls that do not depend on occupancy:
// listing, creating and annotating PVCs, and stamping the left-desired-at mark.
// Those can be slow, and a run's pod create must not wait behind them.
func (a *K8sAgent) applyVolumes(ctx context.Context, desired []api.DesiredVolume) []api.AgentVolumeStatus {
	s := &a.volState
	s.mu.Lock()
	if s.vols == nil {
		s.vols = map[string]*volumeEntry{}
		s.reserved = map[string]string{}
		s.orphanLogged = map[string]bool{}
	}
	if s.reclaimLogged == nil {
		s.reclaimLogged = map[string]bool{}
	}
	s.mu.Unlock()

	var owned map[string]*corev1.PersistentVolumeClaim
	if a.kube != nil {
		owned = a.ownedPVCs(ctx)
	}

	// First pass, without volMu: validate, create or annotate each PVC.
	var dups []api.AgentVolumeStatus
	staged := make([]stagedVolume, 0, len(desired))
	mentioned := make(map[string]bool, len(desired))
	for _, d := range desired {
		if mentioned[d.Name] {
			dups = append(dups, api.AgentVolumeStatus{Name: d.Name, Reason: "invalid: volume delivered more than once"})
			continue
		}
		mentioned[d.Name] = true
		staged = append(staged, a.applyOne(ctx, d, owned))
	}
	// A volume that is back in the set starts a new absence period when it next
	// leaves, which logs again.
	s.mu.Lock()
	for name := range mentioned {
		delete(s.reclaimLogged, name)
	}
	s.mu.Unlock()

	// Stamp the absence clock of every Delete volume that left the set, also
	// without volMu: it does not depend on occupancy.
	var graceOver map[string]bool
	if owned != nil {
		graceOver = a.markAbsent(ctx, mentioned, owned)
	}

	// Second pass, under volMu where it can be had.
	statuses := a.decide(ctx, staged, mentioned, owned, graceOver)
	statuses = append(statuses, dups...)

	// A volume that left the set stops being offered at once.
	s.mu.Lock()
	for name := range s.vols {
		if !mentioned[name] {
			delete(s.vols, name)
		}
	}
	s.mu.Unlock()

	// Remember which claims are named volumes', for the pod policy.
	reserved := map[string]string{}
	for name := range mentioned {
		reserved[claimNameFor(a.cfg.AgentName, name)] = name
	}
	for _, pvc := range owned {
		if v := pvc.Labels[labelVolumeName]; v != "" && pvc.Name == claimNameFor(a.cfg.AgentName, v) {
			reserved[pvc.Name] = v
		}
	}
	s.mu.Lock()
	if owned != nil || len(reserved) > 0 {
		s.reserved = reserved
	}
	s.mu.Unlock()

	sort.SliceStable(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })
	return statuses
}

// decide is the part of an apply that depends on which pods hold a volume: it
// settles the mode of each staged volume and reclaims absent ones. It holds volMu
// for as long as it runs (when it can take it) and reads occupancy under it.
func (a *K8sAgent) decide(ctx context.Context, staged []stagedVolume, mentioned map[string]bool, owned map[string]*corev1.PersistentVolumeClaim, graceOver map[string]bool) []api.AgentVolumeStatus {
	// held is nil when occupancy is unknown. TryLock, not Lock: this runs on the
	// heartbeat goroutine, which must not stall behind a pod create.
	var held map[string]bool
	if a.volMu.TryLock() {
		defer a.volMu.Unlock()
		held = a.occupancy(ctx)
	}
	holdsVolume := func(volume string) bool { return held[volume] || held[claimNameFor(a.cfg.AgentName, volume)] }

	statuses := make([]api.AgentVolumeStatus, 0, len(staged))
	for _, sv := range staged {
		statuses = append(statuses, a.settleMode(sv, held != nil, holdsVolume))
	}
	if owned != nil {
		statuses = append(statuses, a.reclaim(ctx, mentioned, owned, graceOver, held != nil, holdsVolume)...)
	}
	return statuses
}

// occupancy returns the set of volume names and claim names a live pod holds,
// or nil if the pods could not be listed. A pod holds a volume by its label
// (volumes_lock.go's occupancy record) or, defensively, by a pod volume that
// references the claim.
func (a *K8sAgent) occupancy(ctx context.Context) map[string]bool {
	pods, err := a.pm.ListPods(ctx, agentPodSelector)
	if err != nil {
		slog.Warn("k8s: could not read volume occupancy; volume removals, mode changes and reclaim wait", "error", err)
		return nil
	}
	held := heldVolumes(pods.Items, a.cfg.AgentName)
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil {
				held[v.PersistentVolumeClaim.ClaimName] = true
			}
		}
	}
	return held
}

// ownedPVCs lists the PVCs labelled as this agent's, by name; nil on error.
func (a *K8sAgent) ownedPVCs(ctx context.Context) map[string]*corev1.PersistentVolumeClaim {
	l, err := a.kube.CoreV1().PersistentVolumeClaims(a.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: labelVolumeOwner + "=" + a.cfg.AgentName})
	if err != nil {
		slog.Warn("k8s: could not list this agent's PVCs; creating volumes and reclaim wait", "error", err)
		return nil
	}
	out := make(map[string]*corev1.PersistentVolumeClaim, len(l.Items))
	for i := range l.Items {
		out[l.Items[i].Name] = &l.Items[i]
	}
	return out
}

// stagedVolume is a delivered volume after its PVC has been ensured, waiting
// for the decisions that need occupancy (settleMode).
type stagedVolume struct {
	d  api.DesiredVolume
	st api.AgentVolumeStatus
	// pvc is the volume's claim; nil when the volume already failed (st is then
	// final).
	pvc *corev1.PersistentVolumeClaim
}

// applyOne validates one delivered volume and ensures its PVC. It takes no
// occupancy decision and must run without volMu; settleMode finishes the job.
func (a *K8sAgent) applyOne(ctx context.Context, d api.DesiredVolume, owned map[string]*corev1.PersistentVolumeClaim) stagedVolume {
	s := &a.volState
	st := api.AgentVolumeStatus{Name: d.Name}
	entry := func() *volumeEntry {
		s.mu.Lock()
		defer s.mu.Unlock()
		e := s.vols[d.Name]
		if e == nil {
			e = &volumeEntry{}
			s.vols[d.Name] = e
		}
		return e
	}
	setOffered := func(offered bool) {
		s.mu.Lock()
		s.vols[d.Name].offered = offered
		s.mu.Unlock()
	}

	size, err := a.validateDesired(d)
	if err != nil {
		entry()
		setOffered(false)
		st.Reason = fmt.Sprintf("invalid: %v", err)
		// The volume is still declared: keep its PVC's policy current and its
		// absence clock cleared, so a Delete-to-Retain change is never lost.
		if pvc := owned[claimNameFor(a.cfg.AgentName, d.Name)]; pvc != nil && a.kube != nil {
			if _, perr := a.syncPVCAnnotations(ctx, pvc, d); perr != nil {
				slog.Warn("k8s: could not update the annotations of a volume that failed to apply", "volume", d.Name, "pvc", pvc.Name, "error", perr)
			}
		}
		return stagedVolume{d: d, st: st}
	}
	e := entry()
	if a.kube == nil {
		setOffered(false)
		st.Reason = "create PVC: the agent has no Kubernetes client"
		return stagedVolume{d: d, st: st}
	}
	pvc, err := a.ensurePVC(ctx, d, size, owned, e)
	if err != nil {
		setOffered(false)
		st.Reason = err.Error()
		return stagedVolume{d: d, st: st}
	}
	return stagedVolume{d: d, st: st, pvc: pvc}
}

// settleMode decides whether a volume whose PVC exists is offered, and under
// which mode. A mode change depends on whether a pod holds the volume, so this
// runs under volMu (when it could be taken; heldKnown says it was).
func (a *K8sAgent) settleMode(sv stagedVolume, heldKnown bool, holdsVolume func(string) bool) api.AgentVolumeStatus {
	if sv.pvc == nil {
		return sv.st
	}
	s := &a.volState
	d, st := sv.d, sv.st
	setOffered := func(offered bool, mode string) {
		s.mu.Lock()
		e := s.vols[d.Name]
		e.offered = offered
		if mode != "" {
			e.mode = mode
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	e := s.vols[d.Name]
	offered, mode := e.offered, e.mode
	s.mu.Unlock()
	switch {
	case !offered && mode == "":
		// New (or recovered after a failure): offered under the delivered mode.
		setOffered(true, d.Mode)
	case mode == d.Mode:
		setOffered(true, "")
	case !heldKnown:
		// Cannot tell whether a pod holds it: leave things as they are.
		st.Reason = fmt.Sprintf("waiting to switch to %s until occupancy can be read", d.Mode)
		return st
	case holdsVolume(d.Name):
		setOffered(false, "")
		st.Reason = fmt.Sprintf("waiting for runs holding it to finish before switching to %s", d.Mode)
		return st
	default:
		setOffered(true, d.Mode)
		slog.Warn("k8s: volume mode changed; the existing PVC's access mode is not changed",
			"volume", d.Name, "pvc", sv.pvc.Name, "mode", d.Mode, "accessModes", sv.pvc.Spec.AccessModes)
	}
	st.Ready = true
	return st
}

// ensurePVC returns the PVC behind d, creating it if there is none. An
// existing PVC is used as it is; one this agent created (it carries the owner
// label) is also annotated with d's reclaim policy. A PVC without the owner
// label was not created by this agent and is never annotated, so it can never
// be reclaimed.
func (a *K8sAgent) ensurePVC(ctx context.Context, d api.DesiredVolume, size resource.Quantity, owned map[string]*corev1.PersistentVolumeClaim, e *volumeEntry) (*corev1.PersistentVolumeClaim, error) {
	pvcs := a.kube.CoreV1().PersistentVolumeClaims(a.cfg.Namespace)
	name := claimNameFor(a.cfg.AgentName, d.Name)
	policy := reclaimPolicyOrDefault(d.ReclaimPolicy)

	existing := owned[name]
	if existing == nil {
		got, err := pvcs.Get(ctx, name, metav1.GetOptions{})
		switch {
		case err == nil:
			existing = got
		case apierrors.IsNotFound(err):
		default:
			return nil, fmt.Errorf("create PVC: look up %s: %w", name, err)
		}
	}
	if existing != nil && existing.DeletionTimestamp != nil {
		// Terminating: the PVC is gone as soon as its last pod releases it, so
		// it is not offered. A later call creates a fresh one.
		return nil, fmt.Errorf("create PVC: %s is being deleted", name)
	}
	if existing == nil {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Namespace:   a.cfg.Namespace,
				Labels:      map[string]string{labelVolumeOwner: a.cfg.AgentName, labelVolumeName: d.Name},
				Annotations: map[string]string{annotationReclaimPolicy: policy},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{accessModeFor(d.Mode)},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: size},
				},
			},
		}
		// storageClassName stays nil when unset: an empty string would mean "no
		// StorageClass" and leave the claim Pending instead of using the default.
		if d.StorageClassName != "" {
			sc := d.StorageClassName
			pvc.Spec.StorageClassName = &sc
		}
		made, err := pvcs.Create(ctx, pvc, metav1.CreateOptions{})
		if err != nil {
			// Another replica sharing this agentName got there first.
			if apierrors.IsAlreadyExists(err) {
				return pvc, nil
			}
			return nil, fmt.Errorf("create PVC: %w", err)
		}
		slog.Info("k8s: created volume PVC", "volume", d.Name, "pvc", name, "size", d.Size, "mode", d.Mode, "reclaimPolicy", policy)
		return made, nil
	}

	if existing.Labels[labelVolumeOwner] != a.cfg.AgentName {
		if !e.mismatchWarned {
			e.mismatchWarned = true
			slog.Warn("k8s: a PVC with this volume's claim name already exists but was not created by this agent; it is used as it is and will never be reclaimed",
				"volume", d.Name, "pvc", name)
			warnOnClaimMismatch(existing, d)
		}
		return existing, nil
	}
	if !e.mismatchWarned {
		e.mismatchWarned = true
		warnOnClaimMismatch(existing, d)
	}
	got, err := a.syncPVCAnnotations(ctx, existing, d)
	if err != nil {
		return nil, fmt.Errorf("create PVC: annotate %s with reclaim policy: %w", name, err)
	}
	return got, nil
}

// syncPVCAnnotations brings the annotations of pvc, a PVC this agent created
// for the still-delivered volume d, in line: the reclaim policy is d's, and the
// left-desired-at mark is removed (the volume is desired, so its absence clock
// restarts). A PVC without this agent's owner label is never touched.
func (a *K8sAgent) syncPVCAnnotations(ctx context.Context, pvc *corev1.PersistentVolumeClaim, d api.DesiredVolume) (*corev1.PersistentVolumeClaim, error) {
	if pvc.Labels[labelVolumeOwner] != a.cfg.AgentName {
		return pvc, nil
	}
	policy := reclaimPolicyOrDefault(d.ReclaimPolicy)
	ann := map[string]any{}
	if pvc.Annotations[annotationReclaimPolicy] != policy {
		ann[annotationReclaimPolicy] = policy
	}
	if _, ok := pvc.Annotations[annotationLeftDesiredAt]; ok {
		ann[annotationLeftDesiredAt] = nil // a merge patch removes a null key
	}
	if len(ann) == 0 {
		return pvc, nil
	}
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": ann}})
	return a.kube.CoreV1().PersistentVolumeClaims(a.cfg.Namespace).Patch(ctx, pvc.Name, types.MergePatchType, patch, metav1.PatchOptions{})
}

// warnOnClaimMismatch reports a claim whose spec differs from the delivered
// volume, without touching it.
func warnOnClaimMismatch(existing *corev1.PersistentVolumeClaim, d api.DesiredVolume) {
	if want := accessModeFor(d.Mode); len(existing.Spec.AccessModes) != 1 || existing.Spec.AccessModes[0] != want {
		slog.Warn("k8s: existing volume PVC has different access modes than the volume's mode asks for; it is left as it is",
			"volume", d.Name, "pvc", existing.Name, "mode", d.Mode, "accessModes", existing.Spec.AccessModes)
	}
	if got, ok := existing.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
		if want, err := resource.ParseQuantity(d.Size); err == nil && got.Cmp(want) != 0 {
			slog.Warn("k8s: existing volume PVC has a different size than the volume; it is not resized",
				"volume", d.Name, "pvc", existing.Name, "pvcSize", got.String(), "size", d.Size)
		}
	}
	if d.StorageClassName != "" && (existing.Spec.StorageClassName == nil || *existing.Spec.StorageClassName != d.StorageClassName) {
		slog.Warn("k8s: existing volume PVC has a different storageClassName than the volume; it is left as it is",
			"volume", d.Name, "pvc", existing.Name)
	}
}

// reclaimCandidate says whether pvc is one reclaim may consider, and for which
// volume: it is named exactly claimNameFor(agent, volume) for the volume its
// label names (so a claim left by the old "<agentName>-<volume>" naming is never
// one), it carries this agent's owner label, and the volume is not delivered.
func (a *K8sAgent) reclaimCandidate(pvc *corev1.PersistentVolumeClaim, mentioned map[string]bool) (string, bool) {
	v := pvc.Labels[labelVolumeName]
	if v == "" || pvc.Name != claimNameFor(a.cfg.AgentName, v) || pvc.Labels[labelVolumeOwner] != a.cfg.AgentName || mentioned[v] {
		return "", false
	}
	return v, true
}

// markAbsent stamps the left-desired-at annotation of every Delete volume's PVC
// that left the delivered set and reports, by PVC name, which have been absent
// for reclaimGrace. It does not depend on occupancy and is called without volMu.
func (a *K8sAgent) markAbsent(ctx context.Context, mentioned map[string]bool, owned map[string]*corev1.PersistentVolumeClaim) map[string]bool {
	over := map[string]bool{}
	for n, pvc := range owned {
		v, ok := a.reclaimCandidate(pvc, mentioned)
		if ok && pvc.Annotations[annotationReclaimPolicy] == dsl.ReclaimDelete && a.reclaimGraceOver(ctx, pvc, v) {
			over[n] = true
		}
	}
	return over
}

// reclaim handles the PVCs of volumes that are not in the delivered set. A PVC
// is deleted only when ALL of these hold: it is named <agentName>.<volume> for
// the volume its label names, it carries the owner label for this agent, its
// annotation says reclaimPolicy Delete, the volume is not in the delivered set,
// its left-desired-at annotation is at least reclaimGrace old (graceOver, from
// markAbsent), no live pod holds it (the pod list read under volMu at the start
// of this decision), and its lock annotations name no holder stamped within the
// lock grace. The delete is conditional on the PVC's resourceVersion from the
// read at the start of the apply, so a lock taken, or a left-desired-at cleared
// by another replica, since then cancels it. Everything else is kept; a Retain
// one is reported once as left behind. A failed delete is reported as "reclaim:
// <error>" and retried. It runs under volMu: eligibility and the delete are
// decisions about occupancy.
func (a *K8sAgent) reclaim(ctx context.Context, mentioned map[string]bool, owned map[string]*corev1.PersistentVolumeClaim, graceOver map[string]bool, heldKnown bool, holdsVolume func(string) bool) []api.AgentVolumeStatus {
	var failed []api.AgentVolumeStatus
	names := make([]string, 0, len(owned))
	for n := range owned {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		pvc := owned[n]
		v, ok := a.reclaimCandidate(pvc, mentioned)
		if !ok {
			continue
		}
		if pvc.Annotations[annotationReclaimPolicy] == dsl.ReclaimDelete && !graceOver[pvc.Name] {
			continue
		}
		if !heldKnown || holdsVolume(v) {
			continue // not yet: nothing is decided about a volume a pod may hold
		}
		if pvc.Annotations[annotationReclaimPolicy] != dsl.ReclaimDelete {
			a.volState.mu.Lock()
			first := !a.volState.orphanLogged[pvc.Name]
			a.volState.orphanLogged[pvc.Name] = true
			a.volState.mu.Unlock()
			if first {
				slog.Warn("k8s: volume is no longer in the delivered set and no run holds it; its PVC is left in place (reclaimPolicy Retain). Delete the PVC yourself if the data is not wanted.",
					"volume", v, "pvc", pvc.Name)
			}
			continue
		}
		if holder := pvc.Annotations[annoVolumeHeldBy]; holder != "" {
			at, perr := time.Parse(time.RFC3339, pvc.Annotations[annoVolumeHeldAt])
			if perr != nil || a.now().Sub(at) < a.lockGrace() {
				continue // a run may be between taking the lock and creating its pod
			}
		}
		rv := pvc.ResourceVersion
		err := a.kube.CoreV1().PersistentVolumeClaims(a.cfg.Namespace).Delete(ctx, pvc.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{ResourceVersion: &rv}})
		if apierrors.IsConflict(err) {
			continue // the PVC changed since it was read: decide again next time
		}
		if err != nil && !apierrors.IsNotFound(err) {
			slog.Warn("k8s: volume reclaim failed; it will be retried on the next heartbeat", "volume", v, "pvc", pvc.Name, "error", err)
			failed = append(failed, api.AgentVolumeStatus{Name: v, Reason: fmt.Sprintf("reclaim: %v", err)})
			continue
		}
		slog.Info("k8s: reclaimed volume PVC", "volume", v, "pvc", pvc.Name)
		a.volState.mu.Lock()
		delete(a.volState.orphanLogged, pvc.Name)
		delete(a.volState.reclaimLogged, v)
		a.volState.mu.Unlock()
	}
	return failed
}

// reclaimGraceOver reports whether the Delete volume v, absent from the
// delivered set, has been absent for reclaimGrace. The first time it is seen
// absent (no or an unreadable left-desired-at annotation) the annotation is
// stamped with the current time and the reclaim is scheduled.
func (a *K8sAgent) reclaimGraceOver(ctx context.Context, pvc *corev1.PersistentVolumeClaim, v string) bool {
	now := a.now()
	if at, err := time.Parse(time.RFC3339, pvc.Annotations[annotationLeftDesiredAt]); err == nil {
		return now.Sub(at) >= reclaimGrace
	}
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{annotationLeftDesiredAt: now.UTC().Format(time.RFC3339)}}})
	if _, err := a.kube.CoreV1().PersistentVolumeClaims(a.cfg.Namespace).Patch(ctx, pvc.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		slog.Warn("k8s: could not mark a volume as left the set; its reclaim waits", "volume", v, "pvc", pvc.Name, "error", err)
		return false
	}
	// Once per absence period: it is logged again only after the volume has been
	// delivered again and has left anew.
	a.volState.mu.Lock()
	first := !a.volState.reclaimLogged[v]
	a.volState.reclaimLogged[v] = true
	a.volState.mu.Unlock()
	if first {
		slog.Info(fmt.Sprintf("volume left the set; reclaim scheduled after %s", reclaimGrace), "volume", v, "pvc", pvc.Name)
	}
	return false
}
