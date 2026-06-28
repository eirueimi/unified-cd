package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// AgentConfig holds all configuration for the agent binary.
// Populated from a YAML file via LoadAgent; zero-value fields mean "not set".
type AgentConfig struct {
	Server         string        `yaml:"server"`
	Token          string        `yaml:"token"`
	ID             string        `yaml:"id"`
	Labels         []string      `yaml:"labels"`
	ExposeEnv      []string      `yaml:"exposeEnv"`
	CacheEndpoint  string        `yaml:"cacheEndpoint"`
	CacheKey       string        `yaml:"cacheKey"`
	CacheSecret    string        `yaml:"cacheSecret"`
	CacheBucket    string        `yaml:"cacheBucket"`
	MaxConcurrent  int           `yaml:"maxConcurrent"`
	CleanWorkspace bool          `yaml:"cleanWorkspace"`
	DrainTimeout   time.Duration `yaml:"drainTimeout"`
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
		Server:        os.Getenv("UNIFIED_SERVER"),
		Token:         os.Getenv("UNIFIED_AGENT_TOKEN"),
		ID:            os.Getenv("UNIFIED_AGENT_ID"),
		CacheEndpoint: os.Getenv("UNIFIED_CACHE_ENDPOINT"),
		CacheKey:      os.Getenv("UNIFIED_CACHE_KEY"),
		CacheSecret:   os.Getenv("UNIFIED_CACHE_SECRET"),
		CacheBucket:   os.Getenv("UNIFIED_CACHE_BUCKET"),
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
	if file.Token != "" {
		eff.Token = file.Token
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
	if file.DrainTimeout != 0 {
		eff.DrainTimeout = file.DrainTimeout
	}
	return eff, nil
}

// LabelsString returns Labels as a comma-separated string for use as a flag default.
func (c *AgentConfig) LabelsString() string {
	return strings.Join(c.Labels, ",")
}
