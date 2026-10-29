package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/agentid"
	"gopkg.in/yaml.v3"
)

// DefaultAgentCredentialFile returns the default persistent refresh-credential
// path for an agent whose canonical ID is id:
// $HOME/.unified-cd/<id>/credential.json. It is used when no credential file is
// configured via flag, env, or the config file. The agent ID is part of the
// path so multiple agents sharing a host (and $HOME) never share a credential
// file. An ID is required to derive this path.
func DefaultAgentCredentialFile(id string) (string, error) {
	if strings.TrimSpace(id) == "" {
		return "", fmt.Errorf("agent ID is required to derive the default credential file path")
	}
	if err := agentid.ValidatePortable(id); err != nil {
		return "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory for default credential file: %w", err)
	}
	return filepath.Join(home, ".unified-cd", id, "credential.json"), nil
}

// DiscoverDefaultAgentCredentialFile finds the only ID-scoped default
// credential file, if one exists.
func DiscoverDefaultAgentCredentialFile() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory for default credential discovery: %w", err)
	}
	return discoverAgentCredentialFile(filepath.Join(home, ".unified-cd"))
}

func discoverAgentCredentialFile(root string) (string, error) {
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read default agent credential directory: %w", err)
	}
	var candidates []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(root, entry.Name(), "credential.json")
		if _, err := os.Stat(path); err == nil {
			if err := agentid.ValidatePortable(entry.Name()); err != nil {
				return "", err
			}
			candidates = append(candidates, path)
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("inspect default agent credential file: %w", err)
		}
	}
	switch len(candidates) {
	case 0:
		return "", nil
	case 1:
		return candidates[0], nil
	default:
		return "", fmt.Errorf("multiple default agent credential files found; set --id or --credential-file")
	}
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
	EnrollmentToken string   `yaml:"-"`
	ID              string   `yaml:"id"`
	Labels          []string `yaml:"labels"`
	ExposeEnv       []string `yaml:"exposeEnv"`
	CacheEndpoint   string   `yaml:"cacheEndpoint"`
	CacheKey        string   `yaml:"cacheKey"`
	CacheSecret     string   `yaml:"cacheSecret"`
	CacheBucket     string   `yaml:"cacheBucket"`
	MaxConcurrent   int      `yaml:"maxConcurrent"`
	// MaxDetachedConcurrent caps how many detached (spec.detached) runs this
	// agent claims concurrently, from a pool separate from MaxConcurrent.
	// 0 / unset -> the agent applies the default (16), so detached jobs are
	// claimable out of the box; a negative value disables detached hosting on
	// this agent; a positive value is the cap.
	MaxDetachedConcurrent int           `yaml:"maxDetachedConcurrent"`
	CleanWorkspace        bool          `yaml:"cleanWorkspace"`
	WorkspaceDir          string        `yaml:"workspaceDir"`
	DrainTimeout          time.Duration `yaml:"drainTimeout"`

	// FinallyTimeout bounds each of a run's post-DAG cleanup phases: the
	// `spec.finally` pipeline, and each of the two `post:`/`cache:` hook
	// drains. Those phases deliberately ignore run cancellation, which also
	// discards the job-level `spec.timeoutMinutes` deadline — this is the
	// ceiling that replaces it, so a wedged cleanup step cannot pin the agent
	// slot forever. Unset or non-positive falls back to
	// agent.DefaultFinallyBudget (10m). Env: UNIFIED_AGENT_FINALLY_TIMEOUT.
	FinallyTimeout time.Duration `yaml:"finallyTimeout"`

	PauseImage  string `yaml:"pauseImage"`
	RunnerImage string `yaml:"runnerImage"`

	// MinFreeDisk is the minimum free space required on the workspace
	// filesystem for the host agent to keep claiming runs: a percentage
	// ("1%"), a size ("10GiB"), a byte count, or 0/"off" to disable. Unset
	// means the default (see DefaultDiskThreshold). Host-only: k8s agents use
	// pod volumes.
	MinFreeDisk DiskThreshold `yaml:"minFreeDisk"`

	// WorkspaceRetentionDays is the age (in days) after which an inactive
	// per-job workspace directory (working<slot>/<job>) becomes eligible
	// for removal by the opt-in workspace GC. Zero (the default) disables
	// the GC entirely. A workspace is emptied before and after every run, so
	// the GC only reclaims what an agent crash left behind; it is an explicit
	// opt-in. Host-only.
	WorkspaceRetentionDays int `yaml:"workspaceRetentionDays"`

	// Volumes declares the agent-owned named volumes this agent advertises and
	// jobs may mount (spec.volumes). A volume name exists once per agent: two
	// agents declaring the same name hold two independent copies.
	//
	// This field is FILE-ONLY. Unlike every scalar setting above it has no
	// command-line flag and no environment variable, because a list of
	// structured entries has no sensible flag or env spelling; AgentEffective
	// copies it from the config file and nowhere else.
	Volumes []VolumeConfig `yaml:"volumes"`
}

