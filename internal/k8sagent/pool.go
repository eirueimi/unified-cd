package k8sagent

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/unified-cd/unified-cd/internal/api"
	"github.com/unified-cd/unified-cd/internal/dsl"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	annoPoolTemplate = "unified-cd/pool-template"
	annoPoolStatus   = "unified-cd/pool-status"
	annoPoolRunID    = "unified-cd/pool-run-id"
	poolStatusIdle   = "idle"
	poolStatusInUse  = "in-use"
)

// PooledPod represents a Pod entry in the pool.
type PooledPod struct {
	PodName         string
	Template        string
	ResourceVersion string
	IdleSince       time.Time // when this pod last became idle
}

// PodPool pools and reuses Pods per template name.
type PodPool struct {
	mu          sync.Mutex
	pods        map[string][]*PooledPod // template name → idle pods
	client      kubernetes.Interface
	namespace   string
	pm          *PodManager
	idleTimeout time.Duration // 0 = no eviction
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
	for tmpl, idle := range p.pods {
		kept := idle[:0]
		for _, pp := range idle {
			if time.Since(pp.IdleSince) >= p.idleTimeout {
				toDelete = append(toDelete, pp)
			} else {
				kept = append(kept, pp)
			}
		}
		p.pods[tmpl] = kept
	}
	p.mu.Unlock()

	for _, pp := range toDelete {
		slog.Info("pool: evicting idle pod (timeout)", "pod", pp.PodName, "idleSince", pp.IdleSince)
		if err := p.pm.DeletePod(ctx, pp.PodName); err != nil {
			slog.Warn("pool: evict delete failed", "pod", pp.PodName, "error", err)
		}
	}
}

// ClaimPod acquires a Pod corresponding to the given template name.
// Reuses an idle Pod if available; otherwise creates a new one.
func (p *PodPool) ClaimPod(ctx context.Context, runID, templateName string, agentTmpls map[string]AgentPodTemplate, jobTmpl *dsl.PodTemplate, fallbackImage string) (*PooledPod, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	idle := p.pods[templateName]
	if len(idle) > 0 {
		pp := idle[len(idle)-1]
		p.pods[templateName] = idle[:len(idle)-1]

		// Update annotation to in-use (optimistic concurrency control)
		err := p.pm.UpdatePodAnnotations(ctx, pp.PodName, map[string]string{
			annoPoolStatus: poolStatusInUse,
			annoPoolRunID:  runID,
		}, pp.ResourceVersion)
		if err != nil {
			// conflict or fetch failure → fall back to creating a new pod
			slog.Warn("pool: pod claim conflict, creating new pod", "pod", pp.PodName, "error", err)
			return p.createPoolPod(ctx, runID, templateName, agentTmpls, jobTmpl, fallbackImage)
		}
		pod, _ := p.client.CoreV1().Pods(p.namespace).Get(ctx, pp.PodName, metav1.GetOptions{})
		if pod != nil {
			pp.ResourceVersion = pod.ResourceVersion
		}
		return pp, nil
	}

	return p.createPoolPod(ctx, runID, templateName, agentTmpls, jobTmpl, fallbackImage)
}

func (p *PodPool) createPoolPod(ctx context.Context, runID, templateName string, agentTmpls map[string]AgentPodTemplate, jobTmpl *dsl.PodTemplate, fallbackImage string) (*PooledPod, error) {
	pod, err := BuildPod(runID, p.namespace, agentTmpls, jobTmpl, fallbackImage)
	if err != nil {
		return nil, err
	}
	created, err := p.pm.CreatePod(ctx, pod)
	if err != nil {
		return nil, err
	}
	return &PooledPod{
		PodName:         created.Name,
		Template:        templateName,
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
	p.pods[pp.Template] = append(p.pods[pp.Template], &PooledPod{
		PodName:         pp.PodName,
		Template:        pp.Template,
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
		tmpl := pod.Annotations[annoPoolTemplate]
		if tmpl == "" {
			continue
		}
		status := pod.Annotations[annoPoolStatus]
		switch status {
		case poolStatusIdle:
			p.pods[tmpl] = append(p.pods[tmpl], &PooledPod{
				PodName:         pod.Name,
				Template:        tmpl,
				ResourceVersion: pod.ResourceVersion,
				IdleSince:       time.Now(),
			})
			slog.Info("pool: restored idle pod", "pod", pod.Name, "template", tmpl)

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
						p.pods[tmpl] = append(p.pods[tmpl], &PooledPod{
							PodName:         pod.Name,
							Template:        tmpl,
							ResourceVersion: updated.ResourceVersion,
							IdleSince:       time.Now(),
						})
						slog.Info("pool: restored in-use pod as idle (run finished)", "pod", pod.Name, "run", runID)
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
