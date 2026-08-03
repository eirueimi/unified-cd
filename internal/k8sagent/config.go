package k8sagent

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"gopkg.in/yaml.v3"
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
	MaxDetachedConcurrent int                         `yaml:"maxDetachedConcurrent"`
	PoolIdleTimeout       string                      `yaml:"poolIdleTimeout,omitempty"`
	PodStartTimeout       string                      `yaml:"podStartTimeout,omitempty"`
	DrainTimeout          string                      `yaml:"drainTimeout,omitempty"`
	PodTemplates          map[string]AgentPodTemplate `yaml:"podTemplates,omitempty"`
	SidecarS3SecretName   string                      `yaml:"sidecarS3SecretName,omitempty"`
}

// AgentPodTemplate is a Pod template defined in the agent configuration file.
type AgentPodTemplate struct {
	Workspace *dsl.WorkspaceConfig `yaml:"workspace,omitempty"`
	Spec      map[string]any       `yaml:"spec"`
}

// defaultShimImage is the default value of Config.ShimImage: the k8s-agent's
// own image, which ships /ucd-sh at its root (see docker/k8s-agent.Dockerfile).
//
// DELIBERATELY NOT DIGEST-PINNED, unlike defaultPodImage and
// defaultSidecarImage below. That asymmetry is recorded here because it looks
// like an oversight and is not — pinning this constant to a digest would be
// strictly worse than leaving it floating, and a future reader "fixing" the
// inconsistency would reintroduce the bug this comment exists to prevent.
//
// The reason is circularity, and it applies to this constant alone.
// defaultPodImage and defaultSidecarImage name OTHER images, with release
// identities independent of this source tree: a digest for them is knowable
// at the commit that records it, and rotating it is a normal edit. This
// constant names THIS binary's own image. Its digest is a function of the
// built image, which contains this constant — so the digest of the image a
// given commit produces cannot be written into that same commit. A digest
// pin here could only ever name the PREVIOUS release, which would permanently
// hard-code the very version skew that docs/operations.md's lockstep
// requirement forbids, instead of merely risking it.
//
// The exposure that pins the two siblings — a mutable tag lets a registry
// compromise execute code in every pod fleet-wide — DOES apply here, and is
// arguably sharper: this image's init container installs the ucd-sh binary
// that every subsequent step in the job container then execs. The exposure is
// accepted at the default and mitigated by configuration, not by pinning:
// operators who need it closed should set `shimImage` explicitly to the
// digest of the k8s-agent image they actually deployed (which they know, and
// which is by construction the lockstep-correct value). See
// docs/kubernetes-integration.md's "Shim image" section.
//
// Making the field REQUIRED was considered and rejected: it would break every
// existing deployment on upgrade to force a value that is mechanically
// derivable from the agent's own image.
//
// The resolution that satisfies both lockstep and immutability is a
// version-derived default (":" + the agent's build version, falling back to
// ":latest" for dev builds), which needs no digest and so has no circularity.
// It is blocked on the agent binary carrying a real version at all: no
// Dockerfile passes -ldflags today, so every containerised agent reports
// "dev". That is tracked as a separate defect; when it is fixed, this
// constant should become version-derived.
//
// NOTE ON WHAT :latest MEANS TODAY: measured 2026-08-03, this tag resolves
// byte-identically to :v0.3.0, because the v0.4.0 release run never applied
// any tag (see the release-docker.yml `verify` job comment). Until the
// operator re-pushes a release tag, a k8s-agent at HEAD running shipped
// defaults injects a v0.3.0 shim.
const defaultShimImage = "ghcr.io/eirueimi/unified-cd-k8s-agent:latest"

// defaultPodImage is the default value of Config.PodImage: the fleet-wide
// primary container image for isolated jobs that don't supply their own
// podTemplate job container. It is digest-pinned — the tag is retained for
// readability, but the digest is what is pulled. A mutable tag would let a
// registry compromise execute code in that container on every such job
// across the fleet. Rotate this together with the runner image release —
// see docs/operations.md#rotating-the-default-runnerpause-image-digests for
// the rotation procedure.
const defaultPodImage = "ghcr.io/eirueimi/unified-cd-runner:v0.0.3@sha256:d7fa1600cf2ec38b78a8893025db7a09cc70b8ac61ae474ceac48444905a729d"

// defaultSidecarImage is the default value of Config.SidecarImage: the
// fleet-wide artifact-transfer sidecar auto-injected into every k8s-agent pod
// (see podbuilder.go's BuildPod/buildArtifactSidecarContainer) — unlike
// PodImage, this is never a job-author-controlled value, so the "job authors
// can already run their own code" carve-out doesn't apply here. It is
// digest-pinned for the same reason as defaultPodImage: a mutable tag would
// let a registry compromise execute code in this sidecar on every k8s-agent
// pod across the fleet, and that sidecar holds long-lived, bucket-scoped
// static S3 credentials (injected via SidecarS3SecretName; see
// cmd/unified-sidecar/main.go and docs/kubernetes-integration.md's threat
// model), making it a credential-exfiltration path, not just a code-exec one.
// The tag is retained for readability, but the digest is what is pulled.
// Rotate this together with the sidecar image release — see
// docs/operations.md#rotating-the-default-runnerpause-image-digests for the
// rotation procedure.
const defaultSidecarImage = "ghcr.io/eirueimi/unified-cd-artifact-sidecar:latest@sha256:5e30d747d7ec954a88d84f4f7a8b5ac5c4b69d152555b80e253e7a0938eb14dd"

const defaultServiceAccountTokenFile = "/var/run/secrets/unified-cd-agent/token"

// DefaultConfig returns a Config with default values.
func DefaultConfig() Config {
	return Config{
		Namespace:               "default",
		PodImage:                defaultPodImage,
		SidecarImage:            defaultSidecarImage,
		ShimImage:               defaultShimImage,
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
	return cfg, nil
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

// Validate validates the configuration values and fills in default values.
func (c *Config) Validate() error {
	if v := os.Getenv("UNIFIED_K8S_POD_START_TIMEOUT"); v != "" {
		c.PodStartTimeout = v
	}
	if v := os.Getenv("UNIFIED_K8S_DRAIN_TIMEOUT"); v != "" {
		c.DrainTimeout = v
	}
	if v := os.Getenv("UNIFIED_K8S_MAX_DETACHED"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.MaxDetachedConcurrent = n
		}
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
		c.PodImage = defaultPodImage
	}
	if c.SidecarImage == "" {
		c.SidecarImage = defaultSidecarImage
	}
	if c.ShimImage == "" {
		c.ShimImage = defaultShimImage
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