// VolumeConfig declares one named volume in the agent config file. Name and
// Mode are the contract a job sees; Path, Size and StorageClassName are the
// operator-only implementation and may differ between agents.
//
//	volumes:
//	  - name: unity-main
//	    mode: exclusive
//	    path: /var/lib/unified-cd/volumes/unity-main   # optional
//	    size: 200Gi                                    # advisory
//	  - name: go-cache
//	    mode: shared
type VolumeConfig struct {
	// Name is what jobs refer to. It is a single path segment: the default
	// Path is derived from it.
	Name string `yaml:"name"`
	// Mode is "exclusive" (at most one run on this agent holds the volume at a
	// time) or "shared" (any number of runs may hold it; unified-cd makes no
	// write-safety guarantee).
	Mode string `yaml:"mode"`
	// Path is the host directory backing the volume. Host agent only; it must
	// be absolute. Defaults to <workspaceDir>/volumes/<name>. It may not be,
	// or sit under, a working<N> directory, which the workspace GC sweeps.
	Path string `yaml:"path"`
	// Size is an advisory capacity such as "200Gi". The host agent measures
	// usage and warns on overrun; it never enforces the limit. Required by the
	// Kubernetes agent, which provisions a PVC of this size.
	Size string `yaml:"size"`
	// StorageClassName selects the PVC's StorageClass. Kubernetes agent only;
	// ignored by the host agent.
	StorageClassName string `yaml:"storageClassName"`
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
		v, err := ParseDiskThreshold(minFreeDiskEnv)
		if err != nil {
			return nil, fmt.Errorf("UNIFIED_AGENT_MIN_FREE_DISK: %w", err)
		}
		eff.MinFreeDisk = v
	}
	if retentionEnv := os.Getenv("UNIFIED_AGENT_WORKSPACE_RETENTION_DAYS"); retentionEnv != "" {
		if v, err := strconv.Atoi(retentionEnv); err == nil {
			eff.WorkspaceRetentionDays = v
		}
	}
	if v := os.Getenv("UNIFIED_AGENT_MAX_DETACHED"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			eff.MaxDetachedConcurrent = n
		}
	}
	if v := os.Getenv("UNIFIED_AGENT_FINALLY_TIMEOUT"); v != "" {
		// Unparseable is ignored rather than fatal, matching every other
		// duration/numeric env override above: the agent falls back to the
		// built-in 10m ceiling instead of refusing to start.
		if d, err := time.ParseDuration(v); err == nil {
			eff.FinallyTimeout = d
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
	if file.MaxDetachedConcurrent != 0 {
		eff.MaxDetachedConcurrent = file.MaxDetachedConcurrent
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
	if file.FinallyTimeout != 0 {
		eff.FinallyTimeout = file.FinallyTimeout
	}
	if file.PauseImage != "" {
		eff.PauseImage = file.PauseImage
	}
	if file.RunnerImage != "" {
		eff.RunnerImage = file.RunnerImage
	}
	if file.MinFreeDisk.Set {
		eff.MinFreeDisk = file.MinFreeDisk
	}
	if file.WorkspaceRetentionDays != 0 {
		eff.WorkspaceRetentionDays = file.WorkspaceRetentionDays
	}
	// File-only: there is no env var or flag to merge with (see
	// AgentConfig.Volumes).
	if len(file.Volumes) > 0 {
		eff.Volumes = file.Volumes
	}
	return eff, nil
}

// LabelsString returns Labels as a comma-separated string for use as a flag default.
func (c *AgentConfig) LabelsString() string {
	return strings.Join(c.Labels, ",")
}
