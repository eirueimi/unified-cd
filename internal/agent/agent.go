package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
	"k8s.io/apimachinery/pkg/api/resource"
)

// hostContainerLimits converts a validated runsIn.resources spec to the OCI-CLI
// limit values: cpu as a core decimal, memory as bytes. Only limits map on the
// host (docker has no request concept); requests are k8s-scheduling-only.
func hostContainerLimits(rs *dsl.ResourceSpec) (cpu, mem string) {
	if rs == nil || rs.Limits == nil {
		return "", ""
	}
	if rs.Limits.CPU != "" {
		if q, err := resource.ParseQuantity(rs.Limits.CPU); err == nil {
			cpu = strconv.FormatFloat(float64(q.MilliValue())/1000.0, 'g', -1, 64)
		}
	}
	if rs.Limits.Memory != "" {
		if q, err := resource.ParseQuantity(rs.Limits.Memory); err == nil {
			mem = strconv.FormatInt(q.Value(), 10)
		}
	}
	return cpu, mem
}

// ApprovalPollInterval is how often WaitForApproval polls the controller for a
// human decision. It is an exported var (not a const) so tests — in this
// package and in the k8s agent, which used to own its own unexported
// approvalPollInterval before this loop was shared — can shorten it.
var ApprovalPollInterval = 3 * time.Second

// heartbeatInterval is the interval Run uses when starting the liveness
// heartbeat. It is a var (not a const) so tests can shorten it; production code
// leaves it at DefaultHeartbeatInterval.
var heartbeatInterval = DefaultHeartbeatInterval

// postHookEntry is a post-processing entry executed after a step completes.
// scope carries the owning step's scope handle, if any, so the post script
// runs inside the same isolated environment the step body ran in rather than
// on the host workspace. The scope container is still alive when hookStack is
// drained (see executeRun: hookStack runs before the deferred
// backend.CloseScopes). container carries the step's RunsIn.Container (empty
// for a plain/scoped/image step) so a runsIn.container step's post hook is
// routed into the same named container the step body ran in, mirroring
// pre-refactor k8s routing; the host backend ignores it (a runsIn.container
// step errors on host, so its hook never queues there).
type postHookEntry struct {
	stepName  string
	post      api.PostStep
	scope     ScopeHandle
	container string
}

// Agent represents an agent that communicates with the master server to execute jobs.
type Agent struct {
	ID             string
	Labels         []string // agent labels used for ClaimNextRun filtering
	ExposeEnv      []string
	Client         *Client
	CacheStore     objectstore.ObjectStore // nil = cache disabled
	MaxConcurrent  int
	WorkspaceDir   string
	CleanWorkspace bool
	DrainTimeout   time.Duration

	// RuntimePref selects the container runtime for runsIn.image steps
	// (docker|podman|nerdctl|wslc|container); empty = auto-detect.
	RuntimePref string

	resolvedRuntime crt.ContainerRuntime
	runtimeErr      error
	runtimeOnce     sync.Once
}

// containerRuntime resolves (once) the container runtime for runsIn.image
// steps, honoring RuntimePref. A missing runtime is a hard error surfaced to
// the step (no silent host fallback).
func (a *Agent) containerRuntime() (crt.ContainerRuntime, error) {
	a.runtimeOnce.Do(func() {
		a.resolvedRuntime, a.runtimeErr = crt.Detect(a.RuntimePref)
	})
	return a.resolvedRuntime, a.runtimeErr
}

// New creates a new agent with the given ID and client.
func New(id string, client *Client) *Agent {
	return &Agent{ID: id, Client: client}
}

// NewWithLabels creates a new agent with the given labels.
func NewWithLabels(id string, labels []string, client *Client) *Agent {
	return &Agent{ID: id, Labels: labels, Client: client}
}

