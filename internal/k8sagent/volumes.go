package k8sagent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/config"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

// Agent-owned named volumes on Kubernetes.
//
// A volume is an ordinary PersistentVolumeClaim named <agentName>-<volume>,
// created at startup and never deleted by this agent. A claim pod that mounts
// one is labelled with it, and that label is what says a volume is in use right
// now: occupancy is read back from the live pods, never from process memory.
// Taking an exclusive volume is separately made atomic against other replicas
// by a lock annotation on the PVC (volumes_lock.go). Replicas of one Deployment
// share a config and so share claims, and a restarted agent's pods outlive it,
// so a table kept in a process would be wrong in exactly the situations (a
// rollout, a crash) where exclusion matters.
const (
	// volumeLabelPrefix starts the key of the label a claim pod carries for
	// each volume it mounts: volume.unified-cd/<claim name>: <volume name>. The
	// claim name is the key and the volume name the value so that one pod can
	// hold several volumes, and so that the value can be checked against the
	// key rather than parsed out of it (agent "a" with volume "b-c" and agent
	// "a-b" with volume "c" share a claim name but not a label).
	volumeLabelPrefix = "volume.unified-cd/"

	// labelVolumeOwner and labelVolumeName mark a PVC this agent created, so
	// that a claim left behind by a volume dropped from the config can be
	// recognised and reported. Nothing here ever deletes one.
	labelVolumeOwner = "unified-cd/volume-owner"
	labelVolumeName  = "unified-cd/volume"

	// podVolumeNamePrefix names the pod-level Volume for a mounted claim:
	// ucd-vol-<index in the claim's volume list>. Index-based because a pod
	// volume name must be a short DNS label and the claim name may be 63
	// characters.
	podVolumeNamePrefix = "ucd-vol-"

	// agentPodSelector selects every pod this agent family creates in the
	// namespace, claim pods and scope pods alike.
	agentPodSelector = "app=unified-cd-agent"
)

// claimNameFor is the PVC backing a volume: the agent name as a prefix, so
// several k8s-agents in one namespace keep independent copies of a volume name.
func claimNameFor(agentName, volume string) string { return agentName + "-" + volume }

// validateVolumeConfigs checks the volumes of a k8s-agent config. It is called
// at config validation, so a bad declaration is a refused start, and again by
// EnsurePVCs, so the function cannot be handed one that was never checked.
func validateVolumeConfigs(agentName string, cfgs []config.VolumeConfig) error {
	if len(cfgs) == 0 {
		return nil
	}
	if agentName == "" {
		return errors.New("agentName is required when volumes are declared: it prefixes the PVC names, and it must stay the same across restarts or each restart would provision empty volumes")
	}
	if errs := validation.IsDNS1123Label(agentName); len(errs) > 0 {
		return fmt.Errorf("agentName %q is not usable in a PVC name: %s", agentName, strings.Join(errs, "; "))
	}
	seen := map[string]bool{}
	for _, c := range cfgs {
		if c.Name == "" {
			return errors.New("a volume in the agent config has no name")
		}
		if errs := validation.IsDNS1123Label(c.Name); len(errs) > 0 {
			return fmt.Errorf("volume %q: the name becomes part of a PVC name, so it must be lower-case letters, digits and '-': %s", c.Name, strings.Join(errs, "; "))
		}
		// The claim name is also a label key's name part, which is capped at 63.
		if claim := claimNameFor(agentName, c.Name); len(claim) > validation.LabelValueMaxLength {
			return fmt.Errorf("volume %q: the PVC name %q is %d characters, over the %d a label allows; shorten the volume name or agentName", c.Name, claim, len(claim), validation.LabelValueMaxLength)
		}
		if seen[c.Name] {
			return fmt.Errorf("duplicate volume name %q in the agent config", c.Name)
		}
		seen[c.Name] = true
		switch c.Mode {
		case agentlib.VolumeModeExclusive, agentlib.VolumeModeShared:
		default:
			return fmt.Errorf("volume %q: mode must be %q or %q, got %q", c.Name, agentlib.VolumeModeExclusive, agentlib.VolumeModeShared, c.Mode)
		}
		if c.Size == "" {
			return fmt.Errorf("volume %q: size is required on the Kubernetes agent, which provisions a PVC of that size (for example 200Gi)", c.Name)
		}
		if q, err := resource.ParseQuantity(c.Size); err != nil || q.Sign() <= 0 {
			return fmt.Errorf("volume %q: size %q is not a positive quantity such as 200Gi", c.Name, c.Size)
		}
	}
	return nil
}

// volumeNames returns the names of the configured volumes, sorted, as
// advertised at registration and on every claim.
func volumeNames(cfgs []config.VolumeConfig) []string {
	names := make([]string, 0, len(cfgs))
	for _, c := range cfgs {
		names = append(names, c.Name)
	}
	sort.Strings(names)
	return names
}

