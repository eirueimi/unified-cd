package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultAgentCredentialFile returns the default persistent refresh-credential
// path for an agent whose canonical ID is id:
// $HOME/.unified-cd/<id>/credential.json. It is used when no credential file is
// configured via flag, env, or the config file. The agent ID is part of the
// path so multiple agents sharing a host (and $HOME) never share a credential
// file. When id is empty (the agent's ID has not yet been resolved — e.g.
// --id was omitted and the agent hasn't enrolled/loaded its credential yet),
// it returns the ID-independent shared path $HOME/.unified-cd/credential.json
// instead; running more than one such agent on the same host requires an
// explicit --id or --credential-file to avoid colliding on that shared path.
func DefaultAgentCredentialFile(id string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory for default credential file: %w", err)
	}
	if strings.TrimSpace(id) == "" {
		return filepath.Join(home, ".unified-cd", "credential.json"), nil
	}
	return filepath.Join(home, ".unified-cd", id, "credential.json"), nil
}

// AgentConfig holds all configuration for the agent binary.
// Populated from a YAML file via LoadAgent; zero-value fields mean "not set".
type AgentConfig struct {
	Server              string `yaml:"server"`
	CredentialFile      string `yaml:"credentialFile"`
	EnrollmentTokenFile string `yaml:"enrollmentTokenFile"`
	// EnrollmentToken is a one-time secret and is intentionally not a YAML
	// field: it is populated only from the --enrollment-token flag, the
	// UNIFIED_AGENT_ENROLLMENT_TOKEN env var, or stdin, never from a
	// persisted config file.
	EnrollmentToken string        `yaml:"-"`
	ID              string        `yaml:"id"`
	Labels          []string      `yaml:"labels"`
	ExposeEnv       []string      `yaml:"exposeEnv"`
	CacheEndpoint   string        `yaml:"cacheEndpoint"`
	CacheKey        string        `yaml:"cacheKey"`
	CacheSecret     string        `yaml:"cacheSecret"`
	CacheBucket     string        `yaml:"cacheBucket"`
	MaxConcurrent   int           `yaml:"maxConcurrent"`
	CleanWorkspace  bool          `yaml:"cleanWorkspace"`
	WorkspaceDir    string        `yaml:"workspaceDir"`
	DrainTimeout    time.Duration `yaml:"drainTimeout"`
	PauseImage      string        `yaml:"pauseImage"`
	RunnerImage     string        `yaml:"runnerImage"`

	// MinFreeDisk is the minimum free space (bytes) required on the
	// workspace filesystem for the host agent to keep claiming runs. Zero
	// disables the check. Host-only: k8s agents use pod volumes.
	MinFreeDisk uint64 `yaml:"minFreeDisk"`

	// WorkspaceRetentionDays is the age (in days) after which an inactive
	// per-job workspace directory (working<slot>/<job>) becomes eligible
	// for removal by the opt-in workspace GC. Zero (the default) disables
	// the GC entirely — persistent workspaces are a feature (inter-run
	// cache), so sweeping them must be an explicit opt-in. Host-only.
	WorkspaceRetentionDays int `yaml:"workspaceRetentionDays"`
}

// LoadAgent reads a YAML config file and returns an AgentConfig.
// Unknown fields are rejected.
func LoadAgent(path string) (*AgentConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open agent config: %w", err)
	}
	defer f.Close()

	var cfg AgentConfig
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse agent config %s: %w", path, err)
	}
	return &cfg, nil
}

