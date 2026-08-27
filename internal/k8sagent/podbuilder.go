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

const (
	// SidecarS3SecretModeEnv is the default: the sidecar's S3 Secret reaches
	// it via envFrom.secretRef, exactly as before this seam existed. A Secret
	// consumed this way is snapshotted into the container's environment when
	// the container is created and never updates — rewriting the Secret
	// leaves a running Pod holding the old value.
	SidecarS3SecretModeEnv = "env"

	// SidecarS3SecretModeFile mounts the sidecar's S3 Secret as a volume and
	// points UNIFIED_S3_CREDENTIAL_FILE at it instead of using envFrom. The
	// kubelet updates a mounted Secret in place, which is what makes a
	// rotated or short-lived credential reach a running sidecar at all (see
	// docs/superpowers/specs/2026-08-26-sidecar-credential-delivery-design.md
	// §5.5). Opt-in only — "env" stays the default so no existing deployment
	// changes behaviour.
	SidecarS3SecretModeFile = "file"

	// SidecarS3SecretModeBroker removes the operator-managed Secret
	// entirely: the sidecar container gets a projected ServiceAccount token
	// instead (mounted the same way as any other read-only volume this file
	// builds — no new agent RBAC, since a projected-token volume is minted
	// by the kubelet from the Pod spec the agent already writes, not fetched
	// by the agent from the API server), presents it to the controller's
	// POST /api/v1/store-credentials, and gets object-store credentials
	// back. See docs/superpowers/specs/2026-08-26-sidecar-credential-delivery-design.md
	// §5.6. Opt-in like "file" — "env" stays the default.
	SidecarS3SecretModeBroker = "broker"
)

// sidecarBrokerTokenVolumeName names the projected ServiceAccount token
// volume SidecarS3SecretModeBroker adds. A fixed name is fine for the same
// reason sidecarS3CredentialsVolumeName's is: at most one artifact sidecar
// per Pod, so at most one such volume to collide with.
const sidecarBrokerTokenVolumeName = "sidecar-store-credential-token"

// sidecarBrokerTokenMountPath is where the projected token is mounted into
// the sidecar container ONLY (Global Constraint: never the job container —
// the same container-boundary isolation that keeps today's Secret-derived
// credentials away from user code). sidecarBrokerTokenFile names the file
// within it, matching the "path" the ServiceAccountTokenProjection writes.
const sidecarBrokerTokenMountPath = "/var/run/secrets/unified-cd-store-credentials"
const sidecarBrokerTokenFile = "token"

// sidecarBrokerTokenExpirationSeconds bounds how long the kubelet-projected
// token is valid for. The kubelet keeps the mounted file refreshed well
// before expiry for as long as the Pod runs (the same mechanism that makes
// the enrollment token in manifests/base/k8s-agent/deployment.yaml usable
// for an agent's whole lifetime, not just its first hour), so this is not a
// ceiling on how long a job can run — it only bounds how long a single
// projected credential would remain valid if copied out of the Pod. 3600s
// matches that deployment.yaml's own projected lifetime, for consistency
// rather than inventing a second convention.
const sidecarBrokerTokenExpirationSeconds = int64(3600)

// sidecarS3CredentialsVolumeName names the volume/mount used in
// SidecarS3SecretModeFile. A fixed name is fine: BuildPod and buildScopePod
// each construct at most one artifact sidecar, so there is never more than
// one such volume in a given Pod spec to collide with.
const sidecarS3CredentialsVolumeName = "sidecar-s3-credentials"

// sidecarS3CredentialsSecretKey is the single key SidecarS3SecretModeFile
// expects inside the named Secret: its value must be the
// UNIFIED_S3_KEY=...\nUNIFIED_S3_SECRET=...\n text objectstore.NewFileCredentials
// parses (internal/objectstore/credfile.go). This is a different shape from
// "env" mode's Secret, which instead carries UNIFIED_S3_KEY and
// UNIFIED_S3_SECRET as separate top-level keys for envFrom to expand into
// two env vars — switching modes means re-authoring the Secret, and that
// trade-off is documented in docs/operator-manual/kubernetes-integration.md
// rather than left for an operator to discover from a signature failure.
const sidecarS3CredentialsSecretKey = "credentials"

// sidecarS3CredentialsMountPath is where SidecarS3SecretModeFile mounts the
// single credentials file, and therefore also the value of the
// UNIFIED_S3_CREDENTIAL_FILE env var pointed at it. Fixed, not derived from
// SidecarSpec: there is nothing for an operator to configure here that
// SidecarS3SecretName doesn't already cover.
const sidecarS3CredentialsMountPath = "/etc/unified-cd/s3-credentials"

// SidecarSpec configures the injected artifact-transfer sidecar.
type SidecarSpec struct {
	Image        string
	S3SecretName string // Secret providing S3 credentials for the direct-S3 sidecar

	// S3SecretMode selects how S3SecretName reaches the sidecar — see
	// SidecarS3SecretModeEnv/SidecarS3SecretModeFile/SidecarS3SecretModeBroker.
	// The zero value behaves as SidecarS3SecretModeEnv, matching
	// Config.SidecarS3SecretMode's documented default so a caller that never
	// sets this field (every caller before this seam existed) sees no
	// change.
	S3SecretMode string

	// BrokerURL is the controller base URL the sidecar exchanges its
	// projected token for store credentials against, used only when
	// S3SecretMode is SidecarS3SecretModeBroker (S3SecretName plays no role
	// in that mode — there is no Secret to name). Set from the agent's own
	// Config.Server: the same controller the agent already enrolls against
	// and long-polls for claims, so there is nothing new for an operator to
	// configure beyond opting into the mode.
	BrokerURL string
}

