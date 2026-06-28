package api

import (
	"time"

	"github.com/eirueimi/unified-cd/internal/dsl"
)

type ApplyJobRequest struct {
	YAML string `json:"yaml"`
}

// InputDef represents an input parameter definition for a job.
type InputDef struct {
	Name        string `json:"name"`
	Type        string `json:"type"`        // "string" | "bool" | "int"
	Required    bool   `json:"required,omitempty"`
	Default     any    `json:"default,omitempty"`
	Description string `json:"description,omitempty"`
}

type Job struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	APIVersion string     `json:"apiVersion"`
	Spec       []byte     `json:"spec"`
	Inputs     []InputDef `json:"inputs,omitempty"`
	UpdatedAt  time.Time  `json:"updatedAt"`
}

type RunStatus string

const (
	RunPending   RunStatus = "Pending"
	RunQueued    RunStatus = "Queued"
	RunRunning   RunStatus = "Running"
	RunSucceeded RunStatus = "Succeeded"
	RunFailed    RunStatus = "Failed"
	RunCancelled RunStatus = "Cancelled"
)

type TriggerRunRequest struct {
	JobName string            `json:"jobName"`
	Params  map[string]string `json:"params,omitempty"`
}

type Run struct {
	ID          string            `json:"id"`
	JobName     string            `json:"jobName"`
	Status      RunStatus         `json:"status"`
	Params      map[string]string `json:"params"`
	CreatedAt   time.Time         `json:"createdAt"`
	UpdatedAt   time.Time         `json:"updatedAt"`
	TriggeredBy string            `json:"triggeredBy"`
}

type AgentRegisterRequest struct {
	AgentID  string            `json:"agentId"`
	Hostname string            `json:"hostname"`
	OS       string            `json:"os"`
	Labels   []string          `json:"labels,omitempty"`
	Version  string            `json:"version,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
}

type ClaimResponse struct {
	RunID          string            `json:"runId"`
	JobName        string            `json:"jobName"`
	Params         map[string]string `json:"params"`
	Stages         []ClaimStage      `json:"stages"`
	JobOutputs     []string          `json:"jobOutputs"`
	SecretsNeeded  []string          `json:"secretsNeeded"`
	// FailFast removed
	TimeoutMinutes float64           `json:"timeoutMinutes,omitempty"`
	PodTemplate    *dsl.PodTemplate  `json:"podTemplate,omitempty"`
}

// ClaimStage is either a single step (Step set) or an explicit parallel group (Parallel set).
// Foreach steps arrive as a single ClaimStep with Foreach set; the agent expands them at runtime.
type ClaimStage struct {
	Step     *ClaimStep  `json:"step,omitempty"`
	Parallel []ClaimStep `json:"parallel,omitempty"`
}

type ClaimStep struct {
	Index            int                   `json:"index"`
	StageIndex       int                   `json:"stageIndex"`
	Name             string                `json:"name"`
	If               string                `json:"if,omitempty"`
	Env              map[string]string     `json:"env,omitempty"`
	Run              string                `json:"run"`
	Outputs          map[string]string     `json:"outputs,omitempty"`
	Call             *ClaimCallStep        `json:"call,omitempty"`
	Cache            *dsl.CacheStep        `json:"cache,omitempty"`
	Post             *PostStep             `json:"post,omitempty"`
	// Needs removed — use parallel: or foreach:
	ContinueOnError  bool                  `json:"continueOnError,omitempty"`
	Container        string                `json:"container,omitempty"`
	TimeoutMinutes   float64               `json:"timeoutMinutes,omitempty"`
	UploadArtifact   *UploadArtifactStep   `json:"uploadArtifact,omitempty"`
	DownloadArtifact *DownloadArtifactStep `json:"downloadArtifact,omitempty"`
	Foreach          *ClaimForeachDef      `json:"foreach,omitempty"`
	ForeachKey       string                `json:"foreachKey,omitempty"`
	ForeachValue     string                `json:"foreachValue,omitempty"`
}

type ClaimForeachDef struct {
	Key    string             `json:"key"`
	Source ClaimForeachSource `json:"source"`
}

type ClaimForeachSource struct {
	Literal []string `json:"literal,omitempty"`
	Expr    string   `json:"expr,omitempty"`
}

type ClaimCallStep struct {
	Job    string            `json:"job"`
	Params map[string]string `json:"params"` // template strings (expanded by the agent at runtime)
}

type StepReportRequest struct {
	RunID      string    `json:"runId"`
	StepIndex  int       `json:"stepIndex"`
	StageIndex int       `json:"stageIndex"`
	StepName   string    `json:"stepName"`
	Status     string    `json:"status"`
	ExitCode   int       `json:"exitCode,omitempty"`
	StartedAt  time.Time `json:"startedAt,omitempty"`
	EndedAt    time.Time `json:"endedAt,omitempty"`
}

type StepReport struct {
	Index      int        `json:"index"`
	StageIndex int        `json:"stageIndex"`
	Name       string     `json:"name"`
	Status     string     `json:"status"`
	ExitCode   *int       `json:"exitCode,omitempty"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	EndedAt    *time.Time `json:"endedAt,omitempty"`
}