// EnsurePVCs creates the PVC behind each configured volume that does not have
// one yet. It runs at startup, beside PodPool.Restore.
//
// An existing claim is left exactly as it is: it holds data, and resizing or
// re-moding a bound claim is an operator's decision, so a mismatch is logged
// and nothing is changed.
//
// It never deletes. A volume dropped from the config stops being advertised
// (the caller registers only what is configured) and its claim stays, logged as
// an orphan: removing a line from a config file must not destroy tens of
// gigabytes. The operator deletes the claim deliberately if the data is not
// wanted.
//
// storageClassName is left nil when unset. An empty string would mean "no
// StorageClass" to Kubernetes, which does not apply the cluster default and
// leaves the claim Pending.
func EnsurePVCs(ctx context.Context, client kubernetes.Interface, namespace, agentName string, cfgs []config.VolumeConfig) error {
	if err := validateVolumeConfigs(agentName, cfgs); err != nil {
		return err
	}
	pvcs := client.CoreV1().PersistentVolumeClaims(namespace)
	wanted := map[string]bool{}
	for _, c := range cfgs {
		wanted[c.Name] = true
		if c.Path != "" {
			slog.Warn("k8s: volume path is ignored on the Kubernetes agent, which provisions a PVC", "volume", c.Name, "path", c.Path)
		}
		name := claimNameFor(agentName, c.Name)
		mode := corev1.ReadWriteOnce
		if c.Mode == agentlib.VolumeModeShared {
			mode = corev1.ReadWriteMany
		}
		existing, err := pvcs.Get(ctx, name, metav1.GetOptions{})
		switch {
		case err == nil:
			warnOnClaimMismatch(existing, c, mode)
			continue
		case !apierrors.IsNotFound(err):
			return fmt.Errorf("volume %q: look up PVC %s: %w", c.Name, name, err)
		}
		size := resource.MustParse(c.Size) // validated above
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels:    map[string]string{labelVolumeOwner: agentName, labelVolumeName: c.Name},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{mode},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: size},
				},
			},
		}
		if c.StorageClassName != "" {
			sc := c.StorageClassName
			pvc.Spec.StorageClassName = &sc
		}
		if _, err := pvcs.Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
			// Another replica sharing this config got there first.
			if apierrors.IsAlreadyExists(err) {
				continue
			}
			return fmt.Errorf("volume %q: create PVC %s: %w", c.Name, name, err)
		}
		slog.Info("k8s: created volume PVC", "volume", c.Name, "pvc", name, "size", c.Size, "mode", c.Mode)
	}

	owned, err := pvcs.List(ctx, metav1.ListOptions{LabelSelector: labelVolumeOwner + "=" + agentName})
	if err != nil {
		// Reporting orphans is a courtesy; it must not stop the agent starting.
		slog.Warn("k8s: could not list this agent's PVCs to look for orphans", "error", err)
		return nil
	}
	for i := range owned.Items {
		if v := owned.Items[i].Labels[labelVolumeName]; v != "" && !wanted[v] {
			slog.Warn("k8s: volume is no longer in the agent config; its PVC is left in place and is not advertised. Delete the PVC yourself if the data is not wanted.",
				"volume", v, "pvc", owned.Items[i].Name)
		}
	}
	return nil
}

// warnOnClaimMismatch reports a claim whose spec differs from the config,
// without touching it.
func warnOnClaimMismatch(existing *corev1.PersistentVolumeClaim, c config.VolumeConfig, want corev1.PersistentVolumeAccessMode) {
	if len(existing.Spec.AccessModes) != 1 || existing.Spec.AccessModes[0] != want {
		slog.Warn("k8s: existing volume PVC has different access modes than the config's mode asks for; it is left as it is",
			"volume", c.Name, "pvc", existing.Name, "mode", c.Mode, "accessModes", existing.Spec.AccessModes)
	}
	if got, ok := existing.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
		if wantSize, err := resource.ParseQuantity(c.Size); err == nil && got.Cmp(wantSize) != 0 {
			slog.Warn("k8s: existing volume PVC has a different size than the config; it is not resized",
				"volume", c.Name, "pvc", existing.Name, "pvcSize", got.String(), "configSize", c.Size)
		}
	}
}

// BusyVolumes returns the volumes of agentName that a live claim pod mounts,
// sorted. It is derived from the pods themselves, so two replicas of one
// Deployment, or an agent that has just restarted, give the same answer.
//
// It lists every volume held, shared ones included; only the caller knows each
// volume's mode, and only an exclusive volume is busy in the sense the
// controller's claim filter wants.
func BusyVolumes(ctx context.Context, client kubernetes.Interface, agentName, namespace string) ([]string, error) {
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: agentPodSelector})
	if err != nil {
		return nil, fmt.Errorf("list pods for volume occupancy: %w", err)
	}
	return sortedKeys(heldVolumes(pods.Items, agentName)), nil
}

