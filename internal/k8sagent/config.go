package k8sagent

import (
	"bytes"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/config"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Config holds the configuration for the Kubernetes agent.
type Config struct {
	Server string `yaml:"server"`
	// AgentID is not a config input (no yaml tag): it is runtime-populated
	// by the Kubernetes enrollment credential source's AgentID() after
	// bootstrap (see cmd/k8s-agent/main.go's bootstrapAgentClient).
	AgentID                 string
	EnrollmentPolicy        string   `yaml:"enrollmentPolicy"`
	AllowInsecureHTTP       bool     `yaml:"allowInsecureHTTP,omitempty"`
	ServiceAccountTokenFile string   `yaml:"serviceAccountTokenFile"`
	Labels                  []string `yaml:"labels"`
	Namespace               string   `yaml:"namespace"`
	PodImage                string   `yaml:"podImage"`
	SidecarImage            string   `yaml:"sidecarImage"`
	// ShimImage is the image the init container that installs the ucd-sh
	// shim onto the /.ucd emptyDir runs (see podbuilder.go's injectUcdShim
	// and docs/superpowers/specs/2026-07-12-step-shell-shim-design.md
	// Component 3). It defaults to the k8s-agent's own image, which ships
	// /ucd-sh at its root (docker/k8s-agent.Dockerfile) — configurable so
	// air-gapped registries can point it at a mirrored copy.
	ShimImage     string `yaml:"shimImage"`
	Kubeconfig    string `yaml:"kubeconfig"`
	MaxConcurrent int    `yaml:"maxConcurrent"`
	// MaxDetachedConcurrent caps concurrent detached (spec.detached) run claims,
	// separate from MaxConcurrent. 0/unset -> default 16; negative -> off; positive
	// -> cap. Same convention as the host agent, so detached jobs are claimable
	// out of the box.
	MaxDetachedConcurrent int    `yaml:"maxDetachedConcurrent"`
	PoolIdleTimeout       string `yaml:"poolIdleTimeout,omitempty"`
	PodStartTimeout       string `yaml:"podStartTimeout,omitempty"`
	DrainTimeout          string `yaml:"drainTimeout,omitempty"`
	// FinallyTimeout bounds each of a run's post-DAG cleanup phases: the
	// `spec.finally` pipeline, and each of the two `post:`/`cache:` hook
	// drains. Those phases deliberately ignore run cancellation, which also
	// discards the job-level `spec.timeoutMinutes` deadline — this is the
	// ceiling that replaces it. Unset, unparseable, or non-positive falls back
	// to agentlib.DefaultFinallyBudget (10m). Env override:
	// UNIFIED_K8S_FINALLY_TIMEOUT. The setting is identical on the host agent
	// (`finallyTimeout` there too) because the phase it bounds is executed by
	// the SHARED orchestration loop, not by either backend.
	FinallyTimeout      string                      `yaml:"finallyTimeout,omitempty"`
	PodTemplates        map[string]AgentPodTemplate `yaml:"podTemplates,omitempty"`
	SidecarS3SecretName string                      `yaml:"sidecarS3SecretName,omitempty"`

	// SidecarS3SecretMode selects how the Secret named by SidecarS3SecretName
	// reaches the sidecar.
	//
	//   "env"  (default) envFrom.secretRef, as today.
	//   "file" mounted as a volume, with UNIFIED_S3_CREDENTIAL_FILE pointing at it.
	//
	// "file" is what makes a rotated or short-lived credential possible: an
	// envFrom Secret is snapshotted into the container's environment at creation
	// and never updates, while a mounted Secret is updated by the kubelet.
	// It also lets the volume be optional, so a missing Secret degrades instead
	// of failing the whole Pod. Env: UNIFIED_K8S_SIDECAR_S3_SECRET_MODE.
	SidecarS3SecretMode string `yaml:"sidecarS3SecretMode,omitempty"`

	// AgentName is the stable name this agent's volume claims are prefixed
	// with: <agentName>-<volume>. Required when Volumes is non-empty, and it
	// must not change while the claims hold data.
	//
	// It is deliberately NOT AgentID. The enrolled ID is
	// "k8s:<cluster>:<namespace>:<pod UID>", which carries characters a claim
	// name cannot and changes with every pod, so a prefix derived from it
	// would provision a fresh empty PVC on each restart and leave the last one
	// behind. Replicas of one Deployment share this config and so share one set
	// of claims; that is why occupancy is read from the live pods (BusyVolumes)
	// and not from any one process. Two agents in one namespace need two names.
	AgentName string `yaml:"agentName,omitempty"`
	// Volumes declares the agent-owned named volumes this agent advertises and
	// jobs may mount (spec.volumes). Each becomes a PVC of the given size, which
	// is required here; path is meaningless on Kubernetes and is ignored.
	Volumes []config.VolumeConfig `yaml:"volumes,omitempty"`

	// MinFreeDisk is the free-space headroom a named volume must have before
	// this agent claims runs that mount it (percent of the volume, a size, a
	// byte count, or off). Unset means 1%; an explicit 0/"off" disables the
	// gate. Validate resolves the default into this field, and
	// EffectiveMinFreeDisk applies it for a Config that was never validated, so
	// no construction path can end up with a silent "off". A malformed value is
	// a Validate/load error. Env: UNIFIED_K8S_MIN_FREE_DISK.
	MinFreeDisk config.DiskThreshold `yaml:"minFreeDisk"`
	// WorkspaceSizeLimit caps the workspace emptyDir (a resource.Quantity such
	// as "20Gi"). Empty means no cap: deliberately unset by default, because a
	// default would evict builds that legitimately write more. Per-template
	// overrides: AgentPodTemplate.WorkspaceSizeLimit. Env:
	// UNIFIED_K8S_WORKSPACE_SIZE_LIMIT.
	WorkspaceSizeLimit string `yaml:"workspaceSizeLimit,omitempty"`
	// JobPodPolicy lists the Pod fields a job's own podTemplate content may
	// use beyond the always-allowed set; the zero value allows none of the
	// gated categories. File-only (no environment variable). See podpolicy.go.
	JobPodPolicy JobPodPolicy `yaml:"jobPodPolicy,omitempty"`
}

// AgentPodTemplate is a Pod template defined in the agent configuration file.
type AgentPodTemplate struct {
	Workspace *dsl.WorkspaceConfig `yaml:"workspace,omitempty"`
	Spec      map[string]any       `yaml:"spec"`
	// WorkspaceSizeLimit overrides Config.WorkspaceSizeLimit for runs that use
	// this template. Empty inherits the global value.
	WorkspaceSizeLimit string `yaml:"workspaceSizeLimit,omitempty"`
}

// defaultShimImage is the default value of Config.ShimImage: the k8s-agent's
// own image, which ships /ucd-sh at its root (see docker/k8s-agent.Dockerfile).
//
// It is derived from the agent's stamped version (agentlib.VersionedImage), so
// the shim the agent injects is, by construction, the one published with that
// agent's release; a build with no release version falls back to :latest.
//
// DELIBERATELY NOT DIGEST-PINNED, and it must not be re-pinned. That is
// recorded here because a digest looks like the safer choice and is
// strictly worse than a version-derived tag. The reason is circularity. This
// constant names THIS binary's own image. Its digest is a function of the
// built image, which contains this constant -- so the digest of the image a
// given commit produces cannot be written into that same commit. A digest
// pin here could only ever name the PREVIOUS release, which would permanently
// hard-code the very version skew that docs/operator-manual/operations.md's
// lockstep requirement forbids, instead of merely risking it. (defaultPodImage
// and defaultSidecarImage name OTHER images and were once digest-pinned for
// the same supply-chain reason, but a digest can be recorded only after its
// image is pushed, so those pins also always named an older release. All
// three now share the version-derived rule.)
//
// The exposure a pin would have closed -- a mutable tag lets a registry
// compromise execute code in every pod fleet-wide -- DOES apply here, and is
// arguably sharper: this image's init container installs the ucd-sh binary
// that every subsequent step in the job container then execs. A release tag
// is still mutable -- a registry lets anyone with push access move it -- and is
// merely not expected to move; the exposure is accepted at the
// default and mitigated by configuration, not by pinning: operators who need
// it closed should set `shimImage` explicitly to the digest of the k8s-agent
// image they actually deployed (which they know, and which is by construction
// the lockstep-correct value). See docs/operator-manual/kubernetes-integration.md's
// "Shim image" section.
//
// Making the field REQUIRED was considered and rejected: it would break every
// existing deployment on upgrade to force a value that is mechanically
// derivable from the agent's own image.
func defaultShimImage() string {
	return agentlib.VersionedImage("ghcr.io/eirueimi/unified-cd-k8s-agent", agentlib.Version)
}

// defaultPodImage is the default value of Config.PodImage: the fleet-wide
// primary container image for isolated jobs that don't supply their own
// podTemplate job container. It is the same image, by the same version-derived
// rule, as the host agent's default runner image: both delegate to
// agent.DefaultRunnerImage so the repository string exists in one place.
//
// It is not digest-pinned: a digest of an image built from this repository
// can only be recorded after the image is pushed, so a pin could only ever
// name the PREVIOUS release's runner. Operators who need an immutable
// reference (a mutable tag lets a registry compromise execute code in this
// container on every such job) set podImage explicitly to a digest.
func defaultPodImage() string {
	return agentlib.DefaultRunnerImage()
}

// defaultSidecarImage is the default value of Config.SidecarImage: the
// fleet-wide artifact-transfer sidecar auto-injected into every k8s-agent pod
// (see podbuilder.go's BuildPod/buildArtifactSidecarContainer) -- unlike
// PodImage, this is never a job-author-controlled value, so the "job authors
// can already run their own code" carve-out doesn't apply here. That sidecar
// holds long-lived, bucket-scoped static S3 credentials (injected via
// SidecarS3SecretName; see cmd/unified-sidecar/main.go and
// docs/operator-manual/kubernetes-integration.md's threat model), making a
// registry compromise of it a credential-exfiltration path, not just a
// code-exec one.
//
// It is derived from the agent's stamped version like the other defaults, and
// for the same reason is not digest-pinned: a digest can be recorded only
// after the image is pushed, so a pin could only name the PREVIOUS release's
// sidecar. Operators who need the exposure closed set sidecarImage explicitly
// to a digest.
func defaultSidecarImage() string {
	return agentlib.VersionedImage("ghcr.io/eirueimi/unified-cd-artifact-sidecar", agentlib.Version)
}

const defaultServiceAccountTokenFile = "/var/run/secrets/unified-cd-agent/token"

// DefaultConfig returns a Config with default values.
func DefaultConfig() Config {
	return Config{
		Namespace:               "default",
		PodImage:                defaultPodImage(),
		SidecarImage:            defaultSidecarImage(),
		ShimImage:               defaultShimImage(),
		ServiceAccountTokenFile: defaultServiceAccountTokenFile,
		MaxConcurrent:           100,
	}
}

// LoadConfig loads configuration from configPath.
func LoadConfig(configPath string) (Config, error) {
	cfg := DefaultConfig()
	if err := loadYAML(configPath, &cfg); err != nil {
		return cfg, err
	}
	if err := rejectRemovedWorkspacePVC(configPath); err != nil {
		return cfg, err
	}
	if err := rejectUnknownJobPodPolicyKeys(configPath); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// rejectUnknownJobPodPolicyKeys strictly decodes the jobPodPolicy block.
// loadYAML is lenient, and a misspelled opt-in would otherwise vanish silently.
func rejectUnknownJobPodPolicyKeys(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var raw struct {
		JobPodPolicy yaml.Node `yaml:"jobPodPolicy"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if raw.JobPodPolicy.Kind == 0 {
		return nil
	}
	block, err := yaml.Marshal(&raw.JobPodPolicy)
	if err != nil {
		return fmt.Errorf("%s: jobPodPolicy: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(block))
	dec.KnownFields(true)
	var p JobPodPolicy
	if err := dec.Decode(&p); err != nil {
		return fmt.Errorf("%s: jobPodPolicy: %w", path, err)
	}
	return nil
}

// rejectRemovedWorkspacePVC fails when an agent podTemplate still carries the
// removed workspace.pvc block. loadYAML is lenient about unknown keys, so
// without this the block would be dropped silently and a persistent workspace
// would quietly become an ephemeral one.
func rejectRemovedWorkspacePVC(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var raw struct {
		PodTemplates map[string]any `yaml:"podTemplates"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	names := make([]string, 0, len(raw.PodTemplates))
	for name := range raw.PodTemplates {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := dsl.CheckRemovedWorkspacePVC(raw.PodTemplates[name], fmt.Sprintf("podTemplates[%q]", name)); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return nil
}

func loadYAML(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, out); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

// PoolIdleTimeoutDuration parses PoolIdleTimeout and returns its value, or 0 if unset/invalid.
func (c *Config) PoolIdleTimeoutDuration() time.Duration {
	if c.PoolIdleTimeout == "" {
		return 0
	}
	d, err := time.ParseDuration(c.PoolIdleTimeout)
	if err != nil {
		return 0
	}
	return d
}

// defaultPodStartTimeout bounds how long executeRun waits for a run Pod to
// reach Running before failing the run (see agent.go's awaitPodRunning). The
// same PodStartTimeoutDuration also bounds the throwaway uses-scope pod's
// Ready wait (see backend.go's ensureScopePod) — one configurable knob for
// both pod-start waits.
const defaultPodStartTimeout = 5 * time.Minute

// PodStartTimeoutDuration parses PodStartTimeout, returning defaultPodStartTimeout
// when unset, unparseable, or non-positive.
func (c *Config) PodStartTimeoutDuration() time.Duration {
	if c.PodStartTimeout == "" {
		return defaultPodStartTimeout
	}
	d, err := time.ParseDuration(c.PodStartTimeout)
	if err != nil || d <= 0 {
		return defaultPodStartTimeout
	}
	return d
}

// FinallyTimeoutDuration parses FinallyTimeout, returning
// agentlib.DefaultFinallyBudget when unset or non-positive. Non-positive is
// deliberately not "unbounded": an unbounded cleanup phase is the bug this
// setting exists to close (see agentlib.DefaultFinallyBudget).
//
// An UNPARSEABLE value never reaches this fallback, whatever the parse error
// branch below suggests: Config.Validate rejects it and cmd/k8s-agent exits 1
// before any claim runs, so the agent never starts with a silently-defaulted
// budget. The branch is kept only so this method is safe to call on a Config
// that was never validated (tests, future callers) — do not document it as
// "unparseable falls back to 10m", because for the binary it does not.
// (PodStartTimeoutDuration and DrainTimeoutDuration above are in the same
// position; this one is called out because a bad `finallyTimeout` is
// something an operator can hit during a cutover.)
func (c *Config) FinallyTimeoutDuration() time.Duration {
	if c.FinallyTimeout == "" {
		return agentlib.DefaultFinallyBudget
	}
	d, err := time.ParseDuration(c.FinallyTimeout)
	if err != nil || d <= 0 {
		return agentlib.DefaultFinallyBudget
	}
	return d
}

// DrainTimeoutDuration parses DrainTimeout, returning 0 (wait indefinitely)
// when unset or unparseable.
func (c *Config) DrainTimeoutDuration() time.Duration {
	if c.DrainTimeout == "" {
		return 0
	}
	d, err := time.ParseDuration(c.DrainTimeout)
	if err != nil {
		return 0
	}
	return d
}

// EffectiveMinFreeDisk returns MinFreeDisk with the 1% default applied when it
// was never configured.
func (c *Config) EffectiveMinFreeDisk() config.DiskThreshold {
	return c.MinFreeDisk.OrDefault()
}

// WorkspaceSizeLimitUnset reports that no global workspaceSizeLimit is set.
func (c *Config) WorkspaceSizeLimitUnset() bool { return c.WorkspaceSizeLimit == "" }

// WorkspaceSizeLimitFor returns the workspace size limit for the named pod
// template: the template's own value if set, else the global one. ok is false
// when neither is set.
func (c *Config) WorkspaceSizeLimitFor(template string) (resource.Quantity, bool, error) {
	raw := c.WorkspaceSizeLimit
	field := "workspaceSizeLimit"
	if t, ok := c.PodTemplates[template]; ok && t.WorkspaceSizeLimit != "" {
		raw = t.WorkspaceSizeLimit
		field = fmt.Sprintf("podTemplates[%q].workspaceSizeLimit", template)
	}
	if raw == "" {
		return resource.Quantity{}, false, nil
	}
	q, err := parseWorkspaceSizeLimit(field, raw)
	if err != nil {
		return resource.Quantity{}, false, err
	}
	return q, true, nil
}

// parseWorkspaceSizeLimit parses a workspaceSizeLimit value as a Kubernetes
// Quantity and requires it to be positive. field names the setting in errors.
func parseWorkspaceSizeLimit(field, raw string) (resource.Quantity, error) {
	q, err := resource.ParseQuantity(raw)
	if err != nil {
		return resource.Quantity{}, fmt.Errorf("%s %q: %w", field, raw, err)
	}
	if q.Sign() <= 0 {
		return resource.Quantity{}, fmt.Errorf("%s %q: must be greater than 0 (leave it unset for no limit)", field, raw)
	}
	return q, nil
}

// Validate validates the configuration values and fills in default values.
func (c *Config) Validate() error {
	if v := os.Getenv("UNIFIED_K8S_POD_START_TIMEOUT"); v != "" {
		c.PodStartTimeout = v
	}
	if v := os.Getenv("UNIFIED_K8S_DRAIN_TIMEOUT"); v != "" {
		c.DrainTimeout = v
	}
	if v := os.Getenv("UNIFIED_K8S_FINALLY_TIMEOUT"); v != "" {
		c.FinallyTimeout = v
	}
	if v := os.Getenv("UNIFIED_K8S_MAX_DETACHED"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.MaxDetachedConcurrent = n
		}
	}
	if v := os.Getenv("UNIFIED_K8S_SIDECAR_S3_SECRET_MODE"); v != "" {
		c.SidecarS3SecretMode = v
	}
	if v := os.Getenv("UNIFIED_K8S_MIN_FREE_DISK"); v != "" {
		// A malformed value is a refused start, never a silent fallback to
		// the default: an operator who typed "10GB " expecting a gate must
		// not get a different one.
		t, err := config.ParseDiskThreshold(v)
		if err != nil {
			return fmt.Errorf("UNIFIED_K8S_MIN_FREE_DISK: %w", err)
		}
		c.MinFreeDisk = t
	}
	if v := os.Getenv("UNIFIED_K8S_WORKSPACE_SIZE_LIMIT"); v != "" {
		c.WorkspaceSizeLimit = v
	}
	if c.Server == "" {
		return fmt.Errorf("server is required")
	}
	if c.EnrollmentPolicy != "" {
		if err := validateKubernetesEnrollmentServer(c.Server, c.AllowInsecureHTTP); err != nil {
			return err
		}
	}
	if c.ServiceAccountTokenFile == "" {
		c.ServiceAccountTokenFile = defaultServiceAccountTokenFile
	}
	if c.EnrollmentPolicy == "" {
		return fmt.Errorf("enrollmentPolicy is required")
	}
	if c.Namespace == "" {
		c.Namespace = "default"
	}
	if c.PodImage == "" {
		c.PodImage = defaultPodImage()
	}
	if c.SidecarImage == "" {
		c.SidecarImage = defaultSidecarImage()
	}
	if c.ShimImage == "" {
		c.ShimImage = defaultShimImage()
	}
	if c.SidecarS3SecretMode == "" {
		c.SidecarS3SecretMode = SidecarS3SecretModeEnv
	}
	if c.SidecarS3SecretMode != SidecarS3SecretModeEnv && c.SidecarS3SecretMode != SidecarS3SecretModeFile && c.SidecarS3SecretMode != SidecarS3SecretModeBroker {
		// An unrecognised value must fail at startup, not silently fall back
		// to "env" — a typo that goes unnoticed here would look identical to
		// "file"/"broker" mode simply not doing anything, which is a much
		// harder bug to track down than a refused start.
		return fmt.Errorf("sidecarS3SecretMode %q: must be %q, %q, or %q", c.SidecarS3SecretMode, SidecarS3SecretModeEnv, SidecarS3SecretModeFile, SidecarS3SecretModeBroker)
	}
	// maxConcurrent: 0/unset -> default 100; negative -> unlimited (preserved
	// as a sentinel; the run loop skips its semaphore); positive -> that bound.
	if c.MaxConcurrent == 0 {
		c.MaxConcurrent = 100
	}
	if c.PoolIdleTimeout != "" {
		if _, err := time.ParseDuration(c.PoolIdleTimeout); err != nil {
			return fmt.Errorf("poolIdleTimeout %q: %w", c.PoolIdleTimeout, err)
		}
	}
	if c.PodStartTimeout != "" {
		if _, err := time.ParseDuration(c.PodStartTimeout); err != nil {
			return fmt.Errorf("podStartTimeout %q: %w", c.PodStartTimeout, err)
		}
	}
	if c.DrainTimeout != "" {
		if _, err := time.ParseDuration(c.DrainTimeout); err != nil {
			return fmt.Errorf("drainTimeout %q: %w", c.DrainTimeout, err)
		}
	}
	if c.FinallyTimeout != "" {
		if _, err := time.ParseDuration(c.FinallyTimeout); err != nil {
			return fmt.Errorf("finallyTimeout %q: %w", c.FinallyTimeout, err)
		}
	}
	// Resolve the 1% default here so every path that validates a Config
	// (and every reader via EffectiveMinFreeDisk) sees the same value.
	c.MinFreeDisk = c.MinFreeDisk.OrDefault()
	if c.WorkspaceSizeLimit != "" {
		if _, err := parseWorkspaceSizeLimit("workspaceSizeLimit", c.WorkspaceSizeLimit); err != nil {
			return err
		}
	}
	tmplNames := make([]string, 0, len(c.PodTemplates))
	for name := range c.PodTemplates {
		tmplNames = append(tmplNames, name)
	}
	sort.Strings(tmplNames)
	for _, name := range tmplNames {
		if v := c.PodTemplates[name].WorkspaceSizeLimit; v != "" {
			if _, err := parseWorkspaceSizeLimit(fmt.Sprintf("podTemplates[%q].workspaceSizeLimit", name), v); err != nil {
				return err
			}
		}
	}
	// A bad volume declaration is a refused start, not a run that fails later:
	// the agent would otherwise advertise a volume it cannot provision.
	if err := validateVolumeConfigs(c.AgentName, c.Volumes); err != nil {
		return err
	}
	if err := c.JobPodPolicy.validate(c.SidecarS3SecretName, c.AgentName, c.Volumes); err != nil {
		return fmt.Errorf("jobPodPolicy: %w", err)
	}
	return nil
}

func validateKubernetesEnrollmentServer(server string, allowInsecureHTTP bool) error {
	u, err := url.Parse(server)
	if err != nil || u.Scheme == "" || u.Hostname() == "" {
		return fmt.Errorf("server must be an absolute URL")
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && (allowInsecureHTTP || isLoopbackHost(u.Hostname())) {
		return nil
	}
	return fmt.Errorf("server must use https for Kubernetes enrollment (http is allowed only for loopback local development)")
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