type LogAppendRequest struct {
	RunID     string    `json:"runId"`
	StepIndex int       `json:"stepIndex"`
	Stream    string    `json:"stream"`
	Timestamp time.Time `json:"timestamp"`
	Line      string    `json:"line"`
}

type LogLine struct {
	Seq       int64     `json:"seq"`
	StepIndex int       `json:"stepIndex"`
	Stream    string    `json:"stream"`
	Timestamp time.Time `json:"timestamp"`
	Line      string    `json:"line"`
}

// ---- outputs ----

// SetOutputsRequest is the request body used by the agent to report step or run outputs.
type SetOutputsRequest struct {
	Outputs map[string]string `json:"outputs"`
}

// RunOutputs is the GET response for run-level outputs.
type RunOutputs struct {
	RunID   string            `json:"runId"`
	Outputs map[string]string `json:"outputs"`
}

// ---- secrets ----

// SetSecretRequest is the request for creating or updating a secret.
type SetSecretRequest struct {
	Name     string `json:"name"`
	Scope    string `json:"scope,omitempty"`
	ScopeRef string `json:"scopeRef,omitempty"`
	Value    string `json:"value"`
}

// SecretMeta is the metadata for a secret without its value (for API responses).
type SecretMeta struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Scope     string    `json:"scope"`
	ScopeRef  string    `json:"scopeRef"`
	CreatedAt time.Time `json:"createdAt"`
}

// AgentFetchSecretsRequest is the request used by the agent to fetch secrets.
type AgentFetchSecretsRequest struct {
	Names []string `json:"names"`
}

// AgentFetchSecretsResponse is the response containing secret values sent to the agent.
type AgentFetchSecretsResponse struct {
	Secrets map[string]string `json:"secrets"`
}

// ---- PAT ----

// CreatePATRequest is the request for creating a PAT.
type CreatePATRequest struct {
	Name      string `json:"name"`
	ExpiresIn string `json:"expiresIn,omitempty"`
}

// CreatePATResponse is the PAT creation response (the token is shown only once).
type CreatePATResponse struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Token string `json:"token"` // returned only at creation time
}

// PATMeta is the metadata for a PAT (does not include the token hash).
type PATMeta struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"createdAt"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
}

// ---- webhook ----

// ApplyWebhookRequest is the request for registering or updating a WebhookReceiver YAML.
type ApplyWebhookRequest struct {
	YAML string `json:"yaml"`
}

// WebhookReceiverMeta is the metadata for a WebhookReceiver (for API responses).
type WebhookReceiverMeta struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ---- schedules ----

// ApplyScheduleRequest is the request for registering or updating a Schedule YAML.
type ApplyScheduleRequest struct {
	YAML string `json:"yaml"`
}

// ScheduleMeta is the metadata for a Schedule (for API responses).
type ScheduleMeta struct {
	Name        string     `json:"name"`
	Cron        string     `json:"cron"`
	JobName     string     `json:"jobName"`
	LastFiredAt *time.Time `json:"lastFiredAt,omitempty"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}

// AgentInfo holds the status information of an agent.
type AgentInfo struct {
	ID         string            `json:"id"`
	Hostname   string            `json:"hostname"`
	OS         string            `json:"os"`
	Labels     []string          `json:"labels"`
	Version    string            `json:"version"`
	Env        map[string]string `json:"env"`
	LastSeenAt time.Time         `json:"lastSeenAt"`
}

type UploadArtifactStep struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type DownloadArtifactStep struct {
	Name    string `json:"name"`
	DestDir string `json:"destDir,omitempty"`
}

// ---- post step ----

// PostStep is the post-processing executed after a step completes. Included in ClaimStep.
type PostStep struct {
	Run string            `json:"run,omitempty"`
	Env map[string]string `json:"env,omitempty"`
}

// ---- gitcredential ----

// UpsertGitCredentialRequest is the request for registering or updating a GitCredential.
type UpsertGitCredentialRequest struct {
	Name      string `json:"name"`
	Host      string `json:"host"`
	CredType  string `json:"credType"`  // "token" | "sshKey"
	SecretRef string `json:"secretRef"` // name of an existing secret
}

// GitCredentialMeta is the metadata for a GitCredential (for API responses).
type GitCredentialMeta struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Host      string    `json:"host"`
	CredType  string    `json:"credType"`
	SecretRef string    `json:"secretRef"`
	CreatedAt time.Time `json:"createdAt"`
}

// ---- appsource ----

// ApplyAppSourceRequest is the request for registering or updating an AppSource YAML.
type ApplyAppSourceRequest struct {
	YAML string `json:"yaml"`
}

// AppSourceMeta is the metadata for an AppSource (for API responses).
type AppSourceMeta struct {
	Name           string     `json:"name"`
	RepoURL        string     `json:"repoURL"`
	TargetRevision string     `json:"targetRevision"`
	Path           string     `json:"path"`
	LastSyncedAt   *time.Time `json:"lastSyncedAt,omitempty"`
	LastCommit     string     `json:"lastCommit,omitempty"`
	UpdatedAt      time.Time  `json:"updatedAt"`
}