// AgentEffective resolves the effective configuration from env vars and an
// optional YAML file. The returned struct is used as flag defaults so that
// explicit CLI flags can override both env vars and the config file.
//
// Priority (lowest to highest): env vars → config file → CLI flags.
func AgentEffective(filePath string) (*AgentConfig, error) {
	eff := &AgentConfig{
		Server:              os.Getenv("UNIFIED_SERVER"),
		CredentialFile:      os.Getenv("UNIFIED_AGENT_CREDENTIAL_FILE"),
		EnrollmentTokenFile: os.Getenv("UNIFIED_AGENT_ENROLLMENT_TOKEN_FILE"),
		EnrollmentToken:     os.Getenv("UNIFIED_AGENT_ENROLLMENT_TOKEN"),
		ID:                  os.Getenv("UNIFIED_AGENT_ID"),
		CacheEndpoint:       os.Getenv("UNIFIED_CACHE_ENDPOINT"),
		CacheKey:            os.Getenv("UNIFIED_CACHE_KEY"),
		CacheSecret:         os.Getenv("UNIFIED_CACHE_SECRET"),
		CacheBucket:         os.Getenv("UNIFIED_CACHE_BUCKET"),
		WorkspaceDir:        os.Getenv("UNIFIED_AGENT_WORKSPACE_DIR"),
	}
	if labelsEnv := os.Getenv("UNIFIED_AGENT_LABELS"); labelsEnv != "" {
		for _, l := range strings.Split(labelsEnv, ",") {
			if l = strings.TrimSpace(l); l != "" {
				eff.Labels = append(eff.Labels, l)
			}
		}
	}
	if exposeEnvStr := os.Getenv("UNIFIED_AGENT_EXPOSE_ENV"); exposeEnvStr != "" {
		for _, e := range strings.Split(exposeEnvStr, ",") {
			if e = strings.TrimSpace(e); e != "" {
				eff.ExposeEnv = append(eff.ExposeEnv, e)
			}
		}
	}
	if minFreeDiskEnv := os.Getenv("UNIFIED_AGENT_MIN_FREE_DISK"); minFreeDiskEnv != "" {
		if v, err := strconv.ParseUint(minFreeDiskEnv, 10, 64); err == nil {
			eff.MinFreeDisk = v
		}
	}
	if retentionEnv := os.Getenv("UNIFIED_AGENT_WORKSPACE_RETENTION_DAYS"); retentionEnv != "" {
		if v, err := strconv.Atoi(retentionEnv); err == nil {
			eff.WorkspaceRetentionDays = v
		}
	}

	if filePath == "" {
		return eff, nil
	}

	file, err := LoadAgent(filePath)
	if err != nil {
		return nil, err
	}
	if file.Server != "" {
		eff.Server = file.Server
	}
	if file.CredentialFile != "" {
		eff.CredentialFile = file.CredentialFile
	}
	if file.EnrollmentTokenFile != "" {
		eff.EnrollmentTokenFile = file.EnrollmentTokenFile
	}
	if file.ID != "" {
		eff.ID = file.ID
	}
	if len(file.Labels) > 0 {
		eff.Labels = file.Labels
	}
	if len(file.ExposeEnv) > 0 {
		eff.ExposeEnv = file.ExposeEnv
	}
	if file.CacheEndpoint != "" {
		eff.CacheEndpoint = file.CacheEndpoint
	}
	if file.CacheKey != "" {
		eff.CacheKey = file.CacheKey
	}
	if file.CacheSecret != "" {
		eff.CacheSecret = file.CacheSecret
	}
	if file.CacheBucket != "" {
		eff.CacheBucket = file.CacheBucket
	}
	if file.MaxConcurrent != 0 {
		eff.MaxConcurrent = file.MaxConcurrent
	}
	if file.CleanWorkspace {
		eff.CleanWorkspace = true
	}
	if file.WorkspaceDir != "" {
		eff.WorkspaceDir = file.WorkspaceDir
	}
	if file.DrainTimeout != 0 {
		eff.DrainTimeout = file.DrainTimeout
	}
	if file.PauseImage != "" {
		eff.PauseImage = file.PauseImage
	}
	if file.RunnerImage != "" {
		eff.RunnerImage = file.RunnerImage
	}
	if file.MinFreeDisk != 0 {
		eff.MinFreeDisk = file.MinFreeDisk
	}
	if file.WorkspaceRetentionDays != 0 {
		eff.WorkspaceRetentionDays = file.WorkspaceRetentionDays
	}
	return eff, nil
}

// LabelsString returns Labels as a comma-separated string for use as a flag default.
func (c *AgentConfig) LabelsString() string {
	return strings.Join(c.Labels, ",")
}
