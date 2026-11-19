package k8sagent

import (
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

// sidecarBinary is where docker/artifact-sidecar.Dockerfile installs the
// unified-sidecar binary. The sidecar is started and exec'd by this absolute
// path, never by a PATH lookup: a job-chosen mount path (the workspace, an
// absolute spec.volumes entry) is mounted into the sidecar too, and the policy
// in podmountpaths.go keeps those off the directories this binary lives in.
const sidecarBinary = "/usr/local/bin/unified-sidecar"

const artifactSidecarName = dsl.ArtifactSidecarContainerName

// primaryContainerName is the exec target for container:-less steps —
// mirrors internal/agent.primaryContainerName (the host claim pod's twin
// constant) and internal/k8sagent/executor.go's "" -> "job" fallback.
const primaryContainerName = dsl.PrimaryContainerName

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
const ucdToolsVolume = dsl.UcdToolsVolumeName

// ucdShimContainerName names the init container that installs ucd-sh onto
// ucdToolsVolume before any other container starts.
const ucdShimContainerName = dsl.UcdShimContainerName

// ucdShimBinary is the path the k8s-agent's own image ships /ucd-sh at (see
// docker/k8s-agent.Dockerfile), used as the init container's own command —
// distinct from ucdMountPath+"/ucd-sh", the path it installs TO on the
// shared volume.
const ucdShimBinary = "/ucd-sh"

// sidecarTokenVolumeName names the projected ServiceAccount token volume the
// artifact sidecar gets. A fixed name is fine: at most one artifact sidecar
// per Pod, so at most one such volume to collide with.
const sidecarTokenVolumeName = "sidecar-store-credential-token"

// sidecarTokenMountPath is where the projected token is mounted into the
// sidecar container ONLY (never the job container: the same container
// boundary keeps credentials away from user code). sidecarTokenFile names the
// file within it, matching the "path" the ServiceAccountTokenProjection writes.
const sidecarTokenMountPath = "/var/run/secrets/unified-cd-store-credentials"
const sidecarTokenFile = "token"

// sidecarTokenExpirationSeconds bounds how long the kubelet-projected token is
// valid for. The kubelet keeps the mounted file refreshed well before expiry
// for as long as the Pod runs (the same mechanism that makes the enrollment
// token in charts/unified-cd/templates/agent/deployment.yaml usable for an
// agent's whole lifetime), so this is not a ceiling on how long a job can run;
// it only bounds how long a single projected credential would remain valid if
// copied out of the Pod. 3600s matches that deployment.yaml's own lifetime.
const sidecarTokenExpirationSeconds = int64(3600)

// SidecarSpec configures the injected artifact-transfer sidecar.
type SidecarSpec struct {
	Image string

	// ControllerURL is the controller base URL the sidecar requests presigned
	// object URLs from, authenticated by its projected token. Set from the
	// agent's own Config.Server: the same controller the agent already enrolls
	// against and long-polls for claims. The sidecar holds no store
	// credentials.
	ControllerURL string
}

// buildArtifactSidecarContainer constructs the artifact-transfer sidecar
// container from a SidecarSpec and the projected-token volume it needs mounted
// alongside it. Shared by BuildPod (workspace PVC pods) and buildScopePod
// (isolated scope pods with a private scratch volume); callers are responsible
// for both attaching whatever OTHER volume the sidecar should mount (e.g.
// workspace) and adding the returned volume to their Pod's Volumes.
//
// The token is minted by the kubelet from the Pod spec the agent already
// writes, so it needs no Kubernetes permission beyond writing a Pod. Its
// audience (api.KubernetesStoreCredentialAudience) differs from the agent's
// enrollment token (KubernetesEnrollmentAudience): if a job Pod's token were
// accepted for enrollment, any job could register itself as an agent.
func buildArtifactSidecarContainer(sidecar SidecarSpec) (corev1.Container, *corev1.Volume) {
	sc := corev1.Container{
		Name:    artifactSidecarName,
		Image:   sidecar.Image,
		Command: []string{sidecarBinary, "idle"},
		Env: []corev1.EnvVar{
			{Name: "UNIFIED_CONTROLLER_URL", Value: sidecar.ControllerURL},
			{Name: "UNIFIED_POD_TOKEN_FILE", Value: sidecarTokenMountPath + "/" + sidecarTokenFile},
		},
		VolumeMounts: []corev1.VolumeMount{{
			Name:      sidecarTokenVolumeName,
			MountPath: sidecarTokenMountPath,
			ReadOnly:  true,
		}},
	}
	vol := &corev1.Volume{
		Name: sidecarTokenVolumeName,
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{
					{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
						Path:              sidecarTokenFile,
						Audience:          api.KubernetesStoreCredentialAudience,
						ExpirationSeconds: ptr.To(sidecarTokenExpirationSeconds),
					}},
				},
			},
		},
	}
	return sc, vol
}