// collectEnv collects and returns PATH/PWD/HOME/HOSTNAME and any variables listed in exposeEnv.
func collectEnv(exposeEnv []string) map[string]string {
	keys := append([]string{"PATH", "PWD", "HOME", "HOSTNAME"}, exposeEnv...)
	env := make(map[string]string, len(keys))
	for _, k := range keys {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	return env
}

// Run executes the agent's main loop.
// After registering with the master server, it continuously claims and executes Runs using N goroutines.
// When ctx (claimCtx) is cancelled, new claims are stopped (cordon) and the agent waits for in-flight Runs to complete before exiting (drain).
func (a *Agent) Run(ctx context.Context) error {
	host, _ := os.Hostname()
	req := api.AgentRegisterRequest{
		AgentID:  a.ID,
		Hostname: host,
		OS:       runtime.GOOS,
		Labels:   a.Labels,
		Version:  Version,
		Env:      collectEnv(a.ExposeEnv),
	}
	var registerErr error
	retryUntilSuccess(ctx, func(ctx context.Context) error {
		registerErr = a.Client.Register(ctx, req)
		return registerErr
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if registerErr != nil {
		return registerErr
	}
	slog.Info("agent registered", "agentId", a.ID)

	// Fail runs a previous process incarnation left behind BEFORE claiming
	// anything. A restarted agent re-registers under the same ID and resumes
	// heartbeating immediately, so the stuck-run reaper never sees those runs
	// as orphaned — without this they stay Running forever. Retried until it
	// succeeds: starting to claim with unreconciled orphans would leave the
	// hole open.
	retryUntilSuccess(ctx, func(ctx context.Context) error {
		count, err := a.Client.ReconcileRuns(ctx, a.ID)
		if err != nil {
			return err
		}
		if count > 0 {
			slog.Warn("failed orphaned runs left by previous agent process", "count", count)
		}
		return nil
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}

	n := a.MaxConcurrent
	if n <= 0 {
		n = 1
	}

	wsBase := a.WorkspaceDir
	if wsBase == "" {
		wsBase = "~/workspace"
	}
	var err error
	wsBase, err = expandHome(wsBase)
	if err != nil {
		return err
	}
	for i := 0; i < n; i++ {
		dir := filepath.Join(wsBase, fmt.Sprintf("working%d", i))
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			return fmt.Errorf("create workspace %s: %w", dir, mkErr)
		}
	}

	// runCtx: continues even after claimCtx is cancelled. If DrainTimeout is set,
	// it will be cancelled DrainTimeout after ctx is cancelled.
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	if a.DrainTimeout > 0 {
		go func() {
			<-ctx.Done()
			timer := time.NewTimer(a.DrainTimeout)
			defer timer.Stop()
			select {
			case <-timer.C:
				runCancel()
			case <-runCtx.Done():
			}
		}()
	}

	// Heartbeat is bound to runCtx, NOT claimCtx. claimCtx is cancelled the instant
	// a drain (SIGTERM/cordon) begins, but an in-flight run keeps executing under
	// runCtx for up to DrainTimeout. Binding to claimCtx would stop heartbeats the
	// moment a drain starts, so after staleAfter the stuck-run reaper would Fail a
	// perfectly healthy draining run. runCtx outlives claimCtx during drain and is
	// cancelled on full shutdown (defer runCancel / DrainTimeout), so heartbeats
	// continue through the whole drain window and stop cleanly on shutdown — no leak.
	StartHeartbeat(runCtx, a.Client, a.ID, heartbeatInterval)

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			a.runLoop(ctx, runCtx, slot, wsBase)
		}(i)
	}
	wg.Wait()

	// ctx is already cancelled, so use a new context for deregistration.
	deregCtx, deregCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer deregCancel()
	if err := a.Client.Deregister(deregCtx, a.ID); err != nil {
		slog.Warn("deregister failed", "agentId", a.ID, "error", err)
	} else {
		slog.Info("agent deregistered", "agentId", a.ID)
	}
	return nil
}

// runLoop runs the claim loop for a single slot.
func (a *Agent) runLoop(claimCtx, runCtx context.Context, slot int, wsBase string) {
	workDir := filepath.Join(wsBase, fmt.Sprintf("working%d", slot))
	for {
		if claimCtx.Err() != nil {
			return
		}
		resp, err := a.Client.Claim(claimCtx, a.ID, "30s", a.Labels)
		if err != nil {
			slog.Error("claim", "error", err, "slot", slot)
			select {
			case <-claimCtx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		if resp.RunID == "" {
			continue
		}
		if a.CleanWorkspace {
			if err := os.RemoveAll(workDir); err != nil {
				slog.Warn("clean workspace failed", "dir", workDir, "error", err)
			}
			if err := os.MkdirAll(workDir, 0o755); err != nil {
				slog.Warn("recreate workspace failed", "dir", workDir, "error", err)
			}
		}
		a.executeRun(runCtx, resp, workDir)
	}
}

// executeRun is the host agent's thin wrapper over the shared orchestration
// loop (RunClaim, internal/agent/orchestrator.go): it handles the ONE thing
// only the host agent needs to decide before the shared loop can run — a
// podTemplate-bearing claim requires the k8s agent, so it is rejected here
// rather than being silently mis-executed on the host — then constructs
// hostBackend (the ExecBackend seam for a bare-process claim, rooted at
// workDir) and hands off to RunClaim for everything else (secrets fetch,
// cancellation, step dispatch, finally, output promotion, FinishRun).
func (a *Agent) executeRun(ctx context.Context, c api.ClaimResponse, workDir string) {
	if c.PodTemplate != nil {
		slog.Error("job requires k8s-agent (podTemplate set); this agent cannot execute it", "runId", c.RunID, "job", c.JobName)
		retryUntilSuccess(ctx, func(callCtx context.Context) error {
			return a.Client.FinishRun(callCtx, a.ID, c.RunID, api.RunFailed)
		})
		return
	}

	backend := newHostBackend(a, c.RunID, workDir)
	RunClaim(ctx, a.Client, a.ID, c, backend)
}

// resolveScope returns the ScopeHandle for a scoped step's cache/artifact
// operations, creating the claim's scope container on first use via backend.
// For non-scoped steps it returns the zero ScopeHandle so callers can branch
// on IsZero(). A scoped step that cannot obtain a runtime or container is a
// hard error (no silent fallback to the host workspace).
func resolveScope(ctx context.Context, step api.ClaimStep, backend ExecBackend) (ScopeHandle, error) {
	if !isScopedStep(step) {
		return ScopeHandle{}, nil
	}
	return backend.EnsureScope(ctx, step, nil)
}

// resolveWorkspacePath joins a relative path against the run's workspace working
// directory (the same directory ExecStep/shell steps use as their cwd, e.g.
// "<workspaceDir>/working<N>"). Absolute paths are returned unchanged.
func resolveWorkspacePath(workDir, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(workDir, p)
}

// resolveScopePath joins a relative CONTAINER-side path against scopeWorkDir
// (the scope container's working directory, see scope.go), so it is always
// absolute before being handed to scopeManager.copyIn/copyOut. This mirrors
// the k8s agent's path.Join(mountPath, dest) (internal/k8sagent/agent.go).
// A relative container path passed straight to `docker cp` is rejected
// ("destination path must be absolute") — this is the fix for that class of
// failure. Uses "path" (forward-slash), not "path/filepath": the scope
// container is always Linux regardless of the host OS. Already-absolute
// container paths are returned unchanged.
func resolveScopePath(p string) string {
	if p == "" {
		return scopeWorkDir
	}
	if path.IsAbs(p) {
		return p
	}
	return path.Join(scopeWorkDir, p)
}

// expandHome expands a leading "~/" using os.UserHomeDir().
func expandHome(path string) (string, error) {
	if !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expand ~ in workspace-dir: %w", err)
	}
	return filepath.Join(home, path[2:]), nil
}
