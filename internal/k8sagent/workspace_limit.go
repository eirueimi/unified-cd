package k8sagent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/dsl"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
)

// workspaceVolumeName is the emptyDir the workspace lives on (podbuilder.go and
// scopepod.go both name it literally; this file only looks it up by name so it
// does not depend on how BuildPod derived the mount path).
const workspaceVolumeName = "workspace"

// scopeStepContainerName is the primary container of a uses-scope pod.
const scopeStepContainerName = "step"

// applyWorkspaceLimit caps the pod's workspace emptyDir at limit and makes the
// scheduler reserve that much ephemeral storage for the pod, by setting
// emptyDir.sizeLimit on the volume "workspace" and an ephemeral-storage
// *request* (never a limit: a container limit would make the kubelet evict on
// total container ephemeral usage, a different and surprising cap) on the
// primary "job" container.
//
// Anything the pod already says wins: if any container (init containers
// included) already has an ephemeral-storage request or limit, the request is
// not added; if the volume already has a sizeLimit it is kept. A pod without a
// "workspace" emptyDir or a "job" container is not an error; the missing part is
// skipped. It is applied after BuildPod, not inside it (see the disk-gating
// design, section 5).
func applyWorkspaceLimit(pod *corev1.Pod, limit resource.Quantity) {
	applyLimit(pod, limit, primaryContainerName)
}

// applyScopeWorkspaceLimit is applyWorkspaceLimit for a uses-scope pod, whose
// primary container is "step" and whose template is not named (so only the
// global workspaceSizeLimit applies). A no-op when no limit is configured.
func applyScopeWorkspaceLimit(pod *corev1.Pod, cfg Config) {
	if q, ok := resolveWorkspaceSizeLimit(cfg, ""); ok {
		applyLimit(pod, q, scopeStepContainerName)
	}
}

func applyLimit(pod *corev1.Pod, limit resource.Quantity, container string) {
	if pod == nil {
		return
	}
	// An existing ephemeral-storage request or limit anywhere means the
	// operator has already sized this pod: leave the whole pod untouched.
	if podHasEphemeralStorage(pod) {
		return
	}
	// Cap and reservation go together: without an emptyDir workspace (absent,
	// or backed by a PVC) a request would reserve space nothing caps.
	var ws *corev1.EmptyDirVolumeSource
	for i := range pod.Spec.Volumes {
		if v := &pod.Spec.Volumes[i]; v.Name == workspaceVolumeName {
			ws = v.EmptyDir
		}
	}
	if ws == nil || ws.SizeLimit != nil {
		return // no emptyDir workspace, or the pod already sizes it: untouched
	}
	var c *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == container {
			c = &pod.Spec.Containers[i]
		}
	}
	if c == nil {
		return
	}
	q := limit.DeepCopy()
	ws.SizeLimit = &q
	if c.Resources.Requests == nil {
		c.Resources.Requests = corev1.ResourceList{}
	}
	c.Resources.Requests[corev1.ResourceEphemeralStorage] = limit.DeepCopy()
}

func podHasEphemeralStorage(pod *corev1.Pod) bool {
	has := func(cs []corev1.Container) bool {
		for _, c := range cs {
			if _, ok := c.Resources.Requests[corev1.ResourceEphemeralStorage]; ok {
				return true
			}
			if _, ok := c.Resources.Limits[corev1.ResourceEphemeralStorage]; ok {
				return true
			}
		}
		return false
	}
	return has(pod.Spec.Containers) || has(pod.Spec.InitContainers)
}

// resolveWorkspaceSizeLimit returns the workspace size limit for the named agent
// pod template (empty for none): the template value beats the global one. ok is
// false when no limit is configured. A value that does not parse is reported
// false too; Config.Validate rejects it at startup, so this is only a backstop
// and must not silently apply a wrong number.
func resolveWorkspaceSizeLimit(cfg Config, tmplName string) (resource.Quantity, bool) {
	q, ok, err := cfg.WorkspaceSizeLimitFor(tmplName)
	if err != nil {
		slog.Warn("k8s: ignoring invalid workspaceSizeLimit", "template", tmplName, "error", err)
		return resource.Quantity{}, false
	}
	return q, ok
}

// claimTemplateName is the agent pod template name a claim asks for ("" for none).
func claimTemplateName(pt *dsl.PodTemplate) string {
	if pt == nil {
		return ""
	}
	return pt.Name
}

// podLossLookupTimeout bounds the one GetPod explainPodLoss makes.
var podLossLookupTimeout = 5 * time.Second

// explainPodLoss turns an exec infrastructure error into one that says why the
// pod went away, when the kubelet recorded a reason. It does one bounded GetPod
// and, if the pod is Failed with a status reason or message (an eviction for
// exceeding the workspace sizeLimit, or node pressure), returns an error that
// begins with the kubelet text verbatim, followed by a hint naming
// workspaceSizeLimit when a limit is configured and the message is about
// ephemeral storage, then the original error. In every other case (nil err, the
// run context done, lookup failed or timed out, NotFound, pod not terminal)
// err is returned unchanged.
func (b *k8sBackend) explainPodLoss(ctx context.Context, podName string, err error) error {
	if err == nil || ctx.Err() != nil {
		return err
	}
	lctx, cancel := context.WithTimeout(ctx, podLossLookupTimeout)
	defer cancel()
	pod, gerr := b.a.pm.GetPod(lctx, podName)
	if gerr != nil || pod == nil {
		if gerr != nil && !apierrors.IsNotFound(gerr) {
			slog.Debug("k8s: could not look up pod to explain a failed exec", "pod", podName, "error", gerr)
		}
		return err
	}
	if pod.Status.Phase != corev1.PodFailed || (pod.Status.Reason == "" && pod.Status.Message == "") {
		return err
	}

	var sb strings.Builder
	if pod.Status.Reason == "Evicted" {
		fmt.Fprintf(&sb, "k8s: pod %s was evicted: %s", podName, pod.Status.Message)
	} else {
		fmt.Fprintf(&sb, "k8s: pod %s failed (%s): %s", podName, pod.Status.Reason, pod.Status.Message)
	}
	lower := strings.ToLower(pod.Status.Message)
	if limit, ok := resolveWorkspaceSizeLimit(b.a.cfg, b.tmplName); ok &&
		(strings.Contains(lower, "emptydir") || strings.Contains(lower, "ephemeral")) {
		fmt.Fprintf(&sb, "\nThe workspace emptyDir is limited to %s by the agent workspaceSizeLimit; "+
			"use a larger podTemplate (podTemplates.<name>.workspaceSizeLimit), "+
			"or keep large data in a named volume (spec.volumes).", limit.String())
	}
	return fmt.Errorf("%s\n(exec error: %w)", sb.String(), err)
}
