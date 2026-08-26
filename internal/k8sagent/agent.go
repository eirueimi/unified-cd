package k8sagent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// logAutoFlushInterval is how often a step's stdout/stderr LogPushers (and a
// user sidecar's log pump) are flushed while still running, so sparse output
// appears in the WebUI before the step/pump completes (mirrors the host
// agent's streaming behavior). Named for what it governs now that stdout has
// its own LogPusher too — it used to be stderr-only, back when stdout shipped
// synchronously per line and needed no flush timer of its own. It is a var
// (not a const) so tests can shorten it.
var logAutoFlushInterval = 2 * time.Second

// podManager and stepExecutor are the narrow slices of *PodManager / *Executor
// that K8sAgent depends on. Interfaces (satisfied by the concrete types) make
// pod-lifecycle and exec paths unit-testable with fakes.
type podManager interface {
	CreatePod(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error)
	WaitForPodRunning(ctx context.Context, name string) error
	DeletePod(ctx context.Context, name string) error
	ListPods(ctx context.Context, labelSelector string) (*corev1.PodList, error)
}

type stepExecutor interface {
	ExecStep(ctx context.Context, podName, container, script string, shell []string, env []string, stdout, stderr io.Writer) (int, error)
	ExecStepArgv(ctx context.Context, podName, container string, argv []string, stdout, stderr io.Writer) (int, error)
}

// K8sAgent is an agent that claims Runs from the master and executes them inside a Kubernetes Pod.
type K8sAgent struct {
	cfg    Config
	client *agentlib.Client
	pm     podManager
	exec   stepExecutor
	pool   *PodPool
	// dispatch executes one claimed run. Defaults to executeRun; overridable in
	// tests to exercise the claim loop's drain/concurrency without a pod backend.
	dispatch func(ctx context.Context, c api.ClaimResponse)
}

// k8sAgentCapabilities reports what the k8s agent can execute: it always
// builds a Kubernetes Pod, and that pod always has a runnable container.
func k8sAgentCapabilities() []string { return []string{dsl.CapPod, dsl.CapContainer} }

// NewK8sAgent creates a new K8sAgent.
func NewK8sAgent(cfg Config, agentClient *agentlib.Client, pm *PodManager, exec *Executor, pool *PodPool) *K8sAgent {
	a := &K8sAgent{cfg: cfg, client: agentClient, pm: pm, exec: exec, pool: pool}
	a.dispatch = a.executeRun
	return a
}

