package k8sagent

import (
	"encoding/json"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/eirueimi/unified-cd/internal/dsl"
)

const artifactSidecarName = "unified-artifact"

// SidecarSpec configures the injected artifact-transfer sidecar.
type SidecarSpec struct {
	Image        string
	S3SecretName string // Secret providing UNIFIED_S3_* env for the direct-S3 sidecar
}

// BuildPod constructs a Pod object from the agent template and Job template.
func BuildPod(runID, namespace string, agentTmpls map[string]AgentPodTemplate, jobTmpl *dsl.PodTemplate, fallbackImage string, sidecar SidecarSpec) (*corev1.Pod, error) {
	suffix := runID
	if len(suffix) > 16 {
		suffix = suffix[:16]
	}
	podName := fmt.Sprintf("ucd-run-%s", suffix)

	labels := map[string]string{
		"app":              "unified-cd-agent",
		"unified-cd/runId": runID,
	}
	annotations := map[string]string{}

	var podSpec *corev1.PodSpec
	var wsCfg *dsl.WorkspaceConfig

	switch {
	case jobTmpl == nil:
		podSpec = defaultPodSpec(fallbackImage)

	case jobTmpl.Name != "":
		at, ok := agentTmpls[jobTmpl.Name]
		if !ok {
			return nil, fmt.Errorf("pod template %q not found in agent config", jobTmpl.Name)
		}
		var err error
		podSpec, err = podSpecFromMap(at.Spec)
		if err != nil {
			return nil, fmt.Errorf("parse agent template %q spec: %w", jobTmpl.Name, err)
		}
		wsCfg = at.Workspace
		if jobTmpl.Workspace != nil {
			wsCfg = jobTmpl.Workspace
		}
		if jobTmpl.Override != nil {
			if err := applyPatch(podSpec, jobTmpl.Override); err != nil {
				return nil, fmt.Errorf("apply pod spec patch: %w", err)
			}
		}
		if jobTmpl.Reuse {
			annotations[annoPoolTemplate] = jobTmpl.Name
			annotations[annoPoolStatus] = poolStatusInUse
		}

	case jobTmpl.Spec != nil:
		var err error
		podSpec, err = podSpecFromMap(jobTmpl.Spec)
		if err != nil {
			return nil, fmt.Errorf("parse inline pod spec: %w", err)
		}
		wsCfg = jobTmpl.Workspace

	default:
		podSpec = defaultPodSpec(fallbackImage)
	}

	podSpec.RestartPolicy = corev1.RestartPolicyNever
	injectSleepInfinity(podSpec)

	// Guard against user-supplied containers using the reserved sidecar name.
	for _, c := range podSpec.Containers {
		if c.Name == artifactSidecarName {
			return nil, fmt.Errorf("container name %q is reserved for the artifact sidecar", artifactSidecarName)
		}
	}
	if sidecar.Image != "" {
		sc := corev1.Container{
			Name:    artifactSidecarName,
			Image:   sidecar.Image,
			Command: []string{"unified-sidecar", "idle"},
		}
		if sidecar.S3SecretName != "" {
			sc.EnvFrom = []corev1.EnvFromSource{
				{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: sidecar.S3SecretName}}},
			}
		}
		podSpec.Containers = append(podSpec.Containers, sc)
	}

	injectWorkspace(podSpec, wsCfg)

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        podName,
			Namespace:   namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: *podSpec,
	}, nil
}

func defaultPodSpec(image string) *corev1.PodSpec {
	if image == "" {
		image = "ghcr.io/eirueimi/unified-cd-runner:v0.0.3"
	}
	return &corev1.PodSpec{
		Containers: []corev1.Container{
			{Name: "job", Image: image, Command: []string{"sleep", "infinity"}},
		},
	}
}

// injectSleepInfinity injects "sleep infinity" into containers that have no command set.
// This is necessary to keep template-defined containers alive so the agent can exec into them.
func injectSleepInfinity(podSpec *corev1.PodSpec) {
	for i := range podSpec.Containers {
		if len(podSpec.Containers[i].Command) == 0 {
			podSpec.Containers[i].Command = []string{"sleep", "infinity"}
		}
	}
}

func podSpecFromMap(m map[string]any) (*corev1.PodSpec, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var spec corev1.PodSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, err
	}
	return &spec, nil
}

func applyPatch(spec *corev1.PodSpec, patch *dsl.PodSpecPatch) error {
	if len(patch.Containers) > 0 {
		patchContainers, err := containersFromMaps(patch.Containers)
		if err != nil {
			return fmt.Errorf("parse patch containers: %w", err)
		}
		spec.Containers = mergeContainers(spec.Containers, patchContainers)
	}
	if len(patch.Volumes) > 0 {
		patchVolumes, err := volumesFromMaps(patch.Volumes)
		if err != nil {
			return fmt.Errorf("parse patch volumes: %w", err)
		}
		spec.Volumes = mergeVolumes(spec.Volumes, patchVolumes)
	}
	return nil
}