// BuildPod constructs a Pod object from the agent template and Job template.
// shimImage is the image the prepended ucd-shim init container runs (see
// injectUcdShim) — normally cfg.ShimImage (the k8s-agent's own image).
//
// vols are the agent-owned named volumes the pod mounts (see volumes.go); nil
// for a pod that mounts none, which is every pooled pod. The pod is labelled
// with each volume it holds, and that label is what BusyVolumes reads.
func BuildPod(runID, namespace string, agentTmpls map[string]AgentPodTemplate, jobTmpl *dsl.PodTemplate, fallbackImage string, sidecar SidecarSpec, shimImage string, vols []PodVolume) (*corev1.Pod, error) {
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
		if jobTmpl.Override != nil {
			if err := applyPatch(podSpec, jobTmpl.Override); err != nil {
				return nil, fmt.Errorf("apply pod spec patch: %w", err)
			}
		}

	case jobTmpl.Spec != nil:
		var err error
		podSpec, err = podSpecFromMap(jobTmpl.Spec)
		if err != nil {
			return nil, fmt.Errorf("parse inline pod spec: %w", err)
		}

	default:
		podSpec = defaultPodSpec(fallbackImage)
	}
	wsCfg := resolveWorkspaceConfig(agentTmpls, jobTmpl)

	// Annotate every reuse pod as in-use at creation time, regardless of
	// which branch above built its spec — a NAMED template pod carries
	// annoPoolTemplate=jobTmpl.Name, while an inline/unnamed template pod
	// carries annoPoolTemplate="" (empty is a valid, expected value: it's
	// how the pool/GC/Restore code tells "reuse pod with no name" apart from
	// "not a pool pod at all", which they do via annoPoolStatus instead).
	// Without this, an inline pooled pod has no pool annotations during its
	// first run, so a GC sweep between the run going terminal and the
	// deferred ReleasePod (which sets it idle) could delete it out from
	// under the run.
	if jobTmpl != nil && jobTmpl.Reuse {
		annotations[annoPoolTemplate] = jobTmpl.Name
		annotations[annoPoolStatus] = poolStatusInUse
	}

	podSpec.RestartPolicy = corev1.RestartPolicyNever
	// A Pod that names no ServiceAccount runs as the namespace's default
	// account, and a job is not entitled to that account's token (podpolicy
	// denies a job-chosen serviceAccountName unless allowServiceAccounts lists
	// it). Default the automount off for such Pods. An explicit value is kept,
	// and a Pod that names a ServiceAccount (an operator's agent template, or
	// an inline spec the policy allowed) keeps Kubernetes' default. The
	// artifact sidecar's store-credential token is an explicit projected
	// volume and is unaffected.
	if podSpec.AutomountServiceAccountToken == nil && podSpec.ServiceAccountName == "" && podSpec.DeprecatedServiceAccount == "" {
		off := false
		podSpec.AutomountServiceAccountToken = &off
	}
	injectPrimaryJob(podSpec, fallbackImage)
	injectKeepAlive(podSpec)

	// Guard against user-supplied containers using the reserved sidecar name
	// or having no name (k8s would otherwise reject the latter only later, at
	// the API server, as an opaque run-creation failure — fail early here to
	// match the host claimContainerDefs check).
	for i, c := range podSpec.Containers {
		if c.Name == "" {
			return nil, fmt.Errorf("podTemplate container at index %d has no name", i)
		}
		if c.Name == artifactSidecarName {
			return nil, fmt.Errorf("container name %q is reserved for the artifact sidecar", artifactSidecarName)
		}
	}
	if sidecar.Image != "" {
		sc, vol := buildArtifactSidecarContainer(sidecar)
		podSpec.Containers = append(podSpec.Containers, sc)
		if vol != nil {
			podSpec.Volumes = append(podSpec.Volumes, *vol)
		}
	}

	if err := injectWorkspace(podSpec, wsCfg); err != nil {
		return nil, err
	}
	// After the workspace: a volume nested inside it has to be mounted second.
	injectVolumes(podSpec, workspaceMountOf(wsCfg), vols)
	for _, v := range vols {
		labels[volumeLabelPrefix+v.Name] = v.Agent
	}
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

