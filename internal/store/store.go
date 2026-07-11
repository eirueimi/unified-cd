package store

import (
	"context"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
)

// LogArchive holds log metadata for a Run that has been archived to object storage.
type LogArchive struct {
	RunID      string
	ObjectKey  string
	SizeBytes  int64
	ArchivedAt time.Time
}

// LogSearchMatch locates one search hit: Row is the 0-based row number
// within the same view ListLogsRange addresses.
type LogSearchMatch struct {
	Row       int64 `json:"row"`
	Seq       int64 `json:"seq"`
	StepIndex int   `json:"stepIndex"`
}

// PAT holds Personal Access Token metadata (does not include the token_hash).
type PAT struct {
	ID         string
	Name       string
	Role       string
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
}

// WebhookReceiver holds the configuration of a webhook receiver.
type WebhookReceiver struct {
	ID        string
	Name      string
	Spec      []byte
	UpdatedAt time.Time
}

// Schedule represents a cron schedule trigger.
type Schedule struct {
	Name        string
	Cron        string
	JobName     string
	Params      map[string]string
	LastFiredAt *time.Time
	UpdatedAt   time.Time
}

// StoredSecret holds an encrypted secret stored in the database.
type StoredSecret struct {
	ID           string
	Name         string
	Scope        string
	ScopeRef     string
	EncryptedDEK []byte
	Ciphertext   []byte
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// SecretMeta holds secret metadata without the secret value.
type SecretMeta struct {
	ID        string
	Name      string
	Scope     string
	ScopeRef  string
	CreatedAt time.Time
}

// ResourceRef identifies a resource managed by an AppSource.
type ResourceRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// AppSource holds a GitOps source definition.
type AppSource struct {
	Name             string
	Spec             []byte
	LastSyncedAt     *time.Time
	LastCommit       string
	ManagedResources []ResourceRef
	UpdatedAt        time.Time
	SyncStatus       string
	LastError        string
}

// GitCredential holds per-host Git credentials.
type GitCredential struct {
	ID        string
	Name      string
	Host      string
	CredType  string // "token" | "sshKey"
	SecretRef string // name of an existing StoredSecret
	CreatedAt time.Time
	UpdatedAt time.Time
}

// PendingRun holds the minimum information about a Run needed for git:// URI resolution.
type PendingRun struct {
	ID   string
	Spec []byte
}

type Store interface {
	UpsertJob(ctx context.Context, name, apiVersion string, spec []byte) (*api.Job, error)
	GetJob(ctx context.Context, name string) (*api.Job, error)
	ListJobs(ctx context.Context) ([]api.Job, error)
	DeleteJob(ctx context.Context, name string) error
	// RenameJob re-keys a job from oldName to newName and repoints run history,
	// in a single transaction. If newName already exists, the oldName row is
	// treated as an orphan: run history is repointed to newName and the orphan is
	// deleted. Idempotent: a missing oldName is a no-op.
	RenameJob(ctx context.Context, oldName, newName string) error
	CreateRun(ctx context.Context, jobName string, params map[string]string, spec []byte, agentSelector []string, requiredCaps []string, triggeredBy string) (*api.Run, error)
	// ListChildRunIDs returns the IDs of runs directly spawned by parentRunID via
	// call: steps (recorded as child_run_id on the parent's step reports), so a
	// cancellation of the parent can cascade to its children.
	ListChildRunIDs(ctx context.Context, parentRunID string) ([]string, error)
	// ListRunningRunIDsByAgent returns IDs of Running runs claimed by agentID,
	// so an agent can fail runs its previous process incarnation left behind.
	ListRunningRunIDsByAgent(ctx context.Context, agentID string) ([]string, error)
	GetRun(ctx context.Context, id string) (*api.Run, error)
	GetRunSpec(ctx context.Context, id string) ([]byte, error)
	ListRunsByJob(ctx context.Context, jobName string, limit int) ([]api.Run, error)
	ListActiveRuns(ctx context.Context) ([]api.Run, error)
	TransitionPendingToQueued(ctx context.Context, limit int) (int, error)
	ClaimNextRun(ctx context.Context, agentID string, agentLabels []string) (*ClaimedRun, error)
	MarkRunRunning(ctx context.Context, runID string) error
	MarkRunFinished(ctx context.Context, runID string, status api.RunStatus) error
	// FinishRun is like MarkRunFinished but reports whether the run actually
	// transitioned (false when it was already terminal).
	FinishRun(ctx context.Context, runID string, status api.RunStatus) (updated bool, err error)
	// CountRunsByStatus returns the number of non-terminal runs per status.
	CountRunsByStatus(ctx context.Context) (map[api.RunStatus]int, error)
	// CountAgentsByLiveness partitions registered agents by heartbeat freshness.
	CountAgentsByLiveness(ctx context.Context, staleAfter time.Duration) (alive, stale int, err error)
	DeleteRun(ctx context.Context, id string) error
	UpsertStepReport(ctx context.Context, runID string, stepIndex int, stageIndex int, stepName, variant, status string, exitCode *int, startedAt, endedAt *time.Time, childRunID, callJobName string) error
	GetRunSteps(ctx context.Context, runID string) ([]api.StepReport, error)
	// GetRunParent returns the call step (and parent run) that launched childRunID,
	// or nil if the run was not created by a call step.
	GetRunParent(ctx context.Context, childRunID string) (*api.CalledBy, error)
	AppendLog(ctx context.Context, runID string, stepIndex int, stream string, ts time.Time, line string) (int64, error)
	TailLogs(ctx context.Context, runID string, afterSeq int64, limit int) ([]api.LogLine, error)
	// TailLogsRecent returns up to the last `limit` log lines for the run, in
	// ascending seq order (the tail of the log), so a bounded backfill can keep
	// the end of a huge log rather than its beginning.
	TailLogsRecent(ctx context.Context, runID string, limit int) ([]api.LogLine, error)
	// CountLogs returns the number of log lines (and the min/max seq) for the
	// run, optionally restricted to the given step indexes (nil/empty = all).
	CountLogs(ctx context.Context, runID string, steps []int) (count, minSeq, maxSeq int64, err error)
	// ListLogsRange returns `limit` lines starting at 0-based row `offset` in
	// seq order, optionally restricted to steps. Row numbering is per-view:
	// with steps set, offset 0 is the view's first line.
	ListLogsRange(ctx context.Context, runID string, steps []int, offset, limit int) ([]api.LogLine, error)
	// SearchLogs returns up to `capN` case-insensitive substring matches plus the
	// TOTAL match count (which may exceed capN). q is a raw substring; ILIKE
	// metacharacters are escaped internally.
	SearchLogs(ctx context.Context, runID string, steps []int, q string, capN int) (total int64, matches []LogSearchMatch, err error)
	// UpsertAgent is the REGISTRATION path: it replaces the agent's labels/hostname/
	// os/version/env wholesale (a registration is the authoritative identity).
	UpsertAgent(ctx context.Context, agentID, hostname, os, version string, labels []string, capabilities []string, env map[string]string) error
	// UpsertAgentOnClaim is the CLAIM path: a lightweight, non-destructive upsert that
	// merges labels and only overwrites scalar fields when non-empty, so a claim never
	// clobbers richer data recorded at registration time.
	UpsertAgentOnClaim(ctx context.Context, agentID, hostname, os, version string, labels []string, env map[string]string) error
	TouchAgent(ctx context.Context, agentID string) error
	DeleteAgent(ctx context.Context, agentID string) error
	// ListAgents returns all agents ordered by last access descending.
	ListAgents(ctx context.Context) ([]api.AgentInfo, error)
	// GetAgent returns the agent with the given ID. Returns nil, nil if not found.
	GetAgent(ctx context.Context, id string) (*api.AgentInfo, error)
	// ListRunsByAgent returns Runs executed by the given agent, newest first.
	ListRunsByAgent(ctx context.Context, agentID string, limit int) ([]api.Run, error)
	// DeleteStaleAgents deletes agents whose last_seen_at is older than olderThan and returns the count deleted.
	DeleteStaleAgents(ctx context.Context, olderThan time.Duration) (int64, error)
	// ListStuckRunIDs returns IDs of Running runs whose claiming agent is gone or has
	// not sent a heartbeat within staleAfter, excluding runs claimed within the grace
	// window (to avoid reaping a just-claimed run before its first heartbeat).
	ListStuckRunIDs(ctx context.Context, staleAfter, grace time.Duration) ([]string, error)
	// ListUnclaimableQueuedRuns returns Queued runs older than minAge for which no
	// live agent (last_seen within staleAfter) has labels satisfying the run's
	// agentSelector — i.e. runs that can never be claimed because the agent they
	// need is gone. Used by the queued-run reaper to fail them instead of leaving
	// them "in progress" forever.
	ListUnclaimableQueuedRuns(ctx context.Context, minAge, staleAfter time.Duration) ([]QueuedRunRef, error)

	// Concurrency — mutex
	AcquireMutex(ctx context.Context, mutexName, runID string) (bool, error)
	ReleaseMutex(ctx context.Context, mutexName string) error

	// Concurrency — semaphore pool
	UpsertSemaphorePool(ctx context.Context, poolName string, capacity int) error
	AcquireSemaphore(ctx context.Context, poolName, runID string) (bool, error)
	ReleaseSemaphore(ctx context.Context, poolName, runID string) error

	// Outputs — step level
	SetStepOutput(ctx context.Context, runID string, stepIndex int, variant, key, value string) error
	GetStepOutputs(ctx context.Context, runID string, stepIndex int) (map[string]string, error)

	// Outputs — run level
	SetRunOutput(ctx context.Context, runID, key, value string) error
	GetRunOutputs(ctx context.Context, runID string) (map[string]string, error)

	// Scheduler — advisory lock
	// AcquireSchedulerLock tries to acquire a session-level advisory lock on a dedicated connection.
	// Returns (release, nil) if acquired — caller MUST call release() to unlock and return the connection.
	// Returns (nil, nil) if another replica holds the lock.
	AcquireSchedulerLock(ctx context.Context) (release func(), err error)
	// AcquireAdvisoryLock non-blockingly acquires a session-level advisory lock for the given key.
	// Acquired: (release, nil) — caller MUST call release() to unlock and return the connection.
	// Held by another replica: (nil, nil)
	// Error: (nil, err)
	AcquireAdvisoryLock(ctx context.Context, key int64) (release func(), err error)

	// Log Archives
	ListRunsNeedingArchival(ctx context.Context, limit int) ([]api.Run, error)
	CreateLogArchive(ctx context.Context, runID, objectKey string, sizeBytes int64) error
	GetLogArchive(ctx context.Context, runID string) (*LogArchive, error)

	// ListenForNotify subscribes to a Postgres channel and calls the callback for each notification.
	// Blocks until ctx is cancelled.
	ListenForNotify(ctx context.Context, channel string, callback func(payload string)) error

	// PAT
	CreatePAT(ctx context.Context, name, tokenHash, role string, expiresAt *time.Time) (*PAT, error)
	GetPATByHash(ctx context.Context, tokenHash string) (*PAT, error)
	ListPATs(ctx context.Context) ([]PAT, error)
	DeletePAT(ctx context.Context, id string) error
	TouchPAT(ctx context.Context, id string) error
	// UpsertBootstrapPAT creates or updates the hash of the single PAT row identified by name.
	// Used to sync UNIFIED_TOKEN as a PAT on each startup (replaces the hash when the value changes, never creates duplicate rows).
	UpsertBootstrapPAT(ctx context.Context, name, tokenHash string) (*PAT, error)
	// DeleteBootstrapPATByName deletes the PAT row identified by name (no-op if it does not exist).
	// Used to avoid leaving a previously synced row behind when UNIFIED_TOKEN is unset.
	DeleteBootstrapPATByName(ctx context.Context, name string) error

	// WebhookReceivers
	UpsertWebhookReceiver(ctx context.Context, name string, spec []byte) (*WebhookReceiver, error)
	GetWebhookReceiver(ctx context.Context, name string) (*WebhookReceiver, error)
	ListWebhookReceivers(ctx context.Context) ([]WebhookReceiver, error)
	DeleteWebhookReceiver(ctx context.Context, name string) error

	// Secrets
	UpsertSecret(ctx context.Context, name, scope, scopeRef string, encryptedDEK, ciphertext []byte) (*StoredSecret, error)
	GetSecret(ctx context.Context, name, scope, scopeRef string) (*StoredSecret, error)
	ListSecrets(ctx context.Context, scope, scopeRef string) ([]SecretMeta, error)
	DeleteSecret(ctx context.Context, name, scope, scopeRef string) error

	// OIDCState
	CreateOIDCState(ctx context.Context, state, redirectTo string, expiresAt time.Time) (*OIDCState, error)
	GetAndDeleteOIDCState(ctx context.Context, state string) (*OIDCState, error)
	DeleteExpiredOIDCStates(ctx context.Context) error

	// Sessions
	CreateSession(ctx context.Context, tokenHash, sub, email, role, encryptedRefreshToken string, expiresAt time.Time) (*Session, error)
	GetSessionByHash(ctx context.Context, tokenHash string) (*Session, error)
	UpdateSessionExpiry(ctx context.Context, id, encryptedRefreshToken string, expiresAt time.Time) error
	DeleteSession(ctx context.Context, id string) error
	TouchSession(ctx context.Context, id string) error

	// GitCredentials
	UpsertGitCredential(ctx context.Context, name, host, credType, secretRef string) error
	GetGitCredentialByHost(ctx context.Context, host string) (*GitCredential, error)
	ListGitCredentials(ctx context.Context) ([]GitCredential, error)
	DeleteGitCredential(ctx context.Context, name string) error

	// AppSources
	UpsertAppSource(ctx context.Context, name string, spec []byte) (*AppSource, error)
	GetAppSource(ctx context.Context, name string) (*AppSource, error)
	ListAppSources(ctx context.Context) ([]AppSource, error)
	DeleteAppSource(ctx context.Context, name string) error
	UpdateAppSourceSyncState(ctx context.Context, name, lastCommit string, syncedAt time.Time, managed []ResourceRef) error
	ResetAppSourceCommit(ctx context.Context, name string) error
	SetAppSourceSyncStatus(ctx context.Context, name, status, lastError string) error
	// FindManagingAppSource returns the AppSource whose managed_resources
	// contains {kind,name}, or nil when the resource is not managed by any
	// AppSource. Exact match only (Job names are qualified).
	FindManagingAppSource(ctx context.Context, kind, name string) (*AppSource, error)
	// ResetStuckSyncingAppSources resets AppSources stuck in sync_status='Syncing'
	// whose updated_at is older than olderThan to a retryable state (clears
	// last_commit so the next reconcile tick re-syncs), returning the count reset.
	ResetStuckSyncingAppSources(ctx context.Context, olderThan time.Duration) (int, error)

	// Git resolver helpers
	ListPendingRuns(ctx context.Context, limit int) ([]PendingRun, error)
	UpdateRunSpec(ctx context.Context, runID string, specJSON []byte) error

	// Schedules
	UpsertSchedule(ctx context.Context, name, cron, jobName string, params map[string]string) (*Schedule, error)
	GetSchedule(ctx context.Context, name string) (*Schedule, error)
	ListSchedules(ctx context.Context) ([]Schedule, error)
	DeleteSchedule(ctx context.Context, name string) error
	UpdateScheduleLastFiredAt(ctx context.Context, name string, firedAt time.Time) error

	// Approvals
	// CreatePendingApproval inserts a new approval gate in Pending status.
	// Idempotent: if a row with the same (run_id, step_index) already exists, it is left untouched.
	CreatePendingApproval(ctx context.Context, runID string, stepIndex int, stepName, message string, timeoutAt *time.Time) error
	// DecideApproval conditionally updates an approval from Pending to the given status (first-writer-wins).
	// Returns true if a row was changed, false if the gate was already decided.
	DecideApproval(ctx context.Context, runID string, stepIndex int, status, decidedBy, comment string) (bool, error)
	// GetApproval returns the approval gate for the given run and step.
	GetApproval(ctx context.Context, runID string, stepIndex int) (api.RunApproval, error)
	// ListRunApprovals returns all approval gates for the given run, ordered by step_index.
	ListRunApprovals(ctx context.Context, runID string) ([]api.RunApproval, error)
	// MarkExpiredApprovalsTimedOut marks all Pending approvals whose timeout has
	// passed as TimedOut (system-decided). Returns the number of rows updated.
	MarkExpiredApprovalsTimedOut(ctx context.Context) (int, error)

	// ControllerSettings
	// EnsureControllerKey returns the persisted controllerKey (hex string for the KEK).
	// If none exists yet, it stores candidateHex and returns it (safe against simultaneous first-startup from multiple replicas).
	EnsureControllerKey(ctx context.Context, candidateHex string) (string, error)

	// Audit
	// InsertAuditLog records a single state-changing API operation.
	InsertAuditLog(ctx context.Context, actor, method, path, action, resource string, status int) error
	// ListAuditLogs returns audit log entries newest-first, with limit/offset pagination.
	ListAuditLogs(ctx context.Context, limit, offset int) ([]api.AuditLog, error)
	// DeleteAuditLogsOlderThan deletes audit log rows with occurred_at before the given time.
	// Returns the number of rows deleted.
	DeleteAuditLogsOlderThan(ctx context.Context, before time.Time) (int, error)

	// Connectivity
	// Ping checks connectivity to the database.
	Ping(ctx context.Context) error

	Close()
}

// OIDCState holds the CSRF-protection state for an OIDC flow.
type OIDCState struct {
	ID         string
	State      string
	RedirectTo string
	CreatedAt  time.Time
	ExpiresAt  time.Time
}

// Session holds a browser session.
type Session struct {
	ID           string
	TokenHash    string
	Sub          string
	Email        string
	Role         string
	RefreshToken string // encrypted by KeyManager
	ExpiresAt    time.Time
	LastUsedAt   *time.Time
	CreatedAt    time.Time
}

// ClaimedRun holds a Run that has been claimed by an agent.
type ClaimedRun struct {
	api.Run
	Spec []byte
}

// QueuedRunRef identifies a Queued run and the agent labels it requires, so the
// queued-run reaper can report why it could not be claimed.
type QueuedRunRef struct {
	ID            string
	AgentSelector []string
}
