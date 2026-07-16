package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
	"github.com/eirueimi/unified-cd/internal/shim/embedded"
)

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
// step errors on host, so its hook never queues there). stepIndex carries the
// owning step's ClaimStep.Index, so the hookStack drain can open log writers
// (ExecBackend.StepLogWriters) attributed to that step: a post hook's
// stdout/stderr is shipped as more output appended to the OWNING step's log,
// not a separate pseudo-step, since post: is documented as cleanup for that
// step rather than an independent unit of work. shell is the hook's
// effective interpreter argv, resolved once when the entry is appended (see
// makeStepRunner in orchestrator.go): post.Shell if the post: hook declared
// its own, else the owning step's effective ClaimStep.Shell — nil/empty
// means "apply the shim default" at exec time, same as every step.
type postHookEntry struct {
	stepName  string
	post      api.PostStep
	scope     ScopeHandle
	container string
	stepIndex int
	shell     []string
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

	// PauseImage / RunnerImage back isolated claims' pods: PauseImage holds
	// the claim netns; RunnerImage is the injected "job" primary when the
	// podTemplate defines none (host twin of the k8s fallback image).
	PauseImage  string
	RunnerImage string

	// ToolsDir is the host directory holding the embedded ucd-sh shim
	// (written by InstallShim, INSIDE the workspace base — see that func's
	// doc comment for why it must not be a sibling of the workspace dir),
	// bind-mounted read-only at /.ucd into every container this agent
	// creates (claim-pod containers, uses-scope containers, the
	// workspace-cleanup container). cmd/agent sets this by calling
	// InstallShim before Run, mirroring RequireShell — NOT called from Run
	// itself, so tests that drive Run()/executeRun() directly without a
	// container runtime (native-only claims) are unaffected by whether the
	// two-stage build populated internal/shim/embedded. Empty means "no
	// shim mount" (see claim_pod.go's ucdToolsMount) — a real deployed
	// agent always has this set.
	ToolsDir string

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

// agentCapabilities reports what this standard agent can execute: always
// native (host process), plus container when a container runtime is present.
func agentCapabilities(runtimeAvailable bool) []string {
	caps := []string{dsl.CapNative}
	if runtimeAvailable {
		caps = append(caps, dsl.CapContainer)
	}
	return caps
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
	_, rtErr := a.containerRuntime()
	runtimeAvailable := rtErr == nil
	req := api.AgentRegisterRequest{
		AgentID:      a.ID,
		Hostname:     host,
		OS:           runtime.GOOS,
		Labels:       a.Labels,
		Version:      Version,
		Env:          collectEnv(a.ExposeEnv),
		Capabilities: agentCapabilities(runtimeAvailable),
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
	hbDone := StartHeartbeat(runCtx, a.Client, a.ID, heartbeatInterval)

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			a.runLoop(ctx, runCtx, slot, wsBase)
		}(i)
	}
	wg.Wait()

	// All slots are done: stop the heartbeat and JOIN it before returning, so a
	// beat can't outlive Run. runCancel is deferred too, but that only signals
	// the goroutine asynchronously — without this join a late tick could fire
	// after Run returned (a leak the drain regression test observes).
	runCancel()
	<-hbDone

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
		workDir := claimWorkDir(wsBase, slot, resp.JobName)
		mode := "isolated"
		if resp.Native {
			mode = "native"
		}
		clean := a.CleanWorkspace || (resp.PodTemplate != nil && resp.PodTemplate.CleanWorkspace)
		if err := prepareWorkspace(runCtx, workDir, mode, clean, a.containerRuntime, a.ToolsDir); err != nil {
			// The claim is ours but its workspace could not be prepared, so the
			// run can never start. Fail it on the controller (retried until it
			// lands) rather than leaving it Running until the stuck-run reaper
			// trips — the same failure path executeRun uses.
			a.failRun(runCtx, resp.RunID, fmt.Sprintf("prepare workspace failed: %v", err))
			continue
		}
		a.executeRun(runCtx, resp, workDir)
	}
}

