package k8sagent

import (
	"fmt"
	"os"
	"time"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"gopkg.in/yaml.v3"
)

// Config holds the configuration for the Kubernetes agent.
type Config struct {
	Server                  string   `yaml:"server"`
	Token                   string   `yaml:"token"`
	AgentID                 string   `yaml:"agentId"`
	EnrollmentPolicy        string   `yaml:"enrollmentPolicy"`
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

const defaultServiceAccountTokenFile = "/var/run/secrets/unified-cd-agent/token"

// DefaultConfig returns a Config with default values.
func DefaultConfig() Config {
	return Config{
		Namespace:               "default",
		PodImage:                "ghcr.io/eirueimi/unified-cd-runner:v0.0.3",
		SidecarImage:            "ghcr.io/eirueimi/unified-cd-artifact-sidecar:latest",
		ShimImage:               defaultShimImage,
		ServiceAccountTokenFile: defaultServiceAccountTokenFile,
		MaxConcurrent:           100,
	}
}

// LoadConfig loads configuration from configPath. If secretPath is non-empty
// and the file exists, its fields are merged on top (secret values take
// precedence), allowing sensitive fields to be stored in a separate Secret.
func LoadConfig(configPath, secretPath string) (Config, error) {
	cfg := DefaultConfig()
	if err := loadYAML(configPath, &cfg); err != nil {
		return cfg, err
	}
	if secretPath != "" {
		if _, err := os.Stat(secretPath); err == nil {
			if err := loadYAML(secretPath, &cfg); err != nil {
				return cfg, fmt.Errorf("secret file: %w", err)
			}
		}
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
// If UNIFIED_K8S_AGENT_ID is set in the environment, it overrides agentId from the config file,
// allowing each pod in a Deployment to use its own pod name as a unique agent ID.
func (c *Config) Validate() error {
	if v := os.Getenv("UNIFIED_K8S_AGENT_ID"); v != "" {
		c.AgentID = v
	}
	if v := os.Getenv("UNIFIED_K8S_POD_START_TIMEOUT"); v != "" {
		c.PodStartTimeout = v
	}
	if v := os.Getenv("UNIFIED_K8S_DRAIN_TIMEOUT"); v != "" {
		c.DrainTimeout = v
	}
	if c.Server == "" {
		return fmt.Errorf("server is required")
	}
	if c.ServiceAccountTokenFile == "" {
		c.ServiceAccountTokenFile = defaultServiceAccountTokenFile
	}
	if c.Token == "" && c.EnrollmentPolicy == "" {
		return fmt.Errorf("token or enrollmentPolicy is required")
	}
	if c.Token != "" && c.AgentID == "" {
		return fmt.Errorf("agentId is required")
	}
	if c.Namespace == "" {
		c.Namespace = "default"
	}
	if c.PodImage == "" {
		c.PodImage = "ghcr.io/eirueimi/unified-cd-runner:v0.0.3"
	}
	if c.SidecarImage == "" {
		c.SidecarImage = "ghcr.io/eirueimi/unified-cd-artifact-sidecar:latest"
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