// buildArtifactSidecarContainer constructs the artifact-transfer sidecar
// container from a SidecarSpec, and — only in SidecarS3SecretModeFile — the
// corev1.Volume it needs mounted alongside it (nil otherwise). Shared by
// BuildPod (workspace PVC pods) and buildScopePod (isolated scope pods with a
// private scratch volume); callers are responsible for both attaching
// whatever OTHER volume the sidecar should mount (e.g. workspace) and adding
// the returned volume, if non-nil, to their Pod's Volumes.
func buildArtifactSidecarContainer(sidecar SidecarSpec) (corev1.Container, *corev1.Volume) {
	sc := corev1.Container{
		Name:    artifactSidecarName,
		Image:   sidecar.Image,
		Command: []string{"unified-sidecar", "idle"},
	}

	// Broker mode is checked FIRST and independently of S3SecretName: there
	// is no Secret in this mode at all, so gating on S3SecretName (as the
	// env/file branches below do) would silently skip it whenever an
	// operator opts in without also — incorrectly — setting
	// sidecarS3SecretName, which this mode makes unnecessary.
	if sidecar.S3SecretMode == SidecarS3SecretModeBroker {
		sc.Env = append(sc.Env,
			corev1.EnvVar{Name: "UNIFIED_S3_BROKER_URL", Value: sidecar.BrokerURL},
			corev1.EnvVar{Name: "UNIFIED_S3_BROKER_TOKEN_FILE", Value: sidecarBrokerTokenMountPath + "/" + sidecarBrokerTokenFile},
		)
		sc.VolumeMounts = append(sc.VolumeMounts, corev1.VolumeMount{
			Name:      sidecarBrokerTokenVolumeName,
			MountPath: sidecarBrokerTokenMountPath,
			ReadOnly:  true,
		})
		// A projected ServiceAccountToken volume is minted by the kubelet
		// directly from this Pod spec at admission time — it needs no
		// Kubernetes permission the agent doesn't already have to write a
		// Pod spec at all, unlike a Secret volume (which needs the Secret to
		// already exist and be readable, though notably still not by the
		// agent itself: the kubelet reads it, not the agent — see the "file"
		// branch below). This is the concrete shape of "the agent adds a
		// volume to a Pod spec it already writes" from the design spec's
		// §5.6: no RBAC change, because nothing here calls the Kubernetes
		// API on the agent's behalf.
		vol := &corev1.Volume{
			Name: sidecarBrokerTokenVolumeName,
			VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{
					Sources: []corev1.VolumeProjection{
						{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
							Path: sidecarBrokerTokenFile,
							// A DIFFERENT audience from the agent's own
							// enrollment token (manifests/base/k8s-agent/deployment.yaml
							// uses KubernetesEnrollmentAudience) is the whole
							// safety property this mode depends on: if a job
							// Pod's token were accepted for enrollment, any
							// job could register itself as an agent. See
							// api.KubernetesStoreCredentialAudience's doc
							// comment and internal/controller/api_store_credentials_test.go
							// for the tests asserting the separation in both
							// directions.
							Audience:          api.KubernetesStoreCredentialAudience,
							ExpirationSeconds: ptr.To(sidecarBrokerTokenExpirationSeconds),
						}},
					},
				},
			},
		}
		return sc, vol
	}

	if sidecar.S3SecretName == "" {
		return sc, nil
	}

	if sidecar.S3SecretMode == SidecarS3SecretModeFile {
		// Deliberately NOT envFrom in this mode: carrying both would leave
		// the snapshotted env values as a silent fallback that masks a
		// broken mount, defeating the entire point of switching to a volume.
		sc.Env = append(sc.Env, corev1.EnvVar{
			Name:  "UNIFIED_S3_CREDENTIAL_FILE",
			Value: sidecarS3CredentialsMountPath,
		})
		sc.VolumeMounts = append(sc.VolumeMounts, corev1.VolumeMount{
			Name:      sidecarS3CredentialsVolumeName,
			MountPath: sidecarS3CredentialsMountPath,
			SubPath:   sidecarS3CredentialsSecretKey,
			ReadOnly:  true,
		})
		// 0400 and the deliberate absence of envFrom both mirror the
		// controller's own KEK mount (manifests/base/controller/deployment.yaml),
		// the established pattern in this project for a rotatable credential:
		// a single named key, projected read-only at a fixed subPath.
		//
		// Optional: true is new relative to that pattern, and is the other
		// half of what makes this safe to opt into — without it, a missing or
		// misnamed Secret makes the kubelet fail the WHOLE Pod with
		// CreateContainerConfigError (every job breaks, not just artifact
		// transfer), where the sidecar itself can instead report "no
		// credentials" cleanly.
		vol := &corev1.Volume{
			Name: sidecarS3CredentialsVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: sidecar.S3SecretName,
					Items: []corev1.KeyToPath{
						{Key: sidecarS3CredentialsSecretKey, Path: sidecarS3CredentialsSecretKey},
					},
					Optional:    ptr.To(true),
					DefaultMode: ptr.To(int32(0o400)),
				},
			},
		}
		return sc, vol
	}

	sc.EnvFrom = []corev1.EnvFromSource{
		{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: sidecar.S3SecretName}}},
	}
	return sc, nil
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
		image = defaultPodImage
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
