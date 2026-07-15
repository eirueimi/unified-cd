package k8sagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	annoPoolTemplate = "unified-cd/pool-template"
	// annoPoolKey carries the pod-shape hash (see poolKey) a pooled pod was
	// created under, so Restore can re-adopt it into the right pool bucket
	// across an agent restart. annoPoolTemplate stays alongside it as the
	// human-readable template name for observability/logs only — it is NOT the
	// pool index (it is empty for inline/unnamed templates and ignores
	// per-job overrides, both of which the hash captures).
	annoPoolKey     = "unified-cd/pool-key"
	annoPoolStatus  = "unified-cd/pool-status"
	annoPoolRunID   = "unified-cd/pool-run-id"
	poolStatusIdle  = "idle"
	poolStatusInUse = "in-use"
)

// PooledPod represents a Pod entry in the pool. PoolKey is the pod-shape hash
// (see poolKey) the pod is pooled under — NOT the template name.
type PooledPod struct {
	PodName         string
	PoolKey         string
	ResourceVersion string
	IdleSince       time.Time // when this pod last became idle
}

// PodPool pools and reuses Pods keyed by the hash of their effective pod
// shape (see poolKey), so two jobs only ever share a pod when every input
// that shapes the pod is identical.
type PodPool struct {
	mu          sync.Mutex
	pods        map[string][]*PooledPod // poolKey (pod-shape hash) → idle pods
	client      kubernetes.Interface
	namespace   string
	pm          *PodManager
	idleTimeout time.Duration // 0 = no eviction
}

// poolKey returns a deterministic identifier for the effective pod shape a
// claim's inputs produce, so the pool only hands a pod to a claim whose
// inputs would build the exact same pod. It covers EVERYTHING BuildPod
// consumes — the template name, the resolved agent-side template for that
// name (agentTmpls[templateName] only, not the whole map), the job's full
// podTemplate (including Override, Spec, Workspace, Reuse), the fallback
// image, the sidecar spec, and the shim image — and deliberately EXCLUDES
// run-specific data (runID etc.). This is what separates named vs unnamed
// templates, two different inline specs (which both have templateName ""),
// and the same named template with different per-job overrides.
//
// Determinism: a struct (not a map) is marshaled, so json.Marshal emits
// fields in fixed declaration order; nested map[string]any values (template
// specs, override patches) are marshaled by encoding/json with sorted keys.
// The digest is therefore stable across calls and process restarts, which
// Restore relies on to re-adopt pods by their annotated key. If json.Marshal
// fails (rare, only if the spec contains an unmarshalable value), the fallback
// hashes only scalar fields (templateName, fallbackImage, shimImage, and
// sidecar.Image + sidecar.S3SecretName) for determinism; this path loses
// override/spec distinction but is deterministic and only reached if the spec
// is unmarshalable (BuildPod would reject it anyway).
func poolKey(templateName string, agentTmpls map[string]AgentPodTemplate, jobTmpl *dsl.PodTemplate, fallbackImage string, sidecar SidecarSpec, shimImage string) string {
	type canonicalShape struct {
		TemplateName  string            `json:"templateName"`
		AgentTemplate *AgentPodTemplate `json:"agentTemplate,omitempty"`
		JobTemplate   *dsl.PodTemplate  `json:"jobTemplate,omitempty"`
		FallbackImage string            `json:"fallbackImage"`
		Sidecar       SidecarSpec       `json:"sidecar"`
		ShimImage     string            `json:"shimImage"`
	}
	shape := canonicalShape{
		TemplateName:  templateName,
		JobTemplate:   jobTmpl,
		FallbackImage: fallbackImage,
		Sidecar:       sidecar,
		ShimImage:     shimImage,
	}
	if templateName != "" {
		if at, ok := agentTmpls[templateName]; ok {
			shape.AgentTemplate = &at
		}
	}
	data, err := json.Marshal(shape)
	if err != nil {
		// A YAML-sourced map[string]any could in principle carry a value
		// encoding/json rejects. Fall back to hashing scalar fields only:
		// templateName, fallbackImage, shimImage, and sidecar scalars.
		// This is deterministic for equal inputs but loses spec/override
		// distinction (acceptable since this path is only reached if the spec
		// is unmarshalable, which BuildPod would reject anyway).
		scalarKey := fmt.Sprintf("%s:%s:%s:%s:%s", templateName, fallbackImage, shimImage, sidecar.Image, sidecar.S3SecretName)
		data = []byte(scalarKey)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

// NewPodPool creates a new PodPool.
func NewPodPool(client kubernetes.Interface, namespace string, pm *PodManager) *PodPool {
	return &PodPool{
		pods:      make(map[string][]*PooledPod),
		client:    client,
		namespace: namespace,
		pm:        pm,
	}
}

// SetIdleTimeout configures how long idle pods are kept before being deleted.
// A value of 0 disables eviction (default).
func (p *PodPool) SetIdleTimeout(d time.Duration) {
	p.idleTimeout = d
}

// StartEviction launches a background goroutine that deletes idle pods older than idleTimeout.
// No-op if idleTimeout is 0. Returns immediately; the goroutine stops when ctx is cancelled.
func (p *PodPool) StartEviction(ctx context.Context) {
	if p.idleTimeout <= 0 {
		return
	}
	interval := p.idleTimeout / 2
	if interval < 30*time.Second {
		interval = 30 * time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.evictExpired(ctx)
			}
		}
	}()
}