// Run executes the agent's main loop.
// After registering with the master server, it continuously claims and executes Runs.
// Continues until the context is cancelled.
func (a *K8sAgent) Run(ctx context.Context) error {
	host, _ := os.Hostname()
	labels := appendLabelIfMissing(a.cfg.Labels, "kubernetes")
	if err := a.client.Register(ctx, api.AgentRegisterRequest{
		AgentID:  a.cfg.AgentID,
		Hostname: host,
		OS:       runtime.GOOS + "/k8s",
		Labels:   labels,
		// Same build variable the host agent reports (both binaries are
		// stamped from the same release tag), so `GET /api/v1/agents` shows a
		// version for k8s agents too instead of an empty string.
		Version:      agentlib.Version,
		Capabilities: k8sAgentCapabilities(),
	}); err != nil {
		return err
	}

	if err := a.pool.Restore(ctx, a.client); err != nil {
		slog.Warn("k8s: pool restore failed, continuing without pool", "error", err)
	}
	slog.Info("k8s agent registered", "agentId", a.cfg.AgentID, "labels", labels)

	// Fail runs a previous process incarnation left behind BEFORE claiming
	// anything (e.g. the Deployment's pod was replaced mid-run): the restarted
	// agent re-registers under the same ID and resumes heartbeating, so the
	// stuck-run reaper never sees those runs as orphaned. Retried until it
	// succeeds — claiming with unreconciled orphans would leave them Running
	// forever, and failing fatally would just crash-loop the pod.
	for {
		count, err := a.client.ReconcileRuns(ctx, a.cfg.AgentID)
		if err == nil {
			if count > 0 {
				slog.Warn("k8s: failed orphaned runs left by previous agent process", "count", count)
			}
			break
		}
		slog.Warn("k8s: reconcile orphaned runs failed; retrying", "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}

	// ctx is the claim context: cancelled on shutdown to stop new claims. runCtx
	// outlives it so in-flight runs can drain; DrainTimeout (0 = wait forever)
	// bounds the drain window. Mirrors the host agent (internal/agent/agent.go).
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	if d := a.cfg.DrainTimeoutDuration(); d > 0 {
		go func() {
			<-ctx.Done()
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-timer.C:
				runCancel()
			case <-runCtx.Done():
			}
		}()
	}

	// activeRuns tracks the run IDs this process currently has in flight, so
	// the heartbeat below can report them to the controller (foundation for
	// the controller's lost-claim reconcile). Shared across every dispatch
	// goroutine below.
	activeRuns := agentlib.NewRunSet()

	// Heartbeat bound to runCtx (not ctx): a drain must not stop heartbeats, or
	// the stuck-run reaper would fail a healthy draining run after staleAfter.
	// Joined before Run returns so no beat outlives Run.
	hbDone := agentlib.StartHeartbeat(runCtx, a.client, a.cfg.AgentID, agentlib.DefaultHeartbeatInterval, activeRuns.Snapshot)
	go a.runPodGC(runCtx, time.Minute)

	// Concurrency gates: MaxConcurrent for normal runs; a SEPARATE
	// MaxDetachedConcurrent pool for detached (spec.detached) runs so a detached
	// orchestrator's call: wait never holds a normal semaphore token. 0/unset
	// defaults to 16 (detached claimable out of the box); a negative value
	// disables the pool.
	// (positive MaxConcurrent -> semaphore; negative -> unlimited; Validate 0->100.)
	var sem chan struct{}
	if a.cfg.MaxConcurrent > 0 {
		sem = make(chan struct{}, a.cfg.MaxConcurrent)
	}
	dmax := a.cfg.MaxDetachedConcurrent
	if dmax == 0 {
		dmax = 16
	}
	if dmax < 0 {
		dmax = 0
	}
	var detSem chan struct{}
	if dmax > 0 {
		detSem = make(chan struct{}, dmax)
	}

	var wg sync.WaitGroup     // in-flight dispatched runs
	var loopWG sync.WaitGroup // the claim loops themselves
	loopWG.Add(1)
	go func() {
		defer loopWG.Done()
		a.k8sClaimLoop(ctx, runCtx, labels, sem, &wg, activeRuns, false)
	}()
	if dmax > 0 {
		loopWG.Add(1)
		go func() {
			defer loopWG.Done()
			a.k8sClaimLoop(ctx, runCtx, labels, detSem, &wg, activeRuns, true)
		}()
	}
	loopWG.Wait()

	// Stop claiming; wait for in-flight runs to drain (bounded by DrainTimeout),
	// then stop and join the heartbeat before returning.
	wg.Wait()
	runCancel()
	<-hbDone

	// ctx is cancelled; deregister on a fresh context so the master drops us
	// immediately instead of waiting for heartbeat staleness.
	deregCtx, deregCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer deregCancel()
	if err := a.client.Deregister(deregCtx, a.cfg.AgentID); err != nil {
		slog.Warn("k8s: deregister failed", "agentId", a.cfg.AgentID, "error", err)
	} else {
		slog.Info("k8s agent deregistered", "agentId", a.cfg.AgentID)
	}
	return ctx.Err()
}

// k8sClaimLoop claims runs for one pool until ctx is cancelled. sem gates
// concurrency (nil = ungated); detached selects the claim kind (ClaimDetached vs
// Claim) so the normal and detached pools draw from independent semaphores. wg
// tracks in-flight dispatched runs (shared across both pools so drain waits for
// all), and activeRuns feeds the heartbeat.
func (a *K8sAgent) k8sClaimLoop(ctx, runCtx context.Context, labels []string, sem chan struct{}, wg *sync.WaitGroup, activeRuns *agentlib.RunSet, detached bool) {
	for {
		if ctx.Err() != nil {
			return
		}
		if sem != nil {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
		}
		var resp api.ClaimResponse
		var err error
		if detached {
			resp, err = a.client.ClaimDetached(ctx, a.cfg.AgentID, "30s", labels)
		} else {
			resp, err = a.client.Claim(ctx, a.cfg.AgentID, "30s", labels)
		}
		if err != nil {
			if sem != nil {
				<-sem
			}
			slog.Error("claim error", "error", err, "detached", detached)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		if resp.RunID == "" {
			if sem != nil {
				<-sem
			}
			continue
		}
		wg.Add(1)
		go func(c api.ClaimResponse) {
			defer wg.Done()
			if sem != nil {
				defer func() { <-sem }()
			}
			// Defense-in-depth: a.dispatch has its own error handling, but a panic
			// anywhere in that call graph would otherwise crash the whole agent
			// process and take every other in-flight run down with it. Recover here
			// and fail just this run (mirrors the host agent's executeRun guard).
			defer func() {
				if r := recover(); r != nil {
					slog.Error("k8s: agent panic in dispatch", "runId", c.RunID, "panic", r, "stack", string(debug.Stack()))
					// An inner recover so a panic INSIDE failRun can't re-crash this goroutine.
					defer func() { _ = recover() }()
					a.failRun(runCtx, c.RunID, fmt.Sprintf("agent panic: %v", r))
				}
			}()
			// Enrolled/retired around dispatch so the heartbeat reports this run as
			// active for its whole execution (defers run LIFO regardless of outcome).
			activeRuns.Add(c.RunID)
			defer activeRuns.Remove(c.RunID)
			a.dispatch(runCtx, c)
		}(resp)
	}
}

// executeRun is the k8s agent's thin wrapper over the shared orchestration
// loop (agentlib.RunClaim, internal/agent/orchestrator.go): it handles the
// things only the k8s agent needs to decide before the shared loop can run —
// acquiring (or building) this claim's Pod, waiting for it to be Running, and
// clearing the workspace for a reused pooled pod — then constructs
// k8sBackend (the ExecBackend seam for this pod) and hands off to RunClaim
// for everything else (secrets fetch, cancellation, step dispatch, finally,
// output promotion, FinishRun).
func (a *K8sAgent) executeRun(ctx context.Context, c api.ClaimResponse) {
	slog.Info("k8s: executing Run", "runId", c.RunID, "job", c.JobName)

	if c.Native {
		a.failRun(ctx, c.RunID, "native: true jobs are host-only; the k8s agent cannot run them")
		return
	}

	// Sibling of the native check above, for the OTHER thing this agent
	// structurally cannot do.
	//
	// Artifact bytes do NOT flow agent -> controller -> S3 the way logs do:
	// they are transferred by the injected `unified-artifact` sidecar talking
	// to S3 DIRECTLY, with credentials from the Secret named by
	// cfg.SidecarS3SecretName. The controller's own S3 configuration never
	// reaches that sidecar. With no secret name configured, every artifact step
	// exits 1 with "artifact requires S3 configuration (UNIFIED_S3_*)" — but
	// only once the run reaches that step, i.e. after the job has already done
	// all of its work. Detect it here, before a Pod is even created.
	//
	// This is FAIL-FAST, not route-elsewhere: ClaimNextRun assigns the run to
	// this agent atomically and RunClaim has no hand-back path, so by the time
	// executeRun is reached the run is already ours and cannot be returned to
	// the queue for a capable agent to take. The only choice left is whether to
	// fail now or fail after wasting the job's work.
	if a.cfg.SidecarS3SecretName == "" {
		scan := scanTransferSteps(c)
		if len(scan.blocking) > 0 {
			a.failRun(ctx, c.RunID, fmt.Sprintf(
				"artifact steps require the unified-artifact sidecar's own S3 credentials, but the k8s-agent's sidecarS3SecretName config field is not set; affected steps: %s. Set sidecarS3SecretName to a Secret carrying UNIFIED_S3_* that exists in the JOB Pod namespace (%q, the agent config's namespace: field) — the controller's S3 configuration does not reach the sidecar.",
				strings.Join(scan.blocking, ", "), a.cfg.Namespace))
			return
		}
		if len(scan.conditional) > 0 {
			// An `if:` guard cannot be evaluated here — it may reference prior
			// steps' outputs, which do not exist until the run is under way — so
			// whether these steps run at all is unknowable at claim time. Failing
			// on a maybe would break jobs whose guard is false in this run and
			// which would otherwise have succeeded, so this only warns.
			a.warnRun(ctx, c.RunID, fmt.Sprintf(
				"this run has artifact step(s) guarded by an if: condition (%s), and the k8s-agent's sidecarS3SecretName config field is not set. If a guard evaluates true the step will fail with \"artifact requires S3 configuration (UNIFIED_S3_*)\"; the agent cannot tell in advance, so the run proceeds. Set sidecarS3SecretName to a Secret carrying UNIFIED_S3_* that exists in the job Pod namespace (%q) to make them work.",
				strings.Join(scan.conditional, ", "), a.cfg.Namespace))
		}
		if scan.cache > 0 {
			// Cache is deliberately best-effort: an unreachable store must never
			// fail a run, so this is one loud per-run warning rather than a
			// failure. Emitted into the run's own log (step -1) as well as the
			// agent log so the operator sees it where they are already looking.
			a.warnRun(ctx, c.RunID, fmt.Sprintf(
				"this run has %d cache step(s), but the k8s-agent's sidecarS3SecretName config field is not set: the unified-artifact sidecar has no S3 credentials, so every cache restore and save will be a no-op and the job pays full build times. Cache stays best-effort (the run will NOT fail); set sidecarS3SecretName to a Secret carrying UNIFIED_S3_* that exists in the job Pod namespace (%q) to enable it.",
				scan.cache, a.cfg.Namespace))
		}
	}

	// Capture the claim's start time up front, before pod acquisition. The
	// sidecar log pump uses it as GetLogs' SinceTime so a reused pooled pod
	// (whose sidecar containers are never restarted between runs) replays only
	// THIS run's sidecar output, not a previous claim's history.
	claimSince := metav1.Now()

	usePool := c.PodTemplate != nil && c.PodTemplate.Reuse

	var pooledPod *PooledPod
	var podName string
	podReady := false

	if usePool {
		templateName := ""
		if c.PodTemplate != nil {
			templateName = c.PodTemplate.Name
		}
		pp, err := a.pool.ClaimPod(ctx, c.RunID, templateName, a.cfg.PodTemplates, c.PodTemplate, a.cfg.PodImage,
			SidecarSpec{Image: a.cfg.SidecarImage, S3SecretName: a.cfg.SidecarS3SecretName}, a.cfg.ShimImage)
		if err != nil {
			a.failRun(ctx, c.RunID, fmt.Sprintf("k8s: failed to acquire Pod: %v", err))
			return
		}
		pooledPod = pp
		podName = pp.PodName
		defer func() {
			teardownCtx, cancelTeardown := a.claimPodTeardownContext(ctx)
			defer cancelTeardown()
			if !podReady {
				// The pod never reached Running; do not return a possibly-wedged
				// pod to the idle pool — delete it so the pool re-creates next time.
				if err := a.pm.DeletePod(teardownCtx, podName); err != nil {
					slog.Warn("k8s: failed to delete not-ready pooled Pod", "pod", podName, "error", err)
				}
				return
			}
			if err := a.pool.ReleasePod(teardownCtx, pooledPod, true); err != nil {
				slog.Warn("k8s: failed to release Pod", "pod", podName, "error", err)
			}
		}()
	} else {
		pod, err := BuildPod(c.RunID, a.cfg.Namespace, a.cfg.PodTemplates, c.PodTemplate, a.cfg.PodImage,
			SidecarSpec{Image: a.cfg.SidecarImage, S3SecretName: a.cfg.SidecarS3SecretName}, a.cfg.ShimImage)
		if err != nil {
			a.failRun(ctx, c.RunID, fmt.Sprintf("k8s: failed to build Pod spec: %v", err))
			return
		}
		created, err := a.pm.CreatePod(ctx, pod)
		if err != nil {
			a.failRun(ctx, c.RunID, fmt.Sprintf("k8s: failed to create Pod: %v", err))
			return
		}
		podName = created.Name
		defer func() {
			teardownCtx, cancelTeardown := a.claimPodTeardownContext(ctx)
			defer cancelTeardown()
			if err := a.pm.DeletePod(teardownCtx, podName); err != nil {
				slog.Warn("k8s: failed to delete Pod", "pod", podName, "error", err)
			}
		}()
	}

	masterTerminal, err := a.awaitPodRunning(ctx, podName, c.RunID)
	if err != nil {
		if masterTerminal {
			slog.Info("k8s: run became terminal before pod ready; abandoning", "runId", c.RunID, "pod", podName)
			return
		}
		a.failRun(ctx, c.RunID, fmt.Sprintf("k8s: run pod did not become ready: %v", err))
		return
	}
	podReady = true

	// If cleanWorkspace is true, clear the workspace before the first step
	if usePool && c.PodTemplate != nil && c.PodTemplate.CleanWorkspace {
		mountPath := "/workspace"
		if c.PodTemplate.Workspace != nil && c.PodTemplate.Workspace.MountPath != "" {
			mountPath = c.PodTemplate.Workspace.MountPath
		}
		firstContainer := ""
		for _, stage := range c.Stages {
			steps := api.StageSteps(stage)
			if len(steps) > 0 {
				firstContainer = execContainer(steps[0])
				break
			}
		}
		// nil shell: this is an internal maintenance script (not user-authored
		// run:), so the shim default (["/.ucd/ucd-sh","-c"], POSIX-compatible)
		// is always sufficient regardless of any step's declared shell:.
		_, _ = a.exec.ExecStep(ctx, podName, firstContainer, fmt.Sprintf("rm -rf %s/*", mountPath), nil, nil, io.Discard, io.Discard)
	}

	mountPath := "/workspace"
	if c.PodTemplate != nil && c.PodTemplate.Workspace != nil && c.PodTemplate.Workspace.MountPath != "" {
		mountPath = c.PodTemplate.Workspace.MountPath
	}

	// backend is the seam between the shared step-orchestration loop
	// (agentlib.RunClaim) and this pod's concrete execution environment. Its
	// scope-pod map is torn down at claim end by RunClaim's own deferred
	// b.CloseScopes, mirroring the pre-refactor scopePods defer (RunClaim
	// installs the masker itself via SetMasker after fetching secrets, so
	// this wrapper does neither).
	backend := newK8sBackend(a, c.RunID, c.JobName, podName, mountPath, dsl.SidecarContainerNames(c.PodTemplate), claimSince)

	agentlib.RunClaim(ctx, a.client, a.cfg.AgentID, c, backend)
}

// claimPodTeardownContext builds the context for the claim Pod's own teardown
// (delete, or release back to the idle pool), which runs from runClaim's
// defers AFTER agentlib.RunClaim — and therefore after RunClaim's own
// teardown window (b.CloseScopes, which handles the claim's SCOPE Pods) has
// already closed.
//
// context.WithoutCancel, because teardown must happen whether the run was
// cancelled, timed out, or the agent is draining — the same reason every
// other post-DAG phase does it. WithTimeout on top of it, because
// WithoutCancel also strips the deadline: this used to be a bare
// context.Background(), so a Kubernetes API server that accepts connections
// and stops answering pinned the claim here forever. No rest.Config.Timeout
// guards it — the same rest.Config drives exec streams and follow-mode log
// reads, which are legitimately long-lived (cmd/k8s-agent/main.go), so a
// client-wide timeout is not available as a fix.
//
// It reuses finallyTimeout rather than introducing a knob of its own, so
// there is exactly one cleanup ceiling for an operator to size. This is the
// FIFTH such window on the Kubernetes agent (see agentlib.DefaultFinallyBudget
// for the four the shared loop owns); the host agent has no equivalent,
// because hostBackend.CloseScopes tears its claim pod down inside window four.
func (a *K8sAgent) claimPodTeardownContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), a.cfg.FinallyTimeoutDuration())
}

