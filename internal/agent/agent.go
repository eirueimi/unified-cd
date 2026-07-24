package agent

import (
	"bytes"
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

	// MinFreeDisk is the minimum free space (in bytes) required on the
	// workspace filesystem for this agent to keep claiming runs. Zero
	// disables the check. Host-only: k8s agents use pod volumes instead.
	MinFreeDisk uint64

	// WorkspaceRetentionDays is the age (in days) after which an inactive
	// per-job workspace directory (working<slot>/<job>) becomes eligible
	// for removal by the opt-in workspace GC (see workspace_gc.go). Zero
	// (the default) disables the GC — persistent workspaces are a feature
	// (inter-run cache), so sweeping them across jobs must be an explicit
	// opt-in. Host-only.
	WorkspaceRetentionDays int

	// freeBytesFn reports available bytes for the filesystem containing the
	// given path. Defaults to freeBytes (the platform-specific syscall
	// implementation); overridable so tests can inject a fake without
	// touching the real filesystem.
	freeBytesFn func(string) (uint64, error)

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
	// workspace-cleanup container). cmd/unified-cd-agent sets this by calling
	// InstallShim before Run, mirroring RequireShell — NOT called from Run
	// itself, so tests that drive Run()/executeRun() directly without a
	// container runtime (native-only claims) are unaffected by whether the
	// committed internal/shim/embedded/ucd-sh-<arch> bytes, generated by `go
	// generate`, populated internal/shim/embedded. Empty means "no shim
	// mount" (see claim_pod.go's ucdToolsMount) — a real deployed agent
	// always has this set.
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
	return &Agent{ID: id, Client: client, freeBytesFn: freeBytes}
}

// NewWithLabels creates a new agent with the given labels.
func NewWithLabels(id string, labels []string, client *Client) *Agent {
	return &Agent{ID: id, Labels: labels, Client: client, freeBytesFn: freeBytes}
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
	// activeRuns tracks the run IDs this process currently has in flight, so
	// the heartbeat below can report them to the controller (foundation for
	// the controller's lost-claim reconcile). Shared across every slot
	// goroutine.
	activeRuns := NewRunSet()

	// activeWorkDirs tracks every working<slot>/<job> directory this process
	// currently has a claim executing in, keyed by the same wsBase-derived
	// path runLoop computes via claimWorkDir — a second, workDir-keyed RunSet
	// enrolled/retired alongside activeRuns (see runLoop). It exists purely to
	// feed gcWorkspaces' active-set parameter; see workspace_gc.go's doc
	// comment for why a workDir path (not a run ID or bare job name) is the
	// safe key. Always constructed (cheap), even when the GC is disabled, so
	// runLoop's signature doesn't need to branch on WorkspaceRetentionDays.
	activeWorkDirs := NewRunSet()

	hbDone := StartHeartbeat(runCtx, a.Client, a.ID, heartbeatInterval, activeRuns.Snapshot)

	// gcDone is closed once workspaceGCLoop has fully stopped, so Run can join
	// it (like hbDone) and guarantee no sweep outlives Run. nil when the GC is
	// disabled — a receive on a nil channel blocks forever, so it is only
	// joined below when non-nil.
	var gcDone <-chan struct{}
	if a.WorkspaceRetentionDays > 0 {
		retention := time.Duration(a.WorkspaceRetentionDays) * 24 * time.Hour
		// Startup sweep: activeWorkDirs is necessarily empty here (nothing has
		// been claimed yet in this process), which is safe — any dir genuinely
		// in use by ANOTHER live agent process sharing this wsBase would have
		// to be younger than retention (days-scale) to survive anyway, since a
		// truly stuck/abandoned dir is exactly what this GC exists to reclaim.
		if removed, err := gcWorkspaces(wsBase, retention, activeWorkDirSet(activeWorkDirs.Snapshot()), time.Now()); err != nil {
			slog.Warn("workspace gc: startup sweep failed", "error", err)
		} else if len(removed) > 0 {
			slog.Info("workspace gc: startup sweep removed aged workspace dirs", "count", len(removed), "dirs", removed)
		}
		done := make(chan struct{})
		gcDone = done
		go func() {
			defer close(done)
			a.workspaceGCLoop(runCtx, wsBase, retention, activeWorkDirs)
		}()
	}

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			a.runLoop(ctx, runCtx, slot, wsBase, activeRuns, activeWorkDirs)
		}(i)
	}
	wg.Wait()

	// All slots are done: stop the heartbeat and GC loop and JOIN them before
	// returning, so neither a beat nor a sweep can outlive Run. runCancel is
	// deferred too, but that only signals the goroutines asynchronously —
	// without these joins a late tick could fire after Run returned (a leak
	// the drain regression test observes).
	runCancel()
	<-hbDone
	if gcDone != nil {
		<-gcDone
	}

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

