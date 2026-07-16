package api

import (
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/dsl"
)

type ApplyJobRequest struct {
	YAML string `json:"yaml"`
}

// InputDef represents an input parameter definition for a job.
type InputDef struct {
	Name        string `json:"name"`
	Type        string `json:"type"` // "string" | "bool" | "int"
	Required    bool   `json:"required,omitempty"`
	Default     any    `json:"default,omitempty"`
	Description string `json:"description,omitempty"`
}

type Job struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Path        string     `json:"path"`
	Leaf        string     `json:"leaf"`
	APIVersion  string     `json:"apiVersion"`
	Spec        []byte     `json:"spec"`
	Inputs      []InputDef `json:"inputs,omitempty"`
	Description string     `json:"description,omitempty"`
	UpdatedAt   time.Time  `json:"updatedAt"`
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
	ClaimedBy   string            `json:"claimedBy,omitempty"` // Claiming agent's ID; empty until claimed.
	CalledBy    *CalledBy         `json:"calledBy,omitempty"`
}

// CalledBy identifies the call step (and its run) that launched this run.
type CalledBy struct {
	ParentRunID   string `json:"parentRunId"`
	ParentJobName string `json:"parentJobName"`
	StepName      string `json:"stepName"`
}

// HeartbeatRequest is the (optional) body of an agent heartbeat. A live agent
// (one built after active-run tracking was added) always sends this body,
// even when ActiveRunIDs is empty — an empty slice still marshals a body
// (the omitempty tag only drops the field itself, not the JSON object),
// letting the controller tell "live agent, zero active runs" (a reconcile
// candidate) apart from "legacy agent, no body at all" (skip: unknown).
type HeartbeatRequest struct {
	ActiveRunIDs []string `json:"activeRunIds,omitempty"`
}

type AgentRegisterRequest struct {
	AgentID      string            `json:"agentId"`
	Hostname     string            `json:"hostname"`
	OS           string            `json:"os"`
	Labels       []string          `json:"labels,omitempty"`
	Version      string            `json:"version,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	Capabilities []string          `json:"capabilities,omitempty"`
}

type ClaimResponse struct {
	RunID         string            `json:"runId"`
	JobName       string            `json:"jobName"`
	Params        map[string]string `json:"params"`
	Stages        []ClaimStage      `json:"stages"`
	Finally       []ClaimStage      `json:"finally,omitempty"`
	JobOutputs    []string          `json:"jobOutputs"`
	SecretsNeeded []string          `json:"secretsNeeded"`
	// FailFast removed
	TimeoutMinutes        float64          `json:"timeoutMinutes,omitempty"`
	PodTemplate           *dsl.PodTemplate `json:"podTemplate,omitempty"`
	Native                bool             `json:"native,omitempty"`
	MatrixMaxCombinations int              `json:"matrixMaxCombinations,omitempty"`
}

// ClaimStage is either a single step (Step set) or an explicit parallel group (Parallel set).
// Foreach/matrix steps arrive as a single ClaimStep with Matrix set (foreach is normalized to a
// 1-dimension matrix); the agent expands them at runtime.
type ClaimStage struct {
	Step     *ClaimStep  `json:"step,omitempty"`
	Parallel []ClaimStep `json:"parallel,omitempty"`
}

type ClaimStep struct {
	Index      int               `json:"index"`
	StageIndex int               `json:"stageIndex"`
	Name       string            `json:"name"`
	If         string            `json:"if,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Run        string            `json:"run"`
	Outputs    map[string]string `json:"outputs,omitempty"`
	Call       *ClaimCallStep    `json:"call,omitempty"`
	Cache      *dsl.CacheStep    `json:"cache,omitempty"`
	Post       *PostStep         `json:"post,omitempty"`
	// Needs removed — use parallel: or foreach:
	ContinueOnError  bool                  `json:"continueOnError,omitempty"`
	Container        string                `json:"container,omitempty"`
	ScopeID          string                `json:"scopeID,omitempty"`
	ScopeImage       string                `json:"scopeImage,omitempty"`
	TimeoutMinutes   float64               `json:"timeoutMinutes,omitempty"`
	Retry            *dsl.RetrySpec        `json:"retry,omitempty"`
	UploadArtifact   *UploadArtifactStep   `json:"uploadArtifact,omitempty"`
	DownloadArtifact *DownloadArtifactStep `json:"downloadArtifact,omitempty"`
	Matrix           *ClaimMatrixDef       `json:"matrix,omitempty"`
	MatrixValues     map[string]string     `json:"matrixValues,omitempty"`
	MatrixKey        string                `json:"matrixKey,omitempty"`
	Approval         *ClaimApproval        `json:"approval,omitempty"`
	// Shell is the effective interpreter argv resolved by the controller
	// (step.shell if set, else spec.shell, else nil). Nil means "the agent
	// applies the shim default" — the controller never writes the /.ucd
	// path itself; that is agent territory.
	Shell []string `json:"shell,omitempty"`
}

