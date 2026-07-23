package k8sagent

import (
	"fmt"
	"net"
	"net/url"
	"os"
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
	ShimImage           string                      `yaml:"shimImage"`
	Kubeconfig          string                      `yaml:"kubeconfig"`
	MaxConcurrent       int                         `yaml:"maxConcurrent"`
	PoolIdleTimeout     string                      `yaml:"poolIdleTimeout,omitempty"`
	PodStartTimeout     string                      `yaml:"podStartTimeout,omitempty"`
	DrainTimeout        string                      `yaml:"drainTimeout,omitempty"`
	PodTemplates        map[string]AgentPodTemplate `yaml:"podTemplates,omitempty"`
	SidecarS3SecretName string                      `yaml:"sidecarS3SecretName,omitempty"`
}

// AgentPodTemplate is a Pod template defined in the agent configuration file.
type AgentPodTemplate struct {
	Workspace *dsl.WorkspaceConfig `yaml:"workspace,omitempty"`
	Spec      map[string]any       `yaml:"spec"`
}

// defaultShimImage is the default value of Config.ShimImage: the k8s-agent's
// own image, which ships /ucd-sh at its root (see docker/k8s-agent.Dockerfile).
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
// reach Running before failing the run (see agent.go). Matches the throwaway
// scope-pod bound (imagePodStartTimeout).
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
