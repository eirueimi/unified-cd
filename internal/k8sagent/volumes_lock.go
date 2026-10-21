package k8sagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
)

// Exclusion across replicas.
//
// An exclusive volume is held by at most one run, and Kubernetes does not give
// that for free: ReadWriteOnce is a NODE-level constraint, so two pods on one
// node can both mount the claim. Replicas of one Deployment share a config and
// therefore share claims, so "read the pods, see the volume free, create a pod"
// is a read-modify-write that two replicas can both win.
//
// The PVC is therefore the lock. Each volume has exactly one PVC, and a run takes
// the volume by writing its ID into an annotation with an optimistic update (the
// Update carries the resourceVersion that was read, so the API server rejects it
// if anyone else wrote the claim in between). The loser re-reads, sees a live
// holder, and moves on or waits. The lock is taken BEFORE the pod is created and
// released AFTER the pod's deletion has been issued, so a pod can only ever exist
// for a run that holds the lock.
//
// Nothing about the lock lives in a process. A holder that died leaves its
// annotation behind; it is treated as stale, and taken over, once the named run
// has no live pod and the grace period has passed (the grace covers the moment
// between writing the annotation and creating the pod). That grace is not a
// guarantee, since nothing renews the annotation and replicas' clocks can
// disagree about its age, so the lock is CONFIRMED after the pod is created, by a
// compare-and-swap that re-stamps it (confirmLock): a run whose lock no longer
// names it deletes its pod and goes back to waiting (tryCreateWithVolumes). The
// create is also bounded to a quarter of the grace, and a create that times out
// is retried, not fatal. The pod labels (BusyVolumes) remain the record of who is
// using a volume right now, and the choice of volume consults both.
const (
	// annoVolumeHeldBy names the run that holds the volume; empty or absent means
	// nobody does.
	annoVolumeHeldBy = "unified-cd/volume-held-by"
	// annoVolumeHeldAt is when the holder wrote annoVolumeHeldBy (RFC 3339).
	annoVolumeHeldAt = "unified-cd/volume-held-at"
	// labelRunID is the label every pod of a run carries (BuildPod, buildScopePod).
	labelRunID = "unified-cd/runId"

	// volumeLockAttempts bounds the read-modify-write retries for one volume. A
	// conflict normally means another replica won, which the re-read reveals; the
	// bound only stops a pathological loop.
	volumeLockAttempts = 5
)

// defaultVolumeLockGrace is how long a lock whose run has no live pod is still
// honoured.
const defaultVolumeLockGrace = 2 * time.Minute

// lockGrace is the agent's grace period. It lives on the agent, not in a
// package variable, so a test can shorten it for one agent without writing
// state other goroutines read (the discipline agentlib.CancelPollInterval,
// which is a variable, had to learn the hard way).
func (a *K8sAgent) lockGrace() time.Duration {
	if a.lockGraceOverride > 0 {
		return a.lockGraceOverride
	}
	return defaultVolumeLockGrace
}

// lockVolume tries to take volume for runID. It reports false, with no error,
// when a live run holds it (or another replica won the update).
func (a *K8sAgent) lockVolume(ctx context.Context, volume, runID string) (bool, error) {
	if a.kube == nil {
		return false, fmt.Errorf("no Kubernetes client to lock volume %q with", volume)
	}
	pvcs := a.kube.CoreV1().PersistentVolumeClaims(a.cfg.Namespace)
	name := claimNameFor(a.cfg.AgentName, volume)
	for attempt := 0; attempt < volumeLockAttempts; attempt++ {
		pvc, err := pvcs.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("volume %q: read PVC %s: %w", volume, name, err)
		}
		holder := pvc.Annotations[annoVolumeHeldBy]
		if holder != "" && holder != runID {
			live, err := a.holderIsLive(ctx, pvc, holder)
			if err != nil {
				return false, err
			}
			if live {
				return false, nil
			}
			slog.Warn("k8s: taking over a volume lock whose run has no live pod", "volume", volume, "staleHolder", holder, "runId", runID)
		}
		if holder == runID {
			return true, nil
		}
		if pvc.Annotations == nil {
			pvc.Annotations = map[string]string{}
		}
		pvc.Annotations[annoVolumeHeldBy] = runID
		pvc.Annotations[annoVolumeHeldAt] = time.Now().UTC().Format(time.RFC3339)
		_, err = pvcs.Update(ctx, pvc, metav1.UpdateOptions{})
		switch {
		case err == nil:
			return true, nil
		case apierrors.IsConflict(err):
			// Someone wrote the claim since we read it. Read again: if it was a
			// rival taking the lock the next pass sees a live holder.
			continue
		default:
			return false, fmt.Errorf("volume %q: lock PVC %s: %w", volume, name, err)
		}
	}
	return false, nil
}

// unlockVolume clears runID's hold on volume. It leaves a lock another run has
// since taken over alone, and a PVC that is gone.
func (a *K8sAgent) unlockVolume(ctx context.Context, volume, runID string) error {
	if a.kube == nil {
		return nil
	}
	pvcs := a.kube.CoreV1().PersistentVolumeClaims(a.cfg.Namespace)
	name := claimNameFor(a.cfg.AgentName, volume)
	for attempt := 0; attempt < volumeLockAttempts; attempt++ {
		pvc, err := pvcs.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("volume %q: read PVC %s: %w", volume, name, err)
		}
		if pvc.Annotations[annoVolumeHeldBy] != runID {
			return nil
		}
		delete(pvc.Annotations, annoVolumeHeldBy)
		delete(pvc.Annotations, annoVolumeHeldAt)
		_, err = pvcs.Update(ctx, pvc, metav1.UpdateOptions{})
		switch {
		case err == nil, apierrors.IsNotFound(err):
			return nil
		case apierrors.IsConflict(err):
			continue
		default:
			return fmt.Errorf("volume %q: unlock PVC %s: %w", volume, name, err)
		}
	}
	return fmt.Errorf("volume %q: could not clear the lock on PVC %s after %d conflicting updates", volume, name, volumeLockAttempts)
}