// DisplayName returns the human-facing step name: matrix copies get the
// combination appended, e.g. `build (linux, amd64)`. Safe because dimension
// values may not contain "/" (enforced at expansion).
func (s ClaimStep) DisplayName() string {
	if s.MatrixKey == "" {
		return s.Name
	}
	return s.Name + " (" + strings.ReplaceAll(s.MatrixKey, "/", ", ") + ")"
}

// ClaimMatrixDef is the wire form of a matrix (or foreach, normalized to one
// dimension) definition. The agent expands it at runtime.
type ClaimMatrixDef struct {
	Dimensions []ClaimMatrixDimension `json:"dimensions"`
	Exclude    []map[string]string    `json:"exclude,omitempty"`
}

type ClaimMatrixDimension struct {
	Name   string             `json:"name"`
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
	Variant    string    `json:"variant,omitempty"`

	ChildRunID  string `json:"childRunId,omitempty"`
	CallJobName string `json:"callJobName,omitempty"`
}

type StepReport struct {
	Index      int        `json:"index"`
	StageIndex int        `json:"stageIndex"`
	Name       string     `json:"name"`
	Status     string     `json:"status"`
	ExitCode   *int       `json:"exitCode,omitempty"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	EndedAt    *time.Time `json:"endedAt,omitempty"`
	Variant    string     `json:"variant,omitempty"`

	ChildRunID  string `json:"childRunId,omitempty"`
	CallJobName string `json:"callJobName,omitempty"`

	Kind    string `json:"kind,omitempty"`    // run|cache|call|uses|uploadArtifact|downloadArtifact|approval|sidecar
	Section string `json:"section,omitempty"` // main|finally
	Matrix  bool   `json:"matrix,omitempty"`  // true if the (planned) step is a matrix/foreach step
}

type LogAppendRequest struct {
	RunID     string    `json:"runId"`
	StepIndex int       `json:"stepIndex"`
	Stream    string    `json:"stream"`
	Timestamp time.Time `json:"timestamp"`
	Line      string    `json:"line"`
}

// SidecarStatusRequest reports one user sidecar container's phase/exit to the
// controller for display. Phase is "running" or "exited". ExitCode is set only
// when Phase == "exited".
type SidecarStatusRequest struct {
	RunID    string `json:"runId"`
	Name     string `json:"name"`
	Index    int    `json:"index"`
	Phase    string `json:"phase"`
	ExitCode *int   `json:"exitCode,omitempty"`
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
	Role      string `json:"role,omitempty"`
}

// CreatePATResponse is the PAT creation response (the token is shown only once).
type CreatePATResponse struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Role  string `json:"role"`
	Token string `json:"token"` // returned only at creation time
}

// PATMeta is the metadata for a PAT (does not include the token hash).
type PATMeta struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Role       string     `json:"role"`
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
	Spec      []byte    `json:"spec,omitempty"`
}

// ---- schedules ----

// ApplyScheduleRequest is the request for registering or updating a Schedule YAML.
type ApplyScheduleRequest struct {
	YAML string `json:"yaml"`
}

// ScheduleMeta is the metadata for a Schedule (for API responses).
type ScheduleMeta struct {
	Name        string            `json:"name"`
	Cron        string            `json:"cron"`
	JobName     string            `json:"jobName"`
	LastFiredAt *time.Time        `json:"lastFiredAt,omitempty"`
	UpdatedAt   time.Time         `json:"updatedAt"`
	Params      map[string]string `json:"params,omitempty"`
}

// AgentInfo holds the status information of an agent.
type AgentInfo struct {
	ID           string            `json:"id"`
	Hostname     string            `json:"hostname"`
	OS           string            `json:"os"`
	Labels       []string          `json:"labels"`
	Version      string            `json:"version"`
	Env          map[string]string `json:"env"`
	LastSeenAt   time.Time         `json:"lastSeenAt"`
	Capabilities []string          `json:"capabilities,omitempty"`
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
	// Shell is carried only when the dsl post: hook declares its own
	// shell:. Nil means the agent inherits the owning step's effective
	// ClaimStep.Shell.
	Shell []string `json:"shell,omitempty"`
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

// ---- approvals ----

// ClaimApproval is the approval gate definition sent to agents in a ClaimStep.
type ClaimApproval struct {
	Message        string  `json:"message,omitempty"`
	TimeoutMinutes float64 `json:"timeoutMinutes"`
}

// ApprovalDecisionRequest is the request body for approving or rejecting an approval gate.
type ApprovalDecisionRequest struct {
	Decision string `json:"decision"` // "approve" | "reject"
	Comment  string `json:"comment,omitempty"`
}

// CreateApprovalRequest is the request body for creating an approval record.
type CreateApprovalRequest struct {
	StepIndex      int     `json:"stepIndex"`
	StepName       string  `json:"stepName"`
	Message        string  `json:"message,omitempty"`
	TimeoutMinutes float64 `json:"timeoutMinutes"`
}

// RunApproval represents a manual approval gate for a run step.
type RunApproval struct {
	RunID     string     `json:"runId"`
	StepIndex int        `json:"stepIndex"`
	StepName  string     `json:"stepName"`
	Message   string     `json:"message"`
	Status    string     `json:"status"` // Pending | Approved | Rejected | TimedOut
	DecidedBy string     `json:"decidedBy,omitempty"`
	Comment   string     `json:"comment,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
	TimeoutAt *time.Time `json:"timeoutAt,omitempty"`
	DecidedAt *time.Time `json:"decidedAt,omitempty"`
}