func (p *PodPool) evictExpired(ctx context.Context) {
	p.mu.Lock()
	var toDelete []*PooledPod
	for key, idle := range p.pods {
		kept := idle[:0]
		for _, pp := range idle {
			if time.Since(pp.IdleSince) >= p.idleTimeout {
				toDelete = append(toDelete, pp)
			} else {
				kept = append(kept, pp)
			}
		}
		p.pods[key] = kept
	}
	p.mu.Unlock()

	for _, pp := range toDelete {
		slog.Info("pool: evicting idle pod (timeout)", "pod", pp.PodName, "idleSince", pp.IdleSince)
		if err := p.pm.DeletePod(ctx, pp.PodName); err != nil {
			slog.Warn("pool: evict delete failed", "pod", pp.PodName, "error", err)
		}
	}
}

// ClaimPod acquires a Pod whose effective shape matches the given inputs
// (see poolKey). Reuses an idle Pod with the same pool key if available;
// otherwise creates a new one. Keying by pod-shape hash (never by template
// name alone) is what guarantees two unrelated jobs — e.g. two unnamed
// inline templates with different images, or the same named template with
// different overrides — are never handed each other's pod.
func (p *PodPool) ClaimPod(ctx context.Context, runID, templateName string, agentTmpls map[string]AgentPodTemplate, jobTmpl *dsl.PodTemplate, fallbackImage string, sidecar SidecarSpec, shimImage string) (*PooledPod, error) {
	key := poolKey(templateName, agentTmpls, jobTmpl, fallbackImage, sidecar, shimImage)

	p.mu.Lock()
	defer p.mu.Unlock()

	idle := p.pods[key]
	if len(idle) > 0 {
		pp := idle[len(idle)-1]
		p.pods[key] = idle[:len(idle)-1]

		// Update annotation to in-use (optimistic concurrency control)
		err := p.pm.UpdatePodAnnotations(ctx, pp.PodName, map[string]string{
			annoPoolStatus: poolStatusInUse,
			annoPoolRunID:  runID,
		}, pp.ResourceVersion)
		if err != nil {
			// conflict or fetch failure → fall back to creating a new pod.
			// The pod was popped from the pool and its claim failed; delete it
			// so it isn't orphaned — a fresh pod is created below.
			slog.Warn("pool: pod claim conflict, creating new pod", "pod", pp.PodName, "error", err)
			_ = p.pm.DeletePod(ctx, pp.PodName)
			return p.createPoolPod(ctx, runID, key, agentTmpls, jobTmpl, fallbackImage, sidecar, shimImage)
		}
		pod, _ := p.client.CoreV1().Pods(p.namespace).Get(ctx, pp.PodName, metav1.GetOptions{})
		if pod != nil {
			pp.ResourceVersion = pod.ResourceVersion
		}
		return pp, nil
	}

	return p.createPoolPod(ctx, runID, key, agentTmpls, jobTmpl, fallbackImage, sidecar, shimImage)
}

// createPoolPod builds and creates a fresh pooled Pod, stamping it with the
// given pool key (annoPoolKey) so Restore can re-adopt it after an agent
// restart. BuildPod already sets annoPoolStatus/annoPoolTemplate on reuse
// pods; only annoPoolKey is added here.
func (p *PodPool) createPoolPod(ctx context.Context, runID, key string, agentTmpls map[string]AgentPodTemplate, jobTmpl *dsl.PodTemplate, fallbackImage string, sidecar SidecarSpec, shimImage string) (*PooledPod, error) {
	pod, err := BuildPod(runID, p.namespace, agentTmpls, jobTmpl, fallbackImage, sidecar, shimImage)
	if err != nil {
		return nil, err
	}
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[annoPoolKey] = key
	created, err := p.pm.CreatePod(ctx, pod)
	if err != nil {
		return nil, err
	}
	return &PooledPod{
		PodName:         created.Name,
		PoolKey:         key,
		ResourceVersion: created.ResourceVersion,
	}, nil
}