// holderIsLive reports whether the run named by a lock still holds the volume: it
// wrote the lock within the grace period, or one of its pods is still alive.
func (a *K8sAgent) holderIsLive(ctx context.Context, pvc *corev1.PersistentVolumeClaim, holder string) (bool, error) {
	if at, err := time.Parse(time.RFC3339, pvc.Annotations[annoVolumeHeldAt]); err != nil || time.Since(at) < a.lockGrace() {
		// An unreadable timestamp is not evidence of staleness.
		return true, nil
	}
	pods, err := a.pm.ListPods(ctx, labelRunID+"="+holder)
	if err != nil {
		return false, fmt.Errorf("list pods of run %s to judge its volume lock: %w", holder, err)
	}
	for i := range pods.Items {
		if p := pods.Items[i].Status.Phase; p != corev1.PodSucceeded && p != corev1.PodFailed {
			return true, nil
		}
	}
	return false, nil
}

// confirmLock is called once the run's pod exists. For every exclusive volume in
// chosen it checks, and re-stamps, the lock with a compare-and-swap Update, and
// returns the first volume whose lock no longer names runID. An error is to be
// treated as a lost lock: not knowing is never read as holding.
//
// It is an Update and not a read for a reason a read cannot supply. Another
// replica whose clock runs ahead of this one's by more than the grace can have
// read the lock, found its run with no live pod (the pod was not created yet) and
// be about to write the takeover with the resourceVersion it read. A bare Get does
// not change the resourceVersion, so that pending write would still succeed
// after we looked, and both runs would pass their checks. Our Update bumps the
// resourceVersion, which makes the rival's pending write a Conflict; the rival
// re-reads, finds the lock freshly stamped and, with our pod now listed, held.
// If the rival's write lands first instead, ours conflicts and the re-read here
// names the rival. Either order has exactly one winner.
//
// Re-stamping heldAt also starts the grace period afresh from the moment the pod
// is known to exist.
func (a *K8sAgent) confirmLock(ctx context.Context, chosen []string, runID string) (string, error) {
	declared := a.declaredVolumes()
	seen := map[string]bool{}
	var exclusive []string
	for _, n := range chosen {
		if !seen[n] && declared[n].Mode == agentlib.VolumeModeExclusive {
			seen[n] = true
			exclusive = append(exclusive, n)
		}
	}
	if len(exclusive) == 0 {
		return "", nil // nothing was locked, so nothing to confirm
	}
	if a.kube == nil {
		return exclusive[0], errNoKubeClient
	}
	pvcs := a.kube.CoreV1().PersistentVolumeClaims(a.cfg.Namespace)
	for _, n := range exclusive {
		confirmed := false
		for attempt := 0; attempt < volumeLockAttempts && !confirmed; attempt++ {
			pvc, err := pvcs.Get(ctx, claimNameFor(a.cfg.AgentName, n), metav1.GetOptions{})
			if err != nil {
				return n, fmt.Errorf("volume %q: re-read PVC to confirm the lock: %w", n, err)
			}
			if pvc.Annotations[annoVolumeHeldBy] != runID {
				return n, nil
			}
			pvc.Annotations[annoVolumeHeldAt] = time.Now().UTC().Format(time.RFC3339)
			_, err = pvcs.Update(ctx, pvc, metav1.UpdateOptions{})
			switch {
			case err == nil:
				confirmed = true
			case apierrors.IsConflict(err):
				// Somebody wrote the claim between our read and our write; the
				// next pass reads it again and names the winner.
			default:
				return n, fmt.Errorf("volume %q: confirm the lock on PVC %s: %w", n, claimNameFor(a.cfg.AgentName, n), err)
			}
		}
		if !confirmed {
			return n, fmt.Errorf("volume %q: could not confirm the lock after %d conflicting updates", n, volumeLockAttempts)
		}
	}
	return "", nil
}

var errNoKubeClient = errors.New("no Kubernetes client to lock volumes with")

// lockExclusive takes the lock of every exclusive volume in chosen, in name
// order, and never waits while holding one: on the first volume it cannot take it
// releases what it took and names the volume, so two replicas contending for
// overlapping sets cannot each hold what the other needs. The returned release
// clears every lock it took and is safe to call more than once.
func (a *K8sAgent) lockExclusive(ctx context.Context, chosen []string, runID string) (release func(context.Context), contended string, err error) {
	seen := map[string]bool{}
	var names []string
	declared := a.declaredVolumes()
	for _, n := range chosen {
		if !seen[n] && declared[n].Mode == agentlib.VolumeModeExclusive {
			seen[n] = true
			names = append(names, n)
		}
	}
	sort.Strings(names)

	var taken []string
	release = func(rctx context.Context) {
		for _, n := range taken {
			if err := a.unlockVolume(rctx, n, runID); err != nil {
				slog.Warn("k8s: could not release a volume lock; it goes stale once the run has no live pod", "volume", n, "runId", runID, "error", err)
			}
		}
		taken = nil
	}
	for _, n := range names {
		ok, lerr := a.lockVolume(ctx, n, runID)
		if lerr != nil {
			release(context.WithoutCancel(ctx))
			return nil, "", lerr
		}
		if !ok {
			release(context.WithoutCancel(ctx))
			return nil, n, nil
		}
		taken = append(taken, n)
	}
	return release, "", nil
}
