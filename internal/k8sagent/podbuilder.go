package k8sagent

import (
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/eirueimi/unified-cd/internal/dsl"
)

const artifactSidecarName = "unified-artifact"

// primaryContainerName is the exec target for container:-less steps —
// mirrors internal/agent.primaryContainerName (the host claim pod's twin
// constant) and internal/k8sagent/executor.go's "" -> "job" fallback.
const primaryContainerName = "job"

// SidecarSpec configures the injected artifact-transfer sidecar.
type SidecarSpec struct {
	Image        string
	S3SecretName string // Secret providing UNIFIED_S3_* env for the direct-S3 sidecar
}

// buildArtifactSidecarContainer constructs the artifact-transfer sidecar
// container from a SidecarSpec. Shared by BuildPod (workspace PVC pods) and
// buildScopePod (isolated scope pods with a private scratch volume) — callers
// are responsible for attaching whatever volume the sidecar should mount.
func buildArtifactSidecarContainer(sidecar SidecarSpec) corev1.Container {
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
	return sc
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
		podSpec.Containers = append(podSpec.Containers, buildArtifactSidecarContainer(sidecar))
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

// injectSleepInfinity injects "sleep infinity" into the primary "job"
// container if it has no command set, so container:-less steps always have a
// live exec target regardless of whether the podTemplate defined "job"
// explicitly (see defaultPodSpec) or the user did (without setting a
// command).
//
// It deliberately does NOT touch any other container: a podTemplate sidecar
// (e.g. mysql, redis) with no command must run its image's own
// entrypoint/CMD — that IS the sidecar's service. Forcing sleep infinity on
// every container (the previous behavior) silently broke every sidecar with
// no explicit command: its entrypoint (e.g. mysqld) never ran, so the
// service was unreachable. This mirrors the host claim pod's fix in
// internal/agent/claim_pod.go (claimPodManager.Start): only the primary
// "job" container gets the keep-alive.
func injectSleepInfinity(podSpec *corev1.PodSpec) {
	for i := range podSpec.Containers {
		if podSpec.Containers[i].Name == primaryContainerName && len(podSpec.Containers[i].Command) == 0 {
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

// toResourceRequirements converts a validated dsl.ResourceSpec to k8s
// ResourceRequirements. Quantities are already validated at apply time, so a
// parse error here is treated defensively (the value is skipped).
func toResourceRequirements(rs *dsl.ResourceSpec) corev1.ResourceRequirements {
	var req corev1.ResourceRequirements
	if rs == nil {
		return req
	}
	fill := func(rl *dsl.ResourceList) corev1.ResourceList {
		if rl == nil {
			return nil
		}
		out := corev1.ResourceList{}
		if q, err := resource.ParseQuantity(rl.CPU); rl.CPU != "" && err == nil {
			out[corev1.ResourceCPU] = q
		}
		if q, err := resource.ParseQuantity(rl.Memory); rl.Memory != "" && err == nil {
			out[corev1.ResourceMemory] = q
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}
	req.Requests = fill(rs.Requests)
	req.Limits = fill(rs.Limits)
	return req
}