// failRun fails a claim that could not begin executing (pod build/create/acquire
// or the run pod never becoming ready). reason is surfaced into the run's own
// logs (stepIndex -1, rendered "System" in the UI) before FinishRun(Failed).
// The log line is best-effort; FinishRun is retried until it lands so the run
// never sits stuck as Running. Mirrors the host agent's Agent.failRun.
func (a *K8sAgent) failRun(ctx context.Context, runID, reason string) {
	slog.Error(reason, "runId", runID)
	_ = a.client.AppendLogBulk(ctx, a.cfg.AgentID, runID, -1, []api.LogAppendRequest{{
		RunID:     runID,
		StepIndex: -1,
		Stream:    "stderr",
		Timestamp: time.Now().UTC(),
		Line:      reason,
	}})
	agentlib.RetryUntilSuccess(ctx, func(cc context.Context) error {
		return a.client.FinishRun(cc, a.cfg.AgentID, runID, api.RunFailed)
	})
}

// warnRun is failRun's non-fatal twin: it records reason in the agent log and
// in the run's own log (the same step -1 "run level" stream failRun uses), but
// leaves the run's status alone. Used for conditions the operator must see but
// which are not, by design, run-failing — a cache step with no sidecar S3
// credentials being the motivating case.
func (a *K8sAgent) warnRun(ctx context.Context, runID, reason string) {
	slog.Warn(reason, "runId", runID)
	_ = a.client.AppendLogBulk(ctx, a.cfg.AgentID, runID, -1, []api.LogAppendRequest{{
		RunID:     runID,
		StepIndex: -1,
		Stream:    "stderr",
		Timestamp: time.Now().UTC(),
		Line:      reason,
	}})
}