// executeRun is the host agent's thin wrapper over the shared orchestration
// loop (RunClaim, internal/agent/orchestrator.go). It branches on the claim
// kind before handing off:
//
//   - native (c.Native): no pod is built; the backend runs default steps as
//     host processes exactly as before.
//   - isolated: it resolves the container runtime and eagerly builds the claim
//     pod (pause netns owner + every podTemplate container + the injected
//     "job" primary), which becomes the execution target for every default and
//     container: step. A missing runtime or a failed pod build fails the run
//     fast (the isolated job cannot run without its pod).
//
// Everything else — secrets fetch, cancellation, step dispatch, finally,
// output promotion, FinishRun — is delegated to RunClaim via hostBackend.
//
// The whole body is wrapped in a recover: runOne (pipeline.go) already turns
// a panic INSIDE a step into a normal step failure, but a panic ABOVE the
// step level (claim-pod construction, backend wiring, or anywhere else in
// this function's call graph) would otherwise crash the process and leave
// every other in-flight run stuck. This defense-in-depth guard converts such
// a panic into the same failRun path executeRun already uses for its other
// unrecoverable-claim errors, so the run is marked Failed instead of hanging
// as Running until the stuck-run reaper trips.
func (a *Agent) executeRun(ctx context.Context, c api.ClaimResponse, workDir string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("agent panic in executeRun", "runId", c.RunID, "panic", r, "stack", string(debug.Stack()))
			a.failRun(ctx, c.RunID, fmt.Sprintf("agent panic: %v", r))
		}
	}()

	failClaim := func(msg string, err error) {
		a.failRun(ctx, c.RunID, fmt.Sprintf("%s: %v", msg, err))
	}

	var pod *claimPodManager
	if !c.Native {
		rt, err := a.containerRuntime()
		if err != nil {
			failClaim("isolated job requires a container runtime (docker/podman/nerdctl); mark the job native: true or route it via agentSelector", err)
			return
		}
		// The claim pod needs a pause image to hold the netns. cmd/agent
		// defaults it, but an Agent built directly (agent.New) leaves it empty;
		// starting a container with an empty image only surfaces as a cryptic
		// `docker run -d: exit status 125`, so fail fast with an actionable
		// reason instead.
		if a.PauseImage == "" {
			a.failRun(ctx, c.RunID, "isolated job requires the agent's pause image to be configured (set --pause-image / config pauseImage) or mark the job native: true")
			return
		}
		// The injected primary "job" container is backed by RunnerImage — but
		// only when the podTemplate doesn't supply its own "job" container.
		// Guard the empty-image case with the same actionable reason.
		if a.RunnerImage == "" && claimNeedsRunnerImage(c.PodTemplate) {
			a.failRun(ctx, c.RunID, `isolated job requires the agent's runner image to be configured (set --runner-image / config runnerImage), or supply a "job" container in the podTemplate, or mark the job native: true`)
			return
		}
		pod = newClaimPodManager(rt, workDir, hostNamedMountPath(c.PodTemplate), a.PauseImage, a.RunnerImage, a.ToolsDir)
		if err := pod.Start(ctx, c.PodTemplate); err != nil {
			pod.CloseAll(context.WithoutCancel(ctx))
			failClaim("claim pod construction failed", err)
			return
		}
	}
	backend := newHostBackend(a, c.RunID, workDir, pod)
	runClaimFn(ctx, a.Client, a.ID, c, backend)
}

// runClaimFn indirects the call to RunClaim so tests can substitute a stub
// that panics, exercising executeRun's outer panic-recovery guard above
// without needing a genuine crash somewhere in the real orchestration path.
// Production code always leaves this as RunClaim.
var runClaimFn = RunClaim

