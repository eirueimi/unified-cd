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
	Server        string                      `yaml:"server"`
	Token         string                      `yaml:"token"`
	AgentID       string                      `yaml:"agentId"`
	Labels        []string                    `yaml:"labels"`
	Namespace     string                      `yaml:"namespace"`
	PodImage      string                      `yaml:"podImage"`
	SidecarImage  string                      `yaml:"sidecarImage"`
	Kubeconfig    string                      `yaml:"kubeconfig"`
	MaxConcurrent   int                         `yaml:"maxConcurrent"`
	PoolIdleTimeout string                      `yaml:"poolIdleTimeout,omitempty"`
	PodTemplates    map[string]AgentPodTemplate `yaml:"podTemplates,omitempty"`
}

// AgentPodTemplate is a Pod template defined in the agent configuration file.
type AgentPodTemplate struct {
	Workspace *dsl.WorkspaceConfig `yaml:"workspace,omitempty"`
	Spec      map[string]any       `yaml:"spec"`
}

// DefaultConfig returns a Config with default values.
func DefaultConfig() Config {
	return Config{
		Namespace:     "default",
		PodImage:      "ghcr.io/eirueimi/unified-cd-runner:v0.0.3",
		SidecarImage:  "ghcr.io/eirueimi/unified-cd-artifact-sidecar:latest",
		MaxConcurrent: 5,
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

// Validate validates the configuration values and fills in default values.
// If UNIFIED_K8S_AGENT_ID is set in the environment, it overrides agentId from the config file,
// allowing each pod in a Deployment to use its own pod name as a unique agent ID.
func (c *Config) Validate() error {
	if v := os.Getenv("UNIFIED_K8S_AGENT_ID"); v != "" {
		c.AgentID = v
	}
	if c.Server == "" {
		return fmt.Errorf("server is required")
	}
	if c.Token == "" {
		return fmt.Errorf("token is required")
	}
	if c.AgentID == "" {
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
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = 5
	}
	if c.PoolIdleTimeout != "" {
		if _, err := time.ParseDuration(c.PoolIdleTimeout); err != nil {
			return fmt.Errorf("poolIdleTimeout %q: %w", c.PoolIdleTimeout, err)
		}
	}
	return nil
}
