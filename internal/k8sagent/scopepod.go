package k8sagent

import (
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// scopeMountPath is the fixed mount path for a scope pod's private scratch
// volume, mounted by both the step container and the artifact sidecar.
const scopeMountPath = "/workspace"

// buildScopePod builds a dedicated, isolated pod for a uses-level
// runsIn.image scope: a "step" container running the scope image (kept
// alive with `sleep infinity` so scope steps can be exec'd into it) and,
// when configured, the artifact sidecar — both mounting the SAME private
// `emptyDir` scratch volume named "workspace". Unlike BuildPod, this pod
// intentionally has NO outer-workspace PVC: the scratch volume is scoped to
// this pod only and is discarded when the pod is torn down, isolating the
// scope's filesystem from the run's shared workspace.
func buildScopePod(runID, namespace, scopeID, image string, env map[string]string, sidecar SidecarSpec) *corev1.Pod {
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
		Command:      []string{"sleep", "infinity"},
		Env:          envVars,
		WorkingDir:   scopeMountPath,
		VolumeMounts: []corev1.VolumeMount{scratchMount},
	}}

	if sidecar.Image != "" {
		sc := buildArtifactSidecarContainer(sidecar)
		sc.VolumeMounts = append(sc.VolumeMounts, scratchMount)
		containers = append(containers, sc)
	}

	return &corev1.Pod{
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
}
