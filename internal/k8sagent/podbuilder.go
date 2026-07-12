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

// ucdMountPath is the reserved path (documented, see
// docs/superpowers/specs/2026-07-12-step-shell-shim-design.md Component 3)
// the ucd-sh shim shares volume is mounted at in every container of a pod.
// A podTemplate mounting over it is user error — fails loudly at exec.
const ucdMountPath = "/.ucd"

// ucdToolsVolume is the name of the emptyDir volume shared between the
// ucd-shim init container and every other container in the pod, carrying the
// self-installed ucd-sh binary (the Tekton/Argo emissary init-container
// pattern — a pod has no host filesystem to bind-mount from, unlike the host
// agent's claim pod, which bind-mounts its tools dir read-only).
const ucdToolsVolume = "ucd-tools"

// ucdShimContainerName names the init container that installs ucd-sh onto
// ucdToolsVolume before any other container starts.
const ucdShimContainerName = "ucd-shim"

// ucdShimBinary is the path the k8s-agent's own image ships /ucd-sh at (see
// docker/k8s-agent.Dockerfile), used as the init container's own command —
// distinct from ucdMountPath+"/ucd-sh", the path it installs TO on the
// shared volume.
const ucdShimBinary = "/ucd-sh"

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
// shimImage is the image the prepended ucd-shim init container runs (see
// injectUcdShim) — normally cfg.ShimImage (the k8s-agent's own image).
func BuildPod(runID, namespace string, agentTmpls map[string]AgentPodTemplate, jobTmpl *dsl.PodTemplate, fallbackImage string, sidecar SidecarSpec, shimImage string) (*corev1.Pod, error) {
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
	injectKeepAlive(podSpec)

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
	injectUcdShim(podSpec, shimImage)

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
			// Command is intentionally left unset here. injectKeepAlive is
			// the single source of keep-alive truth for the "job" container
			// (see its doc comment) and BuildPod calls it unconditionally
			// right after this function returns; injectKeepAlive only
			// injects ucdKeepAliveArgv() when Command AND Args are both
			// empty. Baking "sleep infinity" (the old behavior) or even
			// ucdKeepAliveArgv() directly into this literal would duplicate
			// that decision in two places and risk them drifting — worse,
			// the old "sleep infinity" literal counted as an explicit
			// Command, so injectKeepAlive's skip-when-set guard silently
			// fired and the bare "podImage, no podTemplate" fallback path
			// (no jobTmpl at all) never got the ucd-sh pause keep-alive,
			// unlike every podTemplate-based path. Leaving Command empty
			// routes this path through the exact same injectKeepAlive logic
			// as a user-supplied podTemplate's "job" container.
			{Name: "job", Image: image},
		},
	}
}

// injectKeepAlive injects the ucd-sh keep-alive (["/.ucd/ucd-sh","pause"])
// into the primary "job" container if it has no command AND no args set, so
// container:-less steps always have a live exec target regardless of whether
// the podTemplate defined "job" explicitly (see defaultPodSpec) or the user
// did (without setting a command). Replaces the previous "sleep infinity"
// (see Component 4 of the step-shell-shim design spec): no `sleep` binary
// requirement, zombie reaping (ucd-sh pause is PID-1-aware), prompt SIGTERM
// exit.
//
// It deliberately does NOT touch any other container: a podTemplate sidecar
// (e.g. mysql, redis) with no command must run its image's own
// entrypoint/CMD — that IS the sidecar's service. Forcing the keep-alive on
// every container (the pre-fix behavior) silently broke every sidecar with
// no explicit command: its entrypoint (e.g. mysqld) never ran, so the
// service was unreachable. This mirrors the host claim pod's fix in
// internal/agent/claim_pod.go (claimPodManager.Start): only the primary
// "job" container gets the keep-alive.
//
// The check also now covers Args, not just Command (the args-clobber fix): a
// "job" container that sets Args only (e.g. relying on the image's own
// ENTRYPOINT, with Args supplying its arguments) previously had that Args
// value silently ignored — the container ran "sleep infinity" instead of the
// author's intended entrypoint invocation, since only len(Command) was
// checked. Skipping injection when EITHER is set respects the author's
// explicit choice either way.
func injectKeepAlive(podSpec *corev1.PodSpec) {
	for i := range podSpec.Containers {
		c := &podSpec.Containers[i]
		if c.Name == primaryContainerName && len(c.Command) == 0 && len(c.Args) == 0 {
			c.Command = ucdKeepAliveArgv()
		}
	}
}

// ucdKeepAliveArgv returns a fresh copy of the keep-alive argv each call, so
// callers can never accidentally alias/mutate a shared backing array.
func ucdKeepAliveArgv() []string {
	return []string{ucdMountPath + "/ucd-sh", "pause"}
}

// injectUcdShim adds the reserved /.ucd shared volume (ucdToolsVolume, an
// emptyDir) and its mount to EVERY container in podSpec — the primary "job"
// container and any podTemplate sidecars, since a sidecar is itself a
// container: exec target and needs the shim just like the primary — and
// prepends an init container that self-installs the shimImage's own /ucd-sh
// binary onto that volume (`/ucd-sh --install /.ucd/ucd-sh`) before any other
// container starts. This is the k8s side of Component 3 of the
// step-shell-shim design spec: the Tekton/Argo emissary "init container
// populates an emptyDir" carrier pattern, since a pod has no host filesystem
// to bind-mount from the way the host agent's claim pod does (read-only bind
// mount of the agent's own tools dir).
func injectUcdShim(podSpec *corev1.PodSpec, shimImage string) {
	podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
		Name:         ucdToolsVolume,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})

	mount := corev1.VolumeMount{Name: ucdToolsVolume, MountPath: ucdMountPath}
	for i := range podSpec.Containers {
		podSpec.Containers[i].VolumeMounts = append(podSpec.Containers[i].VolumeMounts, mount)
	}

	initContainer := corev1.Container{
		Name:         ucdShimContainerName,
		Image:        shimImage,
		Command:      []string{ucdShimBinary, "--install", ucdMountPath + "/ucd-sh"},
		VolumeMounts: []corev1.VolumeMount{mount},
	}
	// Prepend: the shim must be installed before any other init container
	// that might itself need /.ucd, and well before every regular container
	// starts (Kubernetes always runs InitContainers, in order, before any
	// container in podSpec.Containers).
	podSpec.InitContainers = append([]corev1.Container{initContainer}, podSpec.InitContainers...)
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