// injectPrimaryJob appends the default primary "job" container (image =
// fallbackImage, the agent's podImage) when the resolved PodSpec declares no
// container of that name, so a podTemplate that lists only sidecars (e.g.
// just mysql) still has the exec target container:-less steps need. It is the
// twin of the host agent's claimContainerDefs, which appends the runner image
// as the primary AFTER the declared containers; the same order here keeps the
// declared containers' positions (and so the sidecar ordinals derived from
// dsl.SidecarContainerNames) unchanged. Called before injectKeepAlive,
// injectWorkspace and injectUcdShim so the injected container gets exactly
// what a declared "job" container would.
func injectPrimaryJob(podSpec *corev1.PodSpec, fallbackImage string) {
	for _, c := range podSpec.Containers {
		if c.Name == primaryContainerName {
			return
		}
	}
	if fallbackImage == "" {
		fallbackImage = defaultPodImage()
	}
	podSpec.Containers = append(podSpec.Containers, corev1.Container{Name: primaryContainerName, Image: fallbackImage})
}

func defaultPodSpec(image string) *corev1.PodSpec {
	if image == "" {
		image = defaultPodImage()
	}
	return &corev1.PodSpec{
		Containers: []corev1.Container{
			// Command is intentionally left unset here. injectKeepAlive is
			// the single source of keep-alive truth for the "job" container
			// (see its doc comment) and BuildPod calls it unconditionally
			// right after this function returns; injectKeepAlive
			// unconditionally overwrites the primary "job" container's
			// Command/Args with ucdKeepAliveArgv(), regardless of what's set
			// here. Baking "sleep infinity" (the old behavior) or even
			// ucdKeepAliveArgv() directly into this literal would duplicate
			// that decision in two places and risk them drifting. Leaving
			// Command empty routes this path through the exact same
			// injectKeepAlive logic as a user-supplied podTemplate's "job"
			// container.
			{Name: "job", Image: image},
		},
	}
}