func mergeContainers(base, patch []corev1.Container) []corev1.Container {
	result := make([]corev1.Container, len(base))
	copy(result, base)
	for _, pc := range patch {
		found := false
		for i, bc := range result {
			if bc.Name == pc.Name {
				result[i] = pc
				found = true
				break
			}
		}
		if !found {
			result = append(result, pc)
		}
	}
	return result
}

func mergeVolumes(base, patch []corev1.Volume) []corev1.Volume {
	result := make([]corev1.Volume, len(base))
	copy(result, base)
	for _, pv := range patch {
		found := false
		for i, bv := range result {
			if bv.Name == pv.Name {
				result[i] = pv
				found = true
				break
			}
		}
		if !found {
			result = append(result, pv)
		}
	}
	return result
}

func containersFromMaps(ms []map[string]any) ([]corev1.Container, error) {
	data, err := json.Marshal(ms)
	if err != nil {
		return nil, err
	}
	var cs []corev1.Container
	return cs, json.Unmarshal(data, &cs)
}

func volumesFromMaps(ms []map[string]any) ([]corev1.Volume, error) {
	data, err := json.Marshal(ms)
	if err != nil {
		return nil, err
	}
	var vs []corev1.Volume
	return vs, json.Unmarshal(data, &vs)
}

// injectWorkspace injects a workspace volume mount into all containers.
func injectWorkspace(podSpec *corev1.PodSpec, wsCfg *dsl.WorkspaceConfig) {
	mountPath := "/workspace"
	if wsCfg != nil && wsCfg.MountPath != "" {
		mountPath = wsCfg.MountPath
	}

	wsVol := buildWorkspaceVolume(wsCfg)

	replaced := false
	for i, v := range podSpec.Volumes {
		if v.Name == "workspace" {
			podSpec.Volumes[i] = wsVol
			replaced = true
			break
		}
	}
	if !replaced {
		podSpec.Volumes = append(podSpec.Volumes, wsVol)
	}

	wsMount := corev1.VolumeMount{Name: "workspace", MountPath: mountPath}
	for i := range podSpec.Containers {
		hasMnt := false
		for _, m := range podSpec.Containers[i].VolumeMounts {
			if m.Name == "workspace" {
				hasMnt = true
				break
			}
		}
		if !hasMnt {
			podSpec.Containers[i].VolumeMounts = append(podSpec.Containers[i].VolumeMounts, wsMount)
		}
		// Ensure run steps exec into the workspace mount by default, matching the
		// standard agent's cwd behavior. Don't clobber a WorkingDir a user's
		// template already set explicitly.
		if podSpec.Containers[i].WorkingDir == "" {
			podSpec.Containers[i].WorkingDir = mountPath
		}
	}
}

func buildWorkspaceVolume(wsCfg *dsl.WorkspaceConfig) corev1.Volume {
	vol := corev1.Volume{Name: "workspace"}
	if wsCfg == nil || wsCfg.PVC == nil {
		vol.VolumeSource = corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
		return vol
	}
	if wsCfg.PVC.ClaimName != "" {
		vol.VolumeSource = corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: wsCfg.PVC.ClaimName,
			},
		}
		return vol
	}
	// Ephemeral PVC
	accessMode := corev1.ReadWriteOnce
	if wsCfg.PVC.AccessMode != "" {
		accessMode = corev1.PersistentVolumeAccessMode(wsCfg.PVC.AccessMode)
	}
	storageReq := resource.MustParse("10Gi")
	if wsCfg.PVC.StorageRequest != "" {
		storageReq = resource.MustParse(wsCfg.PVC.StorageRequest)
	}
	sc := wsCfg.PVC.StorageClassName
	vol.VolumeSource = corev1.VolumeSource{
		Ephemeral: &corev1.EphemeralVolumeSource{
			VolumeClaimTemplate: &corev1.PersistentVolumeClaimTemplate{
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes:      []corev1.PersistentVolumeAccessMode{accessMode},
					StorageClassName: &sc,
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: storageReq,
						},
					},
				},
			},
		},
	}
	return vol
}

// buildImageStepPod builds a throwaway pod that runs a single runsIn.image step
// in isolation: one container from the given image, kept alive with
// `sleep infinity` so the step script can be exec'd into it. No workspace
// volume and no artifact sidecar are attached (inputs arrive via env, output
// via stdout). imagePullSecrets are intentionally NOT set — the pod uses the
// namespace's default ServiceAccount, exactly like BuildPod.
func buildImageStepPod(runID, namespace, image string, env map[string]string, deadlineSeconds int64) *corev1.Pod {
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

	deadline := deadlineSeconds
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("ucd-img-%s-", suffix),
			Namespace:    namespace,
			Labels: map[string]string{
				"app":              "unified-cd-agent",
				"unified-cd/runId": runID,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:         corev1.RestartPolicyNever,
			ActiveDeadlineSeconds: &deadline,
			Containers: []corev1.Container{{
				Name:    "step",
				Image:   image,
				Command: []string{"sleep", "infinity"},
				Env:     envVars,
			}},
		},
	}
}
