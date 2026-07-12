package k8sagent

import (
	"fmt"
	"sort"

	"github.com/eirueimi/unified-cd/internal/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// scopeMountPath is the fixed mount path for a scope pod's private scratch
// volume, mounted by both the step container and the artifact sidecar.
const scopeMountPath = "/workspace"

// scopeKey returns the identity of a scoped step's scope pod within a claim:
// scope pods are keyed by (ScopeID, MatrixKey) so distinct matrix variants of
// the same uses-scope get their own isolated pod, mirroring the host agent's
// scopeManager key. The NUL separator avoids accidental collisions between a
// ScopeID/MatrixKey pair and another pair whose concatenation happens to
// coincide.
func scopeKey(step api.ClaimStep) string { return step.ScopeID + "\x00" + step.MatrixKey }

// buildScopePod builds a dedicated, isolated pod for a uses-level
// runsIn.image scope: a "step" container running the scope image (kept
// alive via the shared ucdKeepAliveArgv() — the injected ucd-sh shim's pause
// subcommand, see injectUcdShim — so scope steps can be exec'd into it) and,
// when configured, the artifact sidecar — both mounting the SAME private
// `emptyDir` scratch volume named "workspace". Unlike BuildPod, this pod
// intentionally has NO outer-workspace PVC: the scratch volume is scoped to
// this pod only and is discarded when the pod is torn down, isolating the
// scope's filesystem from the run's shared workspace.
//
// shimImage is the image the prepended ucd-shim init container runs (see
// injectUcdShim) — normally cfg.ShimImage, threaded through exactly like
// BuildPod's shimImage parameter, so a uses-scope pod gets the same /.ucd
// shim carrier as the run/pooled pod: a uses-scope step is itself an
// exec-target container and needs ucd-sh just like every other one.
func buildScopePod(runID, namespace, scopeID, image string, env map[string]string, sidecar SidecarSpec, shimImage string) *corev1.Pod {
	suffix := runID
	if len(suffix) > 16 {
		suffix = suffix[:16]
	}

	// Deterministic, sorted env for a stable PodSpec (and stable tests).
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	envVars := make([]corev1.EnvVar, 0, len(keys))
	for _, k := range keys {
		envVars = append(envVars, corev1.EnvVar{Name: k, Value: env[k]})
	}

	scratchMount := corev1.VolumeMount{Name: "workspace", MountPath: scopeMountPath}

	containers := []corev1.Container{{
		Name:         "step",
		Image:        image,
		Command:      ucdKeepAliveArgv(),
		Env:          envVars,
		WorkingDir:   scopeMountPath,
		VolumeMounts: []corev1.VolumeMount{scratchMount},
	}}

	if sidecar.Image != "" {
		sc := buildArtifactSidecarContainer(sidecar)
		sc.VolumeMounts = append(sc.VolumeMounts, scratchMount)
		containers = append(containers, sc)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("ucd-scope-%s-", suffix),
			Namespace:    namespace,
			Labels: map[string]string{
				"app":              "unified-cd-agent",
				"unified-cd/runId": runID,
			},
			Annotations: map[string]string{
				"unified-cd/scopeId": scopeID,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    containers,
			Volumes: []corev1.Volume{
				{
					Name:         "workspace",
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
				},
			},
		},
	}

	// Reuse the exact same /.ucd shim carrier pattern BuildPod uses
	// (Component 3 of the step-shell-shim design spec) instead of
	// duplicating its init-container/volume/mount literals here: prepends
	// the ucd-shim init container and mounts the ucd-tools volume on every
	// container in pod.Spec — here, "step" and the scope's own artifact
	// sidecar (when configured).
	injectUcdShim(&pod.Spec, shimImage)

	return pod
}