// ---- audit ----

// AuditLog represents a single recorded state-changing API operation.
type AuditLog struct {
	ID         int64     `json:"id"`
	OccurredAt time.Time `json:"occurredAt"`
	Actor      string    `json:"actor"`
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	Action     string    `json:"action"`
	Resource   string    `json:"resource,omitempty"`
	Status     int       `json:"status"`
}

// ---- appsource ----

// ApplyAppSourceRequest is the request for registering or updating an AppSource YAML.
type ApplyAppSourceRequest struct {
	YAML string `json:"yaml"`
}

// ResourceRef identifies a resource managed by an AppSource
// (API mirror of store.ResourceRef).
type ResourceRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// AppSourceSyncPolicy is the API mirror of dsl.AppSyncPolicy.
type AppSourceSyncPolicy struct {
	Interval            string `json:"interval,omitempty"`
	Prune               bool   `json:"prune,omitempty"`
	AllowManualOverride bool   `json:"allowManualOverride,omitempty"`
}

// AppSourceMeta is the metadata for an AppSource (for API responses).
type AppSourceMeta struct {
	Name             string               `json:"name"`
	RepoURL          string               `json:"repoURL"`
	TargetRevision   string               `json:"targetRevision"`
	Path             string               `json:"path"`
	LastSyncedAt     *time.Time           `json:"lastSyncedAt,omitempty"`
	LastCommit       string               `json:"lastCommit,omitempty"`
	SyncStatus       string               `json:"syncStatus,omitempty"`
	LastError        string               `json:"lastError,omitempty"`
	UpdatedAt        time.Time            `json:"updatedAt"`
	SyncPolicy       *AppSourceSyncPolicy `json:"syncPolicy,omitempty"`
	ManagedResources []ResourceRef        `json:"managedResources,omitempty"`
}