// heldVolumes is the set of agentName's volumes that a live pod in pods mounts.
// A pod that has finished (Succeeded or Failed) holds nothing. One that is
// being deleted still does, until it is gone: its volume is not yet released.
func heldVolumes(pods []corev1.Pod, agentName string) map[string]bool {
	held := map[string]bool{}
	for i := range pods {
		p := &pods[i]
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		for key, volume := range p.Labels {
			claim, ok := strings.CutPrefix(key, volumeLabelPrefix)
			if ok && volume != "" && claim == claimNameFor(agentName, volume) {
				held[volume] = true
			}
		}
	}
	return held
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PodVolume is one named volume a claim pod mounts.
type PodVolume struct {
	// Name is the volume's name in the agent config.
	Name string
	// ClaimName is the PVC behind it.
	ClaimName string
	// MountPath is cleaned and slash-separated: "." is the workspace root, a
	// relative path is under the workspace, an absolute path is a container
	// path.
	MountPath string
}

// cleanVolumeMountPath cleans one mount path and refuses one that climbs out
// of the workspace. An absolute path is allowed (see dsl.ValidateVolumes for
// why): on Kubernetes it is a container path and touches nothing else.
//
// An absolute path equal to the workspace itself is refused: it means the
// workspace root, which is spelled ".", and passing it on would give the pod two
// mounts at one path, which the API server rejects at create time with an
// opaque "Duplicate value". apply-time validation catches this for the default
// workspace; workspace is where THIS pod's workspace is, which a named agent
// template can move.
func cleanVolumeMountPath(i int, v dsl.VolumeMount, workspace string) (string, error) {
	if v.MountPath == "" {
		return "", fmt.Errorf("spec.volumes[%d]: mountPath is required", i)
	}
	mp := path.Clean(strings.ReplaceAll(v.MountPath, `\`, "/"))
	if !path.IsAbs(mp) && (mp == ".." || strings.HasPrefix(mp, "../")) {
		return "", fmt.Errorf("spec.volumes[%d]: mountPath %q escapes the workspace", i, v.MountPath)
	}
	if path.IsAbs(mp) && mp == path.Clean(workspace) {
		return "", fmt.Errorf("spec.volumes[%d]: mountPath %q is the workspace itself; write \".\" to mount the volume at the workspace root", i, v.MountPath)
	}
	return mp, nil
}

// uniqueMountPath is cleanVolumeMountPath plus a check that no earlier entry
// (recorded in seen) has the same cleaned path. Two mounts at one path reach the
// API server as "Duplicate value" and fail the pod with no hint which entries
// collided. Apply-time validation (dsl.ValidateVolumes) is the primary gate for
// this and rejects it with the same intent; this is the defence for a claim that
// got past it, and it also sees the cleaned form of the paths.
func uniqueMountPath(seen map[string]int, i int, v dsl.VolumeMount, workspace string) (string, error) {
	mp, err := cleanVolumeMountPath(i, v, workspace)
	if err != nil {
		return "", err
	}
	// Keyed by the container path the mount lands at, not by the path as
	// written: ".cache" and "/workspace/.cache" are different strings and the
	// same directory, and only the agent knows where the workspace is.
	where := mp
	switch {
	case mp == ".":
		where = path.Clean(workspace)
	case !path.IsAbs(mp):
		where = path.Join(workspace, mp)
	}
	if j, dup := seen[where]; dup {
		return "", fmt.Errorf("spec.volumes[%d]: mountPath %q is also used by spec.volumes[%d] (both mount at %s in the container); two volumes cannot share a mount path", i, v.MountPath, j, where)
	}
	seen[where] = i
	return mp, nil
}

// planPodVolumes turns a claim's volume mounts, each already narrowed to the
// one volume chosen, into the pod's volumes and into the path a step sees each
// alias at (the UNIFIED_VOLUME_<ALIAS> values). workspace is the container
// path of the workspace.
func planPodVolumes(agentName string, declared map[string]config.VolumeConfig, mounts []dsl.VolumeMount, workspace string) ([]PodVolume, map[string]string, error) {
	vols := make([]PodVolume, 0, len(mounts))
	paths := make(map[string]string, len(mounts))
	seenPaths := make(map[string]int, len(mounts))
	for i, v := range mounts {
		if len(v.Name) != 1 {
			return nil, nil, fmt.Errorf("spec.volumes[%d] names %d candidate volumes; exactly one must be chosen before mounting", i, len(v.Name))
		}
		name := v.Name[0]
		cfg, ok := declared[name]
		if !ok {
			return nil, nil, fmt.Errorf("spec.volumes[%d]: this agent does not declare a volume named %q", i, name)
		}
		alias := dsl.VolumeAlias(v)
		if alias == "" {
			return nil, nil, fmt.Errorf("spec.volumes[%d]: volume %q has no alias", i, name)
		}
		mp, err := uniqueMountPath(seenPaths, i, v, workspace)
		if err != nil {
			return nil, nil, err
		}
		if mp == "." && cfg.Mode == agentlib.VolumeModeShared {
			// Only the agent can know a volume's mode, so this is refused here
			// and not when the job is applied. Same rule, same words, as the
			// host agent.
			return nil, nil, fmt.Errorf("spec.volumes[%d]: volume %q is shared and cannot be mounted at the workspace root (mountPath %q): several runs would work in one checkout with no exclusion. Declare it exclusive in the agent config, or mount it at a subdirectory", i, name, v.MountPath)
		}
		vols = append(vols, PodVolume{Name: name, ClaimName: claimNameFor(agentName, name), MountPath: mp})
		switch {
		case mp == ".":
			paths[alias] = workspace
		case path.IsAbs(mp):
			paths[alias] = mp
		default:
			paths[alias] = path.Join(workspace, mp)
		}
	}
	return vols, paths, nil
}

// injectVolumes adds the claims in vols to podSpec. A volume at the workspace
// root IS the workspace, so its claim replaces the workspace emptyDir. Every
// other volume is mounted into every container that has the workspace (the
// artifact sidecar included), so an artifact or cache path that lies inside a
// volume reads the volume and not the empty directory the kubelet creates as
// its mountpoint.
//
// It must run after injectWorkspace: a nested mount has to come after the
// workspace mount it sits inside.
func injectVolumes(podSpec *corev1.PodSpec, workspaceMount string, vols []PodVolume) {
	for i, pv := range vols {
		src := corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pv.ClaimName},
		}
		if pv.MountPath == "." {
			for j := range podSpec.Volumes {
				if podSpec.Volumes[j].Name == "workspace" {
					podSpec.Volumes[j].VolumeSource = src
				}
			}
			continue
		}
		name := fmt.Sprintf("%s%d", podVolumeNamePrefix, i)
		podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{Name: name, VolumeSource: src})
		mp := pv.MountPath
		if !path.IsAbs(mp) {
			mp = path.Join(workspaceMount, mp)
		}
		for j := range podSpec.Containers {
			podSpec.Containers[j].VolumeMounts = append(podSpec.Containers[j].VolumeMounts, corev1.VolumeMount{Name: name, MountPath: mp})
		}
	}
}

// errRunTerminalWhileWaiting is returned by createClaimPod when the controller
// reported the run finished (cancelled by a user, or failed by the reaper)
// while it waited for a volume. There is nothing left to report for such a run.
var errRunTerminalWhileWaiting = errors.New("the controller reports the run finished while it waited for its volumes")

// maxCreateAttempts is how many consecutive creates of a volume-bearing run's
// pod may fail to land before the run is failed. The count starts over whenever
// the run has to wait for a busy volume in between, so it bounds a run that is
// stuck creating its pod, not one that waited hours for a volume and also met a
// few scattered transient timeouts. A create that times out or
// finds its previous attempt's pod still terminating is a slow cluster, and a
// slow cluster should cost a run time, not the run: failing on the first would
// turn one slow response during a fleet start into a failed build. But retrying
// without limit is worse than either, because a cause that does not clear
// (persistent timeouts, a name taken by a pod this agent will not delete) would
// hold the run and its concurrency slot indefinitely, with only the job's own
// timeoutMinutes, if it sets one, to end it. Five attempts rides out a bad
// minute (each timed-out one costs a quarter of the lock grace plus a poll
// interval) and still fails within a few minutes with the true reason.
const maxCreateAttempts = 5

// createRetryError says that the run's pod did not get created but the attempt is
// worth repeating (see tryCreateWithVolumes). It is not returned past
// createClaimPod.
type createRetryError struct {
	cause    error
	timedOut bool
	timeout  time.Duration
}

func (e *createRetryError) Error() string {
	return "creating the pod did not complete: " + e.cause.Error()
}
func (e *createRetryError) Unwrap() error { return e.cause }

// reason is the part of the message a user can act on.
func (e *createRetryError) reason() string {
	if e.timedOut {
		return fmt.Sprintf("the request to create it timed out after %s", e.timeout)
	}
	return "a pod with its name still exists from an earlier attempt, which has been asked to go away"
}

// claimPodResult is what createClaimPod hands back.
type claimPodResult struct {
	pod *corev1.Pod
	// volumePaths maps each volume alias to the path a step sees it at.
	volumePaths map[string]string
	// mounts are the volumes the pod mounts, as planned.
	mounts []PodVolume
	// rootVolume is true when a volume is mounted at the workspace root, which
	// makes the workspace that volume.
	rootVolume bool
	// release clears the run's volume locks; nil when the claim took none. The
	// caller runs it once the pod's deletion has been issued: the pod label
	// keeps the volume busy while the pod is still terminating.
	release func(context.Context)
}

// declaredVolumes indexes the configured volumes by name.
func (a *K8sAgent) declaredVolumes() map[string]config.VolumeConfig {
	m := make(map[string]config.VolumeConfig, len(a.cfg.Volumes))
	for _, c := range a.cfg.Volumes {
		m[c.Name] = c
	}
	return m
}

// heldVolumes reads which of this agent's volumes a live pod mounts, from the
// cluster, right now.
func (a *K8sAgent) heldVolumes(ctx context.Context) (map[string]bool, error) {
	pods, err := a.pm.ListPods(ctx, agentPodSelector)
	if err != nil {
		return nil, fmt.Errorf("list pods for volume occupancy: %w", err)
	}
	return heldVolumes(pods.Items, a.cfg.AgentName), nil
}

// claimRequest is the body of a normal claim: the volumes this agent declares
// and the exclusive ones a live pod holds. An agent with no volumes sends
// nothing, as before.
//
// The busy list is an optimisation that stops the agent being handed a run it
// would then wait on while holding a concurrency slot. Correctness is
// createClaimPod's check, taken again when the pod is about to be created. So
// a failure to read the pods degrades to an empty list rather than blocking the
// claim loop.
func (a *K8sAgent) claimRequest(ctx context.Context) api.ClaimRequest {
	if len(a.cfg.Volumes) == 0 {
		return api.ClaimRequest{}
	}
	req := api.ClaimRequest{Volumes: volumeNames(a.cfg.Volumes)}
	held, err := a.heldVolumes(ctx)
	if err != nil {
		slog.Warn("k8s: could not read volume occupancy for the claim; reporting none busy", "error", err)
		return req
	}
	declared := a.declaredVolumes()
	for _, name := range sortedKeys(held) {
		if c, ok := declared[name]; ok && c.Mode == agentlib.VolumeModeExclusive {
			req.BusyVolumes = append(req.BusyVolumes, name)
		}
	}
	return req
}

// createClaimPod builds and creates the claim's pod. For a claim with volumes
// it first chooses a free volume for each entry, waiting while the volumes it
// needs are held by another pod, and creates the pod with them mounted.
//
// Exclusion is atomic against another replica, not just this process: an
// exclusive volume is taken by an optimistic update of its PVC (volumes_lock.go)
// before the pod is created, so two replicas that both saw it free cannot both
// get it. volMu additionally serialises choosing within this process, which
// keeps its own claims from needlessly colliding on that update.
//
// A run that has to wait holds nothing. The wait ends, without an error to
// report, when the controller says the run is finished, and it ends with a
// failure when the agent starts draining (a.drain): a drain lets running work
// finish, but this run has not started, and waiting out a holder (a Unity build
// runs for tens of minutes) would hold its slot and stall the rollout. Nothing
// proceeds unmounted.
func (a *K8sAgent) createClaimPod(ctx context.Context, c api.ClaimResponse) (claimPodResult, error) {
	sidecar := SidecarSpec{Image: a.cfg.SidecarImage, S3SecretName: a.cfg.SidecarS3SecretName, S3SecretMode: a.cfg.SidecarS3SecretMode, BrokerURL: a.cfg.Server}
	if len(c.Volumes) == 0 {
		return a.buildAndCreate(ctx, c, sidecar, nil, nil)
	}

	declared := a.declaredVolumes()
	sets := make([][]string, len(c.Volumes))
	seenPaths := map[string]int{}
	for i, v := range c.Volumes {
		if _, err := uniqueMountPath(seenPaths, i, v, a.workspaceMount(c.PodTemplate)); err != nil {
			return claimPodResult{}, fmt.Errorf("k8s: %w", err)
		}
		for _, n := range v.Name {
			if _, ok := declared[n]; ok {
				sets[i] = append(sets[i], n)
			}
		}
		if len(sets[i]) == 0 {
			return claimPodResult{}, fmt.Errorf("k8s: spec.volumes[%d]: this agent declares none of %q", i, []string(v.Name))
		}
	}

	// waitCtx ends with the run context (shutdown, drain timeout) or with the
	// drain itself, whichever comes first.
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if a.drain != nil {
		stop := context.AfterFunc(a.drain, cancel)
		defer stop()
	}

	announced := false
	failedCreates := 0
	for {
		res, created, err := a.tryCreateWithVolumes(ctx, c, sidecar, sets, declared)
		var retry *createRetryError
		if errors.As(err, &retry) {
			// The volumes were free and this run held them; it is the pod
			// that did not land. Say so, not "another run is using", and give
			// up after maxCreateAttempts so a cause that does not clear
			// cannot hold the run, and its slot, under a wrong explanation.
			failedCreates++
			if failedCreates >= maxCreateAttempts {
				return claimPodResult{}, fmt.Errorf("k8s: creating the run's pod did not complete in %d consecutive attempts, with no wait for a busy volume between them; giving up. Last attempt: %w", failedCreates, retry.cause)
			}
			a.warnRun(waitCtx, c.RunID, fmt.Sprintf("creating the run's pod did not complete (attempt %d of %d: %s); its volume locks were released and it will try again",
				failedCreates, maxCreateAttempts, retry.reason()))
		} else if err != nil || created {
			return res, err
		} else {
			// The run waited for a busy volume, so the failed creates before
			// this are no longer "consecutive" with any that follow.
			failedCreates = 0
			if !announced {
				announced = true
				a.warnRun(waitCtx, c.RunID, fmt.Sprintf("waiting for volumes %q, which another run is using", sets))
			}
		}
		select {
		case <-waitCtx.Done():
			if ctx.Err() == nil && a.drain != nil && a.drain.Err() != nil {
				return claimPodResult{}, fmt.Errorf("k8s: run abandoned: the agent is draining and the run was still waiting for its volumes %q", sets)
			}
			return claimPodResult{}, fmt.Errorf("k8s: gave up waiting for volumes %q: %w", sets, waitCtx.Err())
		case <-time.After(agentlib.CancelPollInterval):
		}
		if run, err := a.client.GetRun(waitCtx, c.RunID); err == nil && isTerminalRunStatus(run.Status) {
			return claimPodResult{}, errRunTerminalWhileWaiting
		}
	}
}

// tryCreateWithVolumes chooses a free volume per entry and creates the pod, or
// reports created=false if some entry has no free candidate right now.
func (a *K8sAgent) tryCreateWithVolumes(ctx context.Context, c api.ClaimResponse, sidecar SidecarSpec, sets [][]string, declared map[string]config.VolumeConfig) (res claimPodResult, created bool, err error) {
	a.volMu.Lock()
	defer a.volMu.Unlock()

	held, err := a.heldVolumes(ctx)
	if err != nil {
		slog.Warn("k8s: could not read volume occupancy; treating the run as waiting", "runId", c.RunID, "error", err)
		return claimPodResult{}, false, nil
	}
	// A volume another replica won the lock on is set aside and the choice
	// made again, so a candidate list falls through to its next free entry
	// instead of waiting a whole poll interval for the same loser.
	contended := map[string]bool{}
	var chosen []string
	var release func(context.Context)
	for {
		free := func(n string) bool {
			return !contended[n] && (declared[n].Mode == agentlib.VolumeModeShared || !held[n])
		}
		var ok bool
		chosen, ok = agentlib.ChooseVolumes(sets, free)
		if !ok {
			return claimPodResult{}, false, nil
		}
		var lost string
		release, lost, err = a.lockExclusive(ctx, chosen, c.RunID)
		if err != nil {
			slog.Warn("k8s: could not lock the run's volumes; treating the run as waiting", "runId", c.RunID, "error", err)
			return claimPodResult{}, false, nil
		}
		if lost == "" {
			break
		}
		contended[lost] = true
	}
	// From here the run holds its volumes; give them back unless a pod now
	// exists that will do it when the run ends.
	defer func() {
		if !created {
			release(context.WithoutCancel(ctx))
		}
	}()

	narrowed := make([]dsl.VolumeMount, len(c.Volumes))
	for i, v := range c.Volumes {
		narrowed[i] = v
		narrowed[i].Name = dsl.StringOrSlice{chosen[i]}
	}
	vols, paths, err := planPodVolumes(a.cfg.AgentName, declared, narrowed, a.workspaceMount(c.PodTemplate))
	if err != nil {
		return claimPodResult{}, false, fmt.Errorf("k8s: %w", err)
	}
	// Bounded well under the lock's grace period. The lock is only honoured for
	// the grace while its run has no pod, so a create that stalls past it would
	// let another replica take the lock over and create its own pod.
	createCtx, cancelCreate := context.WithTimeout(ctx, a.lockGrace()/4)
	res, err = a.buildAndCreate(createCtx, c, sidecar, vols, paths)
	cancelCreate()
	if err != nil {
		// A slow cluster should cost a run time, not the run. A create that ran
		// out of its own deadline (not the run's), or that found the previous
		// attempt's pod still terminating (the name is deterministic, and
		// buildAndCreate has just asked for it to be deleted), is retried: the
		// deferred release gives the lock back and the run goes on waiting.
		timedOut := errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil
		if timedOut || apierrors.IsAlreadyExists(err) {
			slog.Warn("k8s: creating the run's pod did not complete; releasing its volume locks and retrying",
				"runId", c.RunID, "timedOut", timedOut, "error", err)
			return claimPodResult{}, false, &createRetryError{cause: err, timedOut: timedOut, timeout: a.lockGrace() / 4}
		}
		return claimPodResult{}, false, err
	}

	// The lock is confirmed again now that the pod exists (confirmLock explains
	// why that has to be a compare-and-swap rather than a read). If it no longer
	// names this run, this pod must not stay: two pods would mount one exclusive
	// volume. Delete it and go back to waiting.
	lost, err := a.confirmLock(ctx, chosen, c.RunID)
	if err != nil || lost != "" {
		slog.Warn("k8s: lost the lock on a volume while creating the pod; removing the pod and waiting",
			"runId", c.RunID, "volume", lost, "error", err)
		delCtx, cancelDel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancelDel()
		if derr := a.pm.DeletePod(delCtx, res.pod.Name); derr != nil && !apierrors.IsNotFound(derr) {
			// A pod we could not remove still mounts the volume and carries this
			// run's label; waiting on with it in place would be wrong. Failing
			// the run lets the pod GC remove it.
			return claimPodResult{}, false, fmt.Errorf("k8s: lost the lock on volume %q while creating the pod, and could not delete the pod %s: %w", lost, res.pod.Name, derr)
		}
		return claimPodResult{}, false, nil
	}
	res.release = release
	return res, true, nil
}

// buildAndCreate builds the claim pod with vols mounted and creates it.
func (a *K8sAgent) buildAndCreate(ctx context.Context, c api.ClaimResponse, sidecar SidecarSpec, vols []PodVolume, paths map[string]string) (claimPodResult, error) {
	pod, err := BuildPod(c.RunID, a.cfg.Namespace, a.cfg.PodTemplates, c.PodTemplate, a.cfg.PodImage, sidecar, a.cfg.ShimImage, vols)
	if err != nil {
		return claimPodResult{}, fmt.Errorf("k8s: failed to build Pod spec: %w", err)
	}
	made, err := a.pm.CreatePod(ctx, pod)
	if err != nil {
		if len(vols) > 0 {
			// A create that timed out client-side may still have succeeded
			// server-side. A pod that mounts a volume must not outlive the lock
			// that is about to be given back, so remove it if it exists.
			delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			a.removeOwnPod(delCtx, c.RunID, pod.Name)
		}
		return claimPodResult{}, fmt.Errorf("k8s: failed to create Pod: %w", err)
	}
	res := claimPodResult{pod: made, volumePaths: paths, mounts: vols}
	for _, v := range vols {
		if v.MountPath == "." {
			res.rootVolume = true
		}
	}
	return res, nil
}

// removeOwnPod deletes the pod called name if, and only if, it belongs to run
// runID (carries its run label). The name is derived from a truncated run ID, so
// a name match alone is not proof the pod is this run's, and a pod that is not
// must never be deleted from here. Absence is the normal case (the create did not
// land) and is silent; a pod that is there and cannot be removed may still mount
// the run's volumes, which is worth a warning.
func (a *K8sAgent) removeOwnPod(ctx context.Context, runID, name string) {
	pods, err := a.pm.ListPods(ctx, labelRunID+"="+runID)
	if err != nil {
		slog.Warn("k8s: could not look for a pod left by a failed create; it may still mount the run's volumes until the pod GC removes it",
			"pod", name, "error", err)
		return
	}
	for i := range pods.Items {
		if pods.Items[i].Name != name {
			continue
		}
		if err := a.pm.DeletePod(ctx, name); err != nil && !apierrors.IsNotFound(err) {
			slog.Warn("k8s: could not remove a pod left by a failed create; it may still mount the run's volumes until the pod GC removes it",
				"pod", name, "error", err)
		}
	}
}

// workspaceMount is the container path of a claim's workspace, resolved the way
// BuildPod resolves it (resolveWorkspaceConfig): a named agent template's
// workspace.mountPath counts, overridden by the job's.
func (a *K8sAgent) workspaceMount(pt *dsl.PodTemplate) string {
	return workspaceMountOf(resolveWorkspaceConfig(a.cfg.PodTemplates, pt))
}

// volumeMeasureTimeout bounds one pass of measuring a run's mounted volumes. A
// measurement is a metric: a container that will not answer must cost the run
// at most this long, never its result. A variable so a test can shorten it.
var volumeMeasureTimeout = 15 * time.Second

// statfsLine is the one line `ucd-sh statfs` prints on success. Anchored and
// digits-only on purpose: anything else is not a reading.
var statfsLine = regexp.MustCompile(`^free=([0-9]+) total=([0-9]+)\n$`)

// parseStatfs reads the output of `ucd-sh statfs`. Anything but exactly that
// one line is an error, so a truncated or noisy stdout can never be mistaken
// for a measurement. A total of 0 and a free larger than the total are refused
// too: neither is a filesystem, and a made-up one is worse than no reading.
func parseStatfs(out string) (api.FilesystemStats, error) {
	m := statfsLine.FindStringSubmatch(out)
	if m == nil {
		return api.FilesystemStats{}, fmt.Errorf("unexpected statfs output %q", out)
	}
	free, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil {
		return api.FilesystemStats{}, fmt.Errorf("statfs free: %w", err)
	}
	total, err := strconv.ParseUint(m[2], 10, 64)
	if err != nil {
		return api.FilesystemStats{}, fmt.Errorf("statfs total: %w", err)
	}
	if total == 0 || free > total {
		return api.FilesystemStats{}, fmt.Errorf("implausible statfs reading free=%d total=%d", free, total)
	}
	return api.FilesystemStats{FreeBytes: free, TotalBytes: total}, nil
}

// volumeReadings is the latest measurement of each volume, by volume name. It
// lives in memory only and is deliberately never cleared: a volume's headroom
// is still its last known headroom between the runs that mount it, and every
// reading carries its own MeasuredAt so a consumer can see how old it is. A
// restart loses it, and those series are then absent until a run next mounts
// the volume. Runs write it concurrently and the heartbeat reads it, so it
// carries its own lock; the zero value is ready to use.
type volumeReadings struct {
	mu sync.Mutex
	m  map[string]api.VolumeStats
}

// set records a reading, unless a newer one for the volume is already held:
// two runs sharing a volume can finish measuring out of order.
func (r *volumeReadings) set(name string, fs api.FilesystemStats, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.m[name]; ok && at.Before(cur.MeasuredAt) {
		return
	}
	if r.m == nil {
		r.m = map[string]api.VolumeStats{}
	}
	r.m[name] = api.VolumeStats{FilesystemStats: fs, MeasuredAt: at}
}

// snapshot returns a copy the caller may keep, since later measurements must
// not change a reading a heartbeat is still encoding.
func (r *volumeReadings) snapshot() map[string]api.VolumeStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]api.VolumeStats, len(r.m))
	for name, v := range r.m {
		out[name] = v
	}
	return out
}

// storageReading is the heartbeat's storage provider. It only reads the cache:
// it runs on the heartbeat goroutine, which must never wait on an exec, and a
// measurement that hung there would stop the agent heartbeating and get its
// running jobs reaped. Measuring happens during a run (measureVolumes).
//
// A Kubernetes agent has no workspace directory of its own, no minFreeDisk and
// no claim-time disk check, so Workspace, MinFreeDiskBytes and
// ClaimBlockedByDisk stay nil: "this agent has no such thing". A zero or a
// false would read as plenty of space and as not blocked.
func (a *K8sAgent) storageReading() *api.AgentStorage {
	return &api.AgentStorage{AgentName: a.cfg.AgentName, Volumes: a.volumeStats.snapshot()}
}

// measureVolumes records the free and total space of each volume vols mounts
// into the run pod, by running `ucd-sh statfs` in the pod's container: the
// agent cannot reach a PVC's filesystem from outside the pod, and a job image
// cannot be assumed to carry df. workspace is where the pod mounted the
// workspace; a volume at its root is measured there.
//
// Best effort, by design. A failed or unparseable measurement logs and caches
// nothing: no entry, no zero, no timestamp, and the previous reading stands.
// It never fails the run, and the whole pass is bounded by volumeMeasureTimeout.
func (a *K8sAgent) measureVolumes(ctx context.Context, podName, container, workspace string, vols []PodVolume) {
	if len(vols) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, volumeMeasureTimeout)
	defer cancel()
	for _, v := range vols {
		if ctx.Err() != nil {
			// Out of time, or the run was cancelled: the rest wait for a later run.
			return
		}
		loc := podVolumeLocation(workspace, v.MountPath)
		var stdout bytes.Buffer
		exit, err := a.exec.ExecStepArgv(ctx, podName, container, []string{ucdShimPath, "statfs", loc}, &stdout, io.Discard)
		if err == nil && exit != 0 {
			err = fmt.Errorf("exited %d", exit)
		}
		var fs api.FilesystemStats
		if err == nil {
			fs, err = parseStatfs(stdout.String())
		}
		if err != nil {
			slog.Warn("k8s: could not measure volume", "volume", v.Name, "path", loc, "pod", podName, "error", err)
			continue
		}
		a.volumeStats.set(v.Name, fs, time.Now().UTC())
	}
}