// transferScan is what the claim-time preflight learns by walking a claim's
// steps. The three buckets exist because they warrant three different actions,
// and lumping them together fails runs that would have succeeded.
type transferScan struct {
	// blocking names artifact steps that will run unconditionally AND whose
	// failure would fail the run. Only these justify failing the claim: for
	// them, "the sidecar has no credentials" and "this run is doomed" are the
	// same statement.
	blocking []string
	// conditional names artifact steps guarded by an `if:`. Whether they run is
	// not knowable here, so they only warrant a warning — see the call site.
	conditional []string
	// cache counts cache steps, which are best-effort by design and never
	// justify failing anything.
	cache int
}

// scanTransferSteps walks every step of a claim — both `stages` and `finally`,
// including the members of explicit `parallel:` groups — and sorts the ones
// that need the artifact sidecar's S3 credentials into transferScan's buckets.
//
// Two step fields deliberately keep an artifact step OUT of the blocking
// bucket, because a preflight that ignores them overrides guarantees the job
// author was given elsewhere:
//
//   - continueOnError: true is the explicit "this step failing must not fail
//     the run" contract, honoured for both a returned error and a panic in
//     internal/agent/pipeline.go's runOne. A preflight that hard-fails the
//     claim would silently revoke it — and would do so EARLIER and more
//     completely than the failure it is protecting against. Skipped entirely.
//
//   - A non-empty if: cannot be evaluated at claim time; the expression may
//     reference prior steps' outputs, which do not exist yet. The step may
//     never run at all, so failing the claim would break jobs whose guard is
//     false in this run. Downgraded to a warning.
//
// continueOnError wins over if:, since a guard being true still cannot make a
// continueOnError step fail its run.
//
// `call:` steps need no recursion: api.ClaimCallStep carries only a job name
// and params — the called job becomes a CHILD RUN with its own claim, which
// runs this same check for itself.
func scanTransferSteps(c api.ClaimResponse) transferScan {
	var scan transferScan
	visit := func(s *api.ClaimStep) {
		if s == nil {
			return
		}
		if s.Cache != nil {
			scan.cache++
		}
		if s.UploadArtifact == nil && s.DownloadArtifact == nil {
			return
		}
		switch {
		case s.ContinueOnError:
			// Its failure is contractually harmless; say nothing.
		case s.If != "":
			scan.conditional = append(scan.conditional, strconv.Quote(s.DisplayName()))
		default:
			scan.blocking = append(scan.blocking, strconv.Quote(s.DisplayName()))
		}
	}
	for _, stages := range [][]api.ClaimStage{c.Stages, c.Finally} {
		for _, st := range stages {
			visit(st.Step)
			for i := range st.Parallel {
				visit(&st.Parallel[i])
			}
		}
	}
	return scan
}