// injectKeepAlive unconditionally forces the primary "job" container's argv
// to the ucd-sh keep-alive (["/.ucd/ucd-sh","pause"]), discarding any
// Command/Args the podTemplate set on it, so container:-less steps always
// have a live exec target regardless of whether the podTemplate defined
// "job" explicitly (see defaultPodSpec) or the user did (with or without
// setting a command/args of their own). Honoring a non-persistent
// user-supplied command would let the container exit and break every later
// step's exec-in — the execution model requires "job" to stay alive as the
// exec target. This matches the host claim pod's behavior in
// internal/agent/claim_pod.go (claimPodManager.Start), which always forces
// the primary's Entrypoint to ucd-sh pause too (host/k8s parity fix #1; see
// docs/superpowers/specs/2026-07-13-host-entrypoint-parity-design.md, "Fix
// #1"). The keep-alive itself replaces the old "sleep infinity" (see
// Component 4 of the step-shell-shim design spec): no `sleep` binary
// requirement, zombie reaping (ucd-sh pause is PID-1-aware), prompt SIGTERM
// exit.
//
// It deliberately does NOT touch any other container: a podTemplate sidecar
// (e.g. mysql, redis) with no command must run its image's own
// entrypoint/CMD — that IS the sidecar's service. Forcing the keep-alive on
// every container (the pre-fix behavior) silently broke every sidecar with
// no explicit command: its entrypoint (e.g. mysqld) never ran, so the
// service was unreachable. A sidecar with its own command still overrides
// its entrypoint, unchanged. Only the primary "job" container is forced to
// keep-alive.
func injectKeepAlive(podSpec *corev1.PodSpec) {
	for i := range podSpec.Containers {
		c := &podSpec.Containers[i]
		if c.Name == primaryContainerName {
			// The primary "job" container is the exec target for
			// container:-less steps, so it ALWAYS runs ucd-sh pause,
			// discarding any command/args the podTemplate set on it —
			// honoring a non-persistent user command would let the
			// container exit and break every later step's exec-in. This
			// matches the host claim pod (claimPodManager.Start forces the
			// primary's Entrypoint to ucd-sh pause). Sidecars are left
			// untouched: a sidecar with no command runs its image's own
			// entrypoint (its service); one with a command overrides it.
			c.Command = ucdKeepAliveArgv()
			c.Args = nil
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

// injectWorkspace injects a workspace volume mount into all containers. For
// the primary "job" container specifically, a non-empty WorkingDir that
// diverges from the workspace mount is rejected: an inline podTemplate would
// have already been caught by dsl's validateStepTargetedWorkingDir at
// apply/parse time, but a NAMED agent template's containers live in agent
// config, invisible at apply time — this is the only point that ever sees
// them, so it is the last chance to catch the same G5 cwd/resolution desync
// (steps always run at the workspace mount: the k8s executor inherits the
// container's WorkingDir, while artifact/cache resolution and
// UNIFIED_WORKSPACE use the mount path). Other containers (sidecars) keep the
// existing preserve-if-set / default-if-empty behavior.
func injectWorkspace(podSpec *corev1.PodSpec, wsCfg *dsl.WorkspaceConfig) error {
	mountPath := workspaceMountOf(wsCfg)

	wsVol := buildWorkspaceVolume()

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
		if podSpec.Containers[i].Name == primaryContainerName {
			if podSpec.Containers[i].WorkingDir != "" && podSpec.Containers[i].WorkingDir != mountPath {
				return fmt.Errorf("podTemplate container %q declares workingDir, but steps execute in it: steps always run at the workspace mount (artifact/cache paths and UNIFIED_WORKSPACE resolve there); move the cd into the step script, or put workingDir on a sidecar", primaryContainerName)
			}
			podSpec.Containers[i].WorkingDir = mountPath
			continue
		}
		// Ensure run steps exec into the workspace mount by default, matching the
		// standard agent's cwd behavior. Don't clobber a WorkingDir a user's
		// template already set explicitly.
		if podSpec.Containers[i].WorkingDir == "" {
			podSpec.Containers[i].WorkingDir = mountPath
		}
	}
	return nil
}

// resolveWorkspaceConfig is the workspace configuration a pod built from
// jobTmpl gets: for a named template the agent template's, overridden by the
// job's; for an inline spec, or a template with neither a name nor a spec (the
// default pod), the job's; none when there is no template.
//
// BuildPod uses it to build the pod, and executeRun uses it for the mount path
// the backend, the volume paths and the workspace wipe all work against. One
// function, so the pod and the agent's idea of where the workspace is cannot
// disagree (a named agent template can set workspace.mountPath, which the job
// alone cannot see).
func resolveWorkspaceConfig(agentTmpls map[string]AgentPodTemplate, jobTmpl *dsl.PodTemplate) *dsl.WorkspaceConfig {
	switch {
	case jobTmpl == nil:
		return nil
	case jobTmpl.Name != "":
		if jobTmpl.Workspace != nil {
			return jobTmpl.Workspace
		}
		return agentTmpls[jobTmpl.Name].Workspace
	default:
		// An inline spec, or neither a name nor a spec (the default pod): the
		// job's own workspace config, so a lone workspace.mountPath is honoured
		// exactly as the host agent honours it.
		return jobTmpl.Workspace
	}
}

// workspaceMountOf is the container path of the workspace a pod built from
// wsCfg mounts: wsCfg.MountPath, else /workspace.
func workspaceMountOf(wsCfg *dsl.WorkspaceConfig) string {
	if wsCfg != nil && wsCfg.MountPath != "" {
		return wsCfg.MountPath
	}
	return "/workspace"
}

// buildWorkspaceVolume is the pod's undeclared workspace: always an emptyDir,
// which dies with the pod. Persistence comes only from a declared named volume
// (a PVC the agent provisions and the job mounts with spec.volumes), never
// from the workspace itself.
func buildWorkspaceVolume() corev1.Volume {
	return corev1.Volume{
		Name:         "workspace",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
}

// toResourceRequirements converts a validated dsl.ResourceSpec to k8s
// ResourceRequirements. Quantities are already validated at apply time, so a
// parse error here is treated defensively (the value is skipped). Used by
// createScopePod (backend.go) to set a uses:-scope pod's "step" container
// resources from runsIn.resources.limits; Requests is always empty in that
// caller since runsIn.resources.requests is rejected at apply time (see
// internal/dsl/parse.go's validateResources), but this function still
// converts Requests when given one — it is a plain, general-purpose
// converter, not the enforcement point.
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
		if rl.CPU != "" {
			if q, err := resource.ParseQuantity(rl.CPU); err == nil {
				out[corev1.ResourceCPU] = q
			}
		}
		if rl.Memory != "" {
			if q, err := resource.ParseQuantity(rl.Memory); err == nil {
				out[corev1.ResourceMemory] = q
			}
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