// failRun fails a claim that could not even begin executing (workspace
// preparation failed, the isolated job's container runtime is missing, or its
// claim pod failed to start). reason is surfaced into the run's own logs
// (stepIndex -1, rendered as "System" in the UI) via AppendLogBulk — the same
// mechanism the orchestrator's warnSkippedOutput uses (see
// internal/agent/orchestrator.go) — before FinishRun(Failed), so the actual
// cause isn't limited to the agent's local slog. Both calls are best-effort /
// retried-until-success respectively: the log line is fire-and-forget (a
// missing system log line must not block failing the run), while FinishRun is
// retried until it lands so the run never sits stuck as Running.
func (a *Agent) failRun(ctx context.Context, runID, reason string) {
	slog.Error(reason, "runId", runID)
	_ = a.Client.AppendLogBulk(ctx, a.ID, runID, -1, []api.LogAppendRequest{{
		RunID:     runID,
		StepIndex: -1,
		Stream:    "stderr",
		Timestamp: time.Now().UTC(),
		Line:      reason,
	}})
	retryUntilSuccess(ctx, func(cc context.Context) error {
		return a.Client.FinishRun(cc, a.ID, runID, api.RunFailed)
	})
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
// "<workspaceDir>/working<N>"). An absolute or escaping path is rejected.
func resolveWorkspacePath(workDir, p string) (string, error) {
	return ContainWithinOS(workDir, p)
}

// resolveScopePath joins a relative CONTAINER-side path against scopeWorkDir
// (the scope container's working directory, see scope.go), so it is always
// absolute before being handed to scopeManager.copyIn/copyOut. This mirrors
// the k8s agent's path.Join(mountPath, dest) (internal/k8sagent/agent.go).
// A relative container path passed straight to `docker cp` is rejected
// ("destination path must be absolute") — this is the fix for that class of
// failure. Uses forward-slash joining (ContainWithinSlash), not
// "path/filepath": the scope container is always Linux regardless of the
// host OS. An absolute or escaping container path is rejected.
func resolveScopePath(p string) (string, error) {
	return ContainWithinSlash(scopeWorkDir, p)
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

// shimBytes is indirected for testability: InstallShim's unit tests fake a
// non-empty payload without needing a real two-stage build (`make
// embed-shim`) to have populated internal/shim/embedded first.
var shimBytes = embedded.Bytes

// InstallShim writes the embedded ucd-sh binary into a tools directory
// derived from workspaceDir (the same "~/workspace" default/expansion Run
// applies to a.WorkspaceDir — see expandHome): a dot-prefixed subdirectory
// INSIDE it, <expanded workspaceDir>/.ucd-tools/ucd-sh, mode 0755. It returns
// the tools directory so the caller can set Agent.ToolsDir, which every
// container-creating path (claim_pod.go, scope.go, workspace.go) reads to
// bind-mount /.ucd read-only.
//
// toolsDir MUST live under wsBase, not beside it. This agent's container
// runtime (crt.ContainerRuntime) may be a REMOTE docker daemon (e.g.
// DOCKER_HOST=tcp://...) whose filesystem is not the agent's — the only
// reason any bind mount this agent creates works at all is that wsBase is a
// volume/mount SHARED between the agent and that daemon (the same invariant
// the per-claim workspace bind mount relies on). A toolsDir computed as a
// sibling of wsBase (e.g. filepath.Dir(wsBase)/tools) is only guaranteed
// visible to the agent's own filesystem; against a remote daemon it silently
// bind-mounts an empty directory at /.ucd (no error — docker happily creates
// the missing host path), and every container's ucd-sh keep-alive/entrypoint
// then fails with "exit status 127" (binary not found). Placing toolsDir
// inside wsBase inherits the same shared-mount guarantee the workspace
// itself depends on. The ".ucd-tools" name is dot-prefixed so it can never
// collide with a job workspace directory (claimWorkDir only ever creates
// "working<slot>/<job>" segments directly under wsBase) and is never swept
// by prepareWorkspace's cleanup, which only os.RemoveAll's a specific
// per-job workDir, never wsBase itself (see workspace.go).
//
// Called once at startup by cmd/agent's main(), mirroring RequireShell —
// deliberately NOT from Agent.Run, so unit tests driving Run()/executeRun()
// directly (native claims, or isolated claims against a fake runtime) are
// unaffected by whether internal/shim/embedded holds a real binary or the
// committed zero-byte placeholder (see that package's doc comment).
//
// A zero-length shimBytes() is a hard, actionable error: since the default
// container step shell is now /.ucd/ucd-sh -c and every keep-alive is
// /.ucd/ucd-sh pause, an agent that starts without the shim would fail every
// isolated job's first exec with an opaque "no such file" — better to refuse
// to start and name the fix.
func InstallShim(workspaceDir string) (toolsDir string, err error) {
	payload := shimBytes()
	if len(payload) == 0 {
		return "", fmt.Errorf("ucd-sh shim is not embedded in this agent binary (0 bytes): build with `make embed-shim` (or `make build`), or run scripts/build-shims.sh, before starting the agent")
	}
	wsBase := workspaceDir
	if wsBase == "" {
		wsBase = "~/workspace"
	}
	wsBase, err = expandHome(wsBase)
	if err != nil {
		return "", err
	}
	toolsDir = filepath.Join(wsBase, ".ucd-tools")
	if err := os.MkdirAll(toolsDir, 0o755); err != nil {
		return "", fmt.Errorf("create tools dir %s: %w", toolsDir, err)
	}
	shimPath := filepath.Join(toolsDir, "ucd-sh")
	if err := os.WriteFile(shimPath, payload, 0o755); err != nil {
		return "", fmt.Errorf("write shim to %s: %w", shimPath, err)
	}
	return toolsDir, nil
}