// awaitPodRunning waits for podName to reach Running, bounded by
// cfg.PodStartTimeoutDuration(), and abortable early if the controller marks the
// run terminal (user cancel or reap) before the pod is ready. Under
// RestartPolicy: Never a Pending/ImagePullBackOff pod never transitions to
// Failed, so without this bound the wait would hang until full agent shutdown.
//
// It returns masterTerminal=true (with a non-nil err) when the wait was aborted
// because the run is already terminal at the controller — the caller must clean
// up the pod but must NOT override the controller's authoritative status.
func (a *K8sAgent) awaitPodRunning(ctx context.Context, podName, runID string) (masterTerminal bool, err error) {
	waitCtx, cancel := context.WithTimeout(ctx, a.cfg.PodStartTimeoutDuration())
	defer cancel()

	// Read the poll interval on this (the caller's) goroutine before spawning the
	// watcher, so a test mutating agentlib.CancelPollInterval concurrently never
	// races the watcher's read (mirrors internal/agent/orchestrator.go:91-100).
	pollInterval := agentlib.CancelPollInterval

	var terminal atomic.Bool
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-waitCtx.Done():
				return
			case <-ticker.C:
				run, gerr := a.client.GetRun(waitCtx, runID)
				if gerr != nil {
					continue
				}
				if isTerminalRunStatus(run.Status) {
					terminal.Store(true)
					cancel()
					return
				}
			}
		}
	}()

	werr := a.pm.WaitForPodRunning(waitCtx, podName)
	cancel()
	<-watchDone

	if terminal.Load() {
		return true, fmt.Errorf("run %s reached terminal status before pod %s became ready", runID, podName)
	}
	return false, werr
}