// ReleasePod returns a completed Run's Pod to the pool or deletes it.
func (p *PodPool) ReleasePod(ctx context.Context, pp *PooledPod, reuse bool) error {
	if !reuse {
		return p.pm.DeletePod(ctx, pp.PodName)
	}

	pod, err := p.client.CoreV1().Pods(p.namespace).Get(ctx, pp.PodName, metav1.GetOptions{})
	if err != nil {
		return p.pm.DeletePod(ctx, pp.PodName)
	}
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[annoPoolStatus] = poolStatusIdle
	delete(pod.Annotations, annoPoolRunID)
	updated, err := p.client.CoreV1().Pods(p.namespace).Update(ctx, pod, metav1.UpdateOptions{})
	if err != nil {
		slog.Warn("pool: failed to mark pod idle, deleting", "pod", pp.PodName, "error", err)
		return p.pm.DeletePod(ctx, pp.PodName)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.pods[pp.PoolKey] = append(p.pods[pp.PoolKey], &PooledPod{
		PodName:         pp.PodName,
		PoolKey:         pp.PoolKey,
		ResourceVersion: updated.ResourceVersion,
		IdleSince:       time.Now(),
	})
	return nil
}

// masterRunGetter is an interface for fetching Run state from the master on agent restart.
type masterRunGetter interface {
	GetRun(ctx context.Context, runID string) (api.Run, error)
}

// Restore scans existing Pods in Kubernetes at agent startup and restores the pool state.
// If masterClient is nil, all in-use Pods are deleted.
func (p *PodPool) Restore(ctx context.Context, masterClient masterRunGetter) error {
	pods, err := p.client.CoreV1().Pods(p.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=unified-cd-agent",
	})
	if err != nil {
		return fmt.Errorf("pool restore: pod list failed: %w", err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	for i := range pods.Items {
		pod := &pods.Items[i]
		status := pod.Annotations[annoPoolStatus]
		if status == "" {
			// Not a pool-managed pod at all (annoPoolStatus is the marker the
			// GC and this loop key on — annoPoolTemplate alone can't tell
			// "pool pod with an unnamed/inline template" apart from "not a
			// pool pod").
			continue
		}
		key := pod.Annotations[annoPoolKey]
		if key == "" {
			// Pool-managed but missing the pool-key annotation: created by an
			// older agent build from before the pool was keyed by pod-shape
			// hash. There is no key to re-adopt it under, and guessing one
			// (e.g. from the template name) would risk handing the pod to a
			// claim with a different effective spec — the exact collision the
			// pool key exists to prevent. Delete it; the GC's
			// annoPoolStatus-based protection means nothing else ever would.
			// Applies uniformly whether idle or in-use (an in-use one's run is
			// being reconciled some other way regardless).
			slog.Info("pool: deleting pooled pod without pool-key annotation (pre-pool-key agent build)", "pod", pod.Name, "status", status)
			_ = p.pm.DeletePod(ctx, pod.Name)
			continue
		}
		tmpl := pod.Annotations[annoPoolTemplate] // human-readable, logs only
		switch status {
		case poolStatusIdle:
			p.pods[key] = append(p.pods[key], &PooledPod{
				PodName:         pod.Name,
				PoolKey:         key,
				ResourceVersion: pod.ResourceVersion,
				IdleSince:       time.Now(),
			})
			slog.Info("pool: restored idle pod", "pod", pod.Name, "poolKey", key, "template", tmpl)

		case poolStatusInUse:
			runID := pod.Annotations[annoPoolRunID]
			if masterClient != nil && runID != "" {
				run, getErr := masterClient.GetRun(ctx, runID)
				if getErr == nil && (run.Status == api.RunSucceeded || run.Status == api.RunFailed || run.Status == api.RunCancelled) {
					if pod.Annotations == nil {
						pod.Annotations = map[string]string{}
					}
					pod.Annotations[annoPoolStatus] = poolStatusIdle
					delete(pod.Annotations, annoPoolRunID)
					updated, err := p.client.CoreV1().Pods(p.namespace).Update(ctx, pod, metav1.UpdateOptions{})
					if err == nil {
						p.pods[key] = append(p.pods[key], &PooledPod{
							PodName:         pod.Name,
							PoolKey:         key,
							ResourceVersion: updated.ResourceVersion,
							IdleSince:       time.Now(),
						})
						slog.Info("pool: restored in-use pod as idle (run finished)", "pod", pod.Name, "run", runID, "poolKey", key, "template", tmpl)
						continue
					}
				}
			}
			slog.Info("pool: deleting orphaned in-use pod", "pod", pod.Name, "run", runID)
			_ = p.pm.DeletePod(ctx, pod.Name)
		}
	}
	return nil
}