// runLoop runs the claim loop for a single slot. Both activeRuns (keyed by
// run ID, feeding the heartbeat) and activeWorkDirs (keyed by the claim's
// wsBase-derived workDir path, feeding the workspace GC) are enrolled BEFORE
// prepareWorkspace runs, and both are retired (via defer, LIFO) only after
// executeRun returns. This matters for two independent reasons:
//
//   - activeWorkDirs: prepareWorkspace (and executeRun) populate this
//     directory, and a job resuming after being idle past retention is
//     exactly the dir the periodic sweep would find aged. Enrolling only
//     after prepareWorkspace succeeded would leave a window where a
//     concurrent workspaceGCLoop snapshot could os.RemoveAll a live run's
//     workspace mid-populate (data loss).
//   - activeRuns: the claim already marked the run Running (claimed_at =
//     now) in the DB before runLoop ever sees it. prepareWorkspace can run
//     long (e.g. CleanWorkspace: true doing os.RemoveAll on a multi-GB
//     workspace) — if activeRuns.Add happened only after prepareWorkspace
//     succeeded, the run would sit Running in the DB but absent from this
//     agent's reported active set for that whole window, and the next
//     heartbeat's ListReconcilableRunIDsByAgent (claimed >60s ago, not
//     reported) would incorrectly reconcile-fail a perfectly healthy
//     run that is still preparing.
//
// The claim/prepare path (claimWorkDir, prepareWorkspace) runs with no
// recover of its own — unlike executeRun, which recovers its own panics
// (see its doc comment) — so the closure below wraps it in an outer recover
// that fails only this run via failRun, mirroring k8sagent/agent.go's
// dispatch guard. Because defers run LIFO, that recover is deferred FIRST
// (so it runs LAST): both active-set entries are retired by the defers
// below it before the recover's failRun runs, so a panicking claim is never
// left in either active set.
func (a *Agent) runLoop(claimCtx, runCtx context.Context, slot int, wsBase string, activeRuns, activeWorkDirs *RunSet) {
	for {
		if claimCtx.Err() != nil {
			return
		}
		if a.MinFreeDisk > 0 {
			freeBytesFn := a.freeBytesFn
			if freeBytesFn == nil {
				freeBytesFn = freeBytes
			}
			free, err := freeBytesFn(wsBase)
			if err != nil {
				// Can't determine free space; log and proceed rather than
				// wedging the agent on a stat failure.
				slog.Warn("free disk space check failed, proceeding with claim", "error", err, "slot", slot)
			} else if belowMinFreeDisk(free, a.MinFreeDisk) {
				slog.Warn("free disk space below minimum, skipping claim", "freeBytes", free, "minFreeDisk", a.MinFreeDisk, "slot", slot)
				select {
				case <-claimCtx.Done():
					return
				case <-time.After(2 * time.Second):
				}
				continue
			}
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
		// Protect workDir from the workspace GC BEFORE prepareWorkspace, not
		// after: prepareWorkspace (and executeRun) populate this directory,
		// and a job resuming after being idle past retention is exactly the
		// dir the periodic sweep would find aged. If activeWorkDirs.Add ran
		// only after prepareWorkspace, a concurrent workspaceGCLoop snapshot
		// taken in the window between claimWorkDir and the Add could
		// os.RemoveAll a live run's workspace mid-populate (data loss). Adding
		// here — with the retire deferred immediately — means the dir is
		// covered for the entire lifetime of this claim.
		activeWorkDirs.Add(workDir)
		activeRuns.Add(resp.RunID)
		func() {
			// Recover any panic in the claim/prepare path (executeRun recovers its
			// own). Registered first so it runs LAST — after the Remove defers below
			// retire this claim's active-set entries — then fails only this run
			// instead of crashing the whole agent process (audit item 4 outer guard;
			// mirrors k8sagent/agent.go's dispatch guard).
			defer func() {
				if r := recover(); r != nil {
					slog.Error("panic in claim/prepare path; failing run",
						"runId", resp.RunID, "slot", slot, "panic", r, "stack", string(debug.Stack()))
					// An inner recover so a panic INSIDE failRun (e.g. a nil client)
					// can't re-crash this goroutine and defeat the guard.
					defer func() { _ = recover() }()
					a.failRun(runCtx, resp.RunID, fmt.Sprintf("agent panic before run execution: %v", r))
				}
			}()
			defer activeWorkDirs.Remove(workDir)
			defer activeRuns.Remove(resp.RunID)

			mode := "isolated"
			if resp.Native {
				mode = "native"
			}
			clean := a.CleanWorkspace || (resp.PodTemplate != nil && resp.PodTemplate.CleanWorkspace)
			if err := prepareWorkspaceFn(runCtx, workDir, mode, clean, a.containerRuntime, a.ToolsDir); err != nil {
				// The claim is ours but its workspace could not be prepared, so the
				// run can never start. Fail it on the controller (retried until it
				// lands) rather than leaving it Running until the stuck-run reaper
				// trips — the same failure path executeRun uses.
				a.failRun(runCtx, resp.RunID, fmt.Sprintf("prepare workspace failed: %v", err))
				return
			}
			// Re-verify the ucd-sh shim before each claim: a native step's
			// cwd is inside wsBase, so it can reach toolsDir by relative
			// traversal and tamper with the shell every later containerized
			// job on this agent will run (see EnsureShimIntact's doc
			// comment). Checking here — once per claim, right before the
			// run that just claimed this slot executes — catches tampering
			// left behind by an earlier run and repairs it before the next
			// claim starts, so it cannot persist indefinitely across the
			// agent's lifetime.
			//
			// This does NOT bound tampering to "a single run" when
			// MaxConcurrent > 1: every slot shares this same a.ToolsDir, and
			// the /.ucd mount each claim pod gets is a LIVE bind mount of
			// it, not a copy taken at pod start. A native step here can
			// tamper with the on-disk shim while another slot's isolated
			// job is already running, and that job sees the tampered bytes
			// on its very next ucd-sh invocation — before this per-claim
			// check ever fires again (see EnsureShimIntact's doc comment
			// for the full picture). A check/repair failure is logged and
			// must never fail the run; this is best-effort hardening, not
			// the primary control.
			if a.ToolsDir != "" {
				if repaired, err := EnsureShimIntact(a.ToolsDir); err != nil {
					slog.Error("shim integrity check failed", "error", err, "toolsDir", a.ToolsDir, "runId", resp.RunID)
				} else if repaired {
					slog.Error("ucd-sh shim was modified on disk and has been restored; a previous step on this agent may have tampered with it",
						"toolsDir", a.ToolsDir, "runId", resp.RunID)
				}
			}
			a.executeRun(runCtx, resp, workDir)
		}()
	}
}

// workspaceGCInterval is the periodic sweep cadence for the opt-in workspace
// GC (see workspaceGCLoop). It mirrors the k8s agent's runPodGC in shape (a
// ticker-driven periodic sweep started alongside the startup one) but on a
// much coarser cadence: runPodGC deals with short-lived per-run pods and
// needs a tight minute-scale loop, whereas workspace retention is
// expressed and reasoned about in whole days (WorkspaceRetentionDays), so
// hourly is frequent enough to reclaim space promptly without adding
// meaningful overhead. A var (not a const) only so tests can shorten it; no
// production path mutates it.
var workspaceGCInterval = time.Hour

// workspaceGCLoop periodically sweeps aged, inactive per-job workspace
// directories under wsBase (see gcWorkspaces) until ctx is done. Only
// started by Run when WorkspaceRetentionDays > 0. activeWorkDirs is
// snapshotted fresh on every tick so the sweep always sees the
// currently-in-flight set of claims across all slots.
func (a *Agent) workspaceGCLoop(ctx context.Context, wsBase string, retention time.Duration, activeWorkDirs *RunSet) {
	ticker := time.NewTicker(workspaceGCInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		removed, err := gcWorkspaces(wsBase, retention, activeWorkDirSet(activeWorkDirs.Snapshot()), time.Now())
		if err != nil {
			slog.Warn("workspace gc: sweep failed", "error", err)
			continue
		}
		if len(removed) > 0 {
			slog.Info("workspace gc: removed aged workspace dirs", "count", len(removed), "dirs", removed)
		}
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
			// An inner recover so a panic INSIDE failRun (e.g. a nil client)
			// can't re-crash this goroutine and defeat the guard.
			defer func() { _ = recover() }()
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
		// The claim pod needs a pause image to hold the netns. cmd/unified-cd-agent
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
	backend := newHostBackend(a, c.RunID, c.JobName, workDir, pod)
	runClaimFn(ctx, a.Client, a.ID, c, backend)
}

// runClaimFn indirects the call to RunClaim so tests can substitute a stub
// that panics, exercising executeRun's outer panic-recovery guard above
// without needing a genuine crash somewhere in the real orchestration path.
// Production code always leaves this as RunClaim.
var runClaimFn = RunClaim

// prepareWorkspaceFn indirects the call to prepareWorkspace so tests can
// substitute a stub that panics, exercising runLoop's claim/prepare-path
// panic-recovery guard (see runLoop's doc comment) without needing a genuine
// crash somewhere in the real workspace-prep code. Mirrors runClaimFn above.
// Production code always leaves this as prepareWorkspace.
var prepareWorkspaceFn = prepareWorkspace

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
// non-empty payload without needing the committed
// internal/shim/embedded/ucd-sh-<arch> bytes, generated by `go generate`, to
// be populated first.
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
// Called once at startup by cmd/unified-cd-agent's main(), mirroring RequireShell —
// deliberately NOT from Agent.Run, so unit tests driving Run()/executeRun()
// directly (native claims, or isolated claims against a fake runtime) are
// unaffected by whether internal/shim/embedded holds the committed
// internal/shim/embedded/ucd-sh-<arch> bytes, generated by `go generate`
// (see that package's doc comment).
//
// A zero-length shimBytes() is a hard, actionable error: since the default
// container step shell is now /.ucd/ucd-sh -c and every keep-alive is
// /.ucd/ucd-sh pause, an agent that starts without the shim would fail every
// isolated job's first exec with an opaque "no such file" — better to refuse
// to start and name the fix.
func InstallShim(workspaceDir string) (toolsDir string, err error) {
	payload := shimBytes()
	if len(payload) == 0 {
		return "", fmt.Errorf("ucd-sh shim is not embedded in this agent binary (0 bytes): the committed internal/shim/embedded/ucd-sh-<arch> is missing or empty — run `go generate ./internal/shim/embedded/` and rebuild before starting the agent")
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
	// Write via a temp file + atomic rename rather than an in-place
	// os.WriteFile: EnsureShimIntact tightens an installed shim to 0o555
	// (read-only) on every tamper check, so on a re-run the existing ucd-sh
	// cannot be reopened for writing (EACCES / "Access is denied"). The rename
	// only needs toolsDir writable and also sidesteps ETXTBSY against an
	// already-executing shim.
	if err := writeShimAtomic(toolsDir, shimPath, payload, 0o755); err != nil {
		return "", err
	}
	return toolsDir, nil
}

// shimPayload returns the exact embedded ucd-sh bytes InstallShim writes,
// via the same shimBytes indirection (so tests can fake it) — the single
// source of truth both InstallShim and EnsureShimIntact read from, so the
// two can never drift apart. shimBytes itself already resolves to the
// architecture this agent binary was compiled for (see
// internal/shim/embedded: embed_amd64.go / embed_arm64.go pick the payload
// via build tags at compile time), so no arch selection happens here.
func shimPayload() []byte {
	return shimBytes()
}

// EnsureShimIntact re-verifies the on-disk ucd-sh shim at
// <toolsDir>/ucd-sh against the embedded payload InstallShim writes, and
// repairs it if the two differ or the file is missing, reporting whether a
// repair was needed.
//
// Why this exists: ucd-sh is the default shell and keep-alive entrypoint for
// every containerized job step (see InstallShim's doc comment on the
// shared-mount invariant that requires toolsDir to live under wsBase). A
// NATIVE step runs as the agent user with its cwd inside wsBase, so it can
// reach toolsDir by relative traversal (e.g. `cp evil ../../.ucd-tools/ucd-sh`)
// and backdoor the shell of every containerized job that later runs on this
// agent — jobs that specifically chose container isolation to avoid the
// host. The control is repair-on-next-claim: re-verifying before EACH claim
// (see the call in runLoop) detects tampering left by an earlier run and
// repairs it via an atomic same-directory rename (see below) before the next
// claim starts, so tampering cannot persist indefinitely across the agent's
// lifetime.
//
// Repair writes the replacement payload to a temp file in toolsDir (the same
// directory as the shim, so the following rename is same-filesystem and
// therefore atomic) and renames it over the shim path, rather than
// os.WriteFile(shimPath, ...) truncating the existing file in place. Two
// reasons:
//
//  1. ETXTBSY: on Linux, opening a binary for writing while ANY process is
//     currently executing it fails with ETXTBSY. With MaxConcurrent > 1,
//     another slot's claim pod holds this exact shim open via the live
//     /.ucd bind mount (its pause keep-alive, or a step exec) for the pod's
//     whole lifetime — so exactly when repair is most needed (concurrent
//     slots, one just tampered), an in-place O_TRUNC write would fail with
//     "text file busy". Rename instead swaps the directory entry: it does
//     not require the target inode to be unreferenced, so it succeeds
//     regardless of who currently has the old file open for exec.
//  2. Torn writes: two slots repairing concurrently against the same
//     O_TRUNC target could interleave, briefly exposing a partial binary
//     (ENOEXEC or exit 127) through the live mount. A rename is a single
//     atomic directory-entry swap, so a concurrent reader always sees either
//     the fully-old or fully-new file, never a partial one.
//
// What this does NOT give: already-running containers keep executing the
// OLD inode until they restart. Rename does not — cannot — reach into a
// process that already has the old file mapped/open; POSIX only guarantees
// the directory entry updates atomically, not that existing file handles are
// invalidated. That is inherent to repairing a live bind mount, not a defect
// here: a native step in one slot that tampers with the on-disk shim mid-run
// is still immediately visible to an isolated job already running in
// another slot, on that job's very next ucd-sh invocation, and this check
// only runs at the START of a new claim, never against claims already in
// flight. Closing that window needs a check independent of claim cadence
// (e.g. periodic re-verification, or verifying before each container-step
// exec rather than each claim); see the design doc's residual/follow-up
// note.
//
// This is best-effort hardening, not the primary control: a failure to read
// or repair the shim is reported to the caller but must never fail the run
// (see runLoop, which logs and continues on error). This function also does
// no locking of its own and, with MaxConcurrent > 1, is now called
// concurrently from every slot against the same file; that is currently
// benign only because every caller writes the identical embedded payload,
// and the rename-based repair below is safe against concurrent repairers by
// construction (each writes its own temp file; the last rename to land
// wins, and it lands the same bytes every other caller would have written).
func EnsureShimIntact(toolsDir string) (repaired bool, err error) {
	shimPath := filepath.Join(toolsDir, "ucd-sh")
	want := shimPayload()
	if len(want) == 0 {
		// A working, intact shim on disk must never be classified "not
		// intact" and truncated to zero bytes because the embedded payload
		// this process was built with is itself empty — that would turn a
		// functioning agent into one that repairs every shim into garbage on
		// its very next claim. InstallShim already refuses to start in this
		// case; this is defense in depth for any path that reaches
		// EnsureShimIntact without having gone through InstallShim first.
		return false, fmt.Errorf("ucd-sh shim payload is empty (0 bytes): refusing to repair %s — this agent binary was built with an empty internal/shim/embedded/ucd-sh (run `go generate ./internal/shim/embedded/` and rebuild)", shimPath)
	}
	got, readErr := os.ReadFile(shimPath)
	intact := readErr == nil && bytes.Equal(got, want)
	if readErr != nil && !os.IsNotExist(readErr) {
		return false, fmt.Errorf("read shim %s: %w", shimPath, readErr)
	}
	if !intact {
		if err := repairShim(toolsDir, shimPath, want); err != nil {
			return false, err
		}
	}
	// Tighten to read-only where the platform allows, on every check (not
	// just on repair): repairShim below already writes the temp file at
	// 0o555 before the rename, so this is a no-op immediately after a
	// repair and only matters if something else loosened the mode since.
	// Not the primary control — the agent user may own the file regardless,
	// so this is defense-in-depth — and a failure here is best-effort and
	// never fatal.
	_ = os.Chmod(shimPath, 0o555)
	return !intact, nil
}

// repairShim writes want to a fresh temp file created in toolsDir (same
// directory as shimPath, so the rename below is guaranteed same-filesystem)
// and atomically renames it over shimPath. See EnsureShimIntact's doc
// comment for why this replaces a straightforward os.WriteFile(shimPath,
// ...): it sidesteps ETXTBSY against an already-executing shim and closes
// the torn-write window between concurrent repairers. The temp file is
// cleaned up on every error path so a failed repair never leaves debris in
// toolsDir.
func repairShim(toolsDir, shimPath string, want []byte) error {
	if err := writeShimAtomic(toolsDir, shimPath, want, 0o555); err != nil {
		return fmt.Errorf("repair shim %s: %w", shimPath, err)
	}
	return nil
}

// writeShimAtomic writes want to a fresh temp file in toolsDir, sets its final
// mode, and atomically renames it over shimPath. Both InstallShim and
// repairShim go through it so they can never drift: the rename replaces
// shimPath regardless of the existing file's mode (a shim EnsureShimIntact has
// tightened to 0o555 would block an in-place write), needs only toolsDir
// writable, and sidesteps ETXTBSY against an already-executing shim. The mode
// is set on the temp file BEFORE the rename so the file that lands at shimPath
// already has its intended permissions the instant it appears — no window where
// it is briefly more permissive. The temp file is cleaned up on every error
// path so a failed write never leaves debris in toolsDir.
func writeShimAtomic(toolsDir, shimPath string, want []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(toolsDir, "ucd-sh.tmp*")
	if err != nil {
		return fmt.Errorf("create temp shim in %s: %w", toolsDir, err)
	}
	tmpPath := tmp.Name()
	succeeded := false
	defer func() {
		if !succeeded {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(want); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp shim %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp shim %s: %w", tmpPath, err)
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		return fmt.Errorf("chmod temp shim %s: %w", tmpPath, err)
	}
	if err := renameOverExisting(tmpPath, shimPath); err != nil {
		return fmt.Errorf("write shim %s: %w", shimPath, err)
	}
	succeeded = true
	return nil
}

// renameOverExisting renames src to dst, working around a Windows-specific
// gap: POSIX rename(2) always atomically replaces an existing dst
// regardless of open readers/executors (the exact property repairShim
// relies on to avoid ETXTBSY), but os.Rename on Windows can fail with the
// destination already present — e.g. if some other handle has it open
// non-shared. Try the direct rename first (this is the common, and on
// POSIX the only, path); only on Windows, if that fails, fall back to
// removing the destination and retrying. That fallback is best-effort and
// no longer atomic against a concurrent reader — but it only runs on a
// platform where the atomic path already isn't guaranteed by the OS, so it
// makes the best of what Windows offers rather than regressing anything.
func renameOverExisting(src, dst string) error {
	if err := os.Rename(src, dst); err != nil {
		if runtime.GOOS != "windows" {
			return err
		}
		if rmErr := os.Remove(dst); rmErr != nil && !os.IsNotExist(rmErr) {
			return err
		}
		if err := os.Rename(src, dst); err != nil {
			return err
		}
	}
	return nil
}