// execContainer returns the pod container a step should exec into. After DSL
// normalization the canonical exec target is the flat Container field; an empty
// string means the default container.
func execContainer(s api.ClaimStep) string {
	return s.Container
}

// imageStepEnv returns a fresh env map for a Linux scope pod: the step's env
// plus UNIFIED_AGENT_OS and UNIFIED_WORKSPACE. Always a new map, so callers
// never mutate the claim. The scope pod runs a Linux container image
// regardless of the agent's host OS, so UNIFIED_AGENT_OS is "linux" — not
// the agent process's runtime.GOOS. UNIFIED_WORKSPACE is scopeMountPath
// ("/workspace"), the scope pod's fixed working directory (scopepod.go) —
// these are the defaults a scope pod's creation env falls back to when the
// caller passes no override (e.g. resolveScope's cache/artifact-step path,
// which calls EnsureScope with a nil env); ensureScopePod (backend.go)
// merges the orchestrator's already-expanded extraEnv over this baseline for
// scoped run: steps, so the caller's value still wins there.
func imageStepEnv(step api.ClaimStep) map[string]string {
	env := make(map[string]string, len(step.Env)+2)
	for k, v := range step.Env {
		env[k] = v
	}
	env["UNIFIED_AGENT_OS"] = "linux"
	env["UNIFIED_WORKSPACE"] = scopeMountPath
	return env
}

func appendLabelIfMissing(labels []string, label string) []string {
	for _, l := range labels {
		if l == label {
			return labels
		}
	}
	return append(labels, label)
}
