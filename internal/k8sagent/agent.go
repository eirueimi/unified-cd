package k8sagent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/secrets"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// stderrAutoFlushInterval is how often a step's stderr LogPusher is flushed
// while the step is still running, so sparse stderr output appears in the
// WebUI before the step completes (mirrors the host agent's stdout streaming
// behavior). It is a var (not a const) so tests can shorten it.
var stderrAutoFlushInterval = 2 * time.Second

// imagePodStartTimeout bounds how long ensureScopePod (k8sBackend, backend.go)
// waits for a throwaway uses-scope pod to reach Running. Under
// RestartPolicy: Never a pod stuck in Pending/ImagePullBackOff never
// transitions to Failed, so without a bound the wait would hang until the
// whole run is cancelled. This gives a bad image a fast, explicit failure
// instead.
const imagePodStartTimeout = 5 * time.Minute

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
		AgentID:      a.cfg.AgentID,
		Hostname:     host,
		OS:           runtime.GOOS + "/k8s",
		Labels:       labels,
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

	// Concurrency gate: positive MaxConcurrent -> semaphore of that size;
	// negative -> unlimited (nil sem, dispatch ungated). Validate mapped 0->100.
	var sem chan struct{}
	if a.cfg.MaxConcurrent > 0 {
		sem = make(chan struct{}, a.cfg.MaxConcurrent)
	}

	var wg sync.WaitGroup
claimLoop:
	for {
		if ctx.Err() != nil {
			break
		}
		if sem != nil {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				break claimLoop
			}
		}
		resp, err := a.client.Claim(ctx, a.cfg.AgentID, "30s", labels)
		if err != nil {
			if sem != nil {
				<-sem
			}
			slog.Error("claim error", "error", err)
			select {
			case <-ctx.Done():
				break claimLoop
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
			// Defense-in-depth: a.dispatch (executeRun) has its own internal
			// error handling, but a panic anywhere in that call graph would
			// otherwise crash the whole agent process and take every other
			// in-flight run down with it. Recover here and fail just this run,
			// mirroring the host agent's executeRun guard
			// (internal/agent/agent.go).
			defer func() {
				if r := recover(); r != nil {
					slog.Error("k8s: agent panic in dispatch", "runId", c.RunID, "panic", r, "stack", string(debug.Stack()))
					// An inner recover so a panic INSIDE failRun (e.g. a nil
					// client) can't re-crash the dispatch goroutine.
					defer func() { _ = recover() }()
					a.failRun(runCtx, c.RunID, fmt.Sprintf("agent panic: %v", r))
				}
			}()
			// Enrolled/retired around dispatch so the heartbeat reports this run
			// as active for its whole execution, including the panic-recover
			// path above (defers run LIFO regardless of outcome).
			activeRuns.Add(c.RunID)
			defer activeRuns.Remove(c.RunID)
			a.dispatch(runCtx, c)
		}(resp)
	}

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
			if !podReady {
				// The pod never reached Running; do not return a possibly-wedged
				// pod to the idle pool — delete it so the pool re-creates next time.
				if err := a.pm.DeletePod(context.Background(), podName); err != nil {
					slog.Warn("k8s: failed to delete not-ready pooled Pod", "pod", podName, "error", err)
				}
				return
			}
			if err := a.pool.ReleasePod(context.Background(), pooledPod, true); err != nil {
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
			if err := a.pm.DeletePod(context.Background(), podName); err != nil {
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

// logLineWriter is a Writer that sends each line of stdout to the master server via AppendLog.
// A nil masker is a no-op (lines are shipped unmodified).
type logLineWriter struct {
	client  *agentlib.Client
	agentID string
	runID   string
	stepIdx int
	stream  string
	masker  *secrets.Masker
	buf     strings.Builder
}

// Write receives a byte slice and sends lines delimited by newlines to the master.
func (lw *logLineWriter) Write(p []byte) (int, error) {
	lw.buf.Write(p)
	s := lw.buf.String()
	for {
		idx := strings.IndexByte(s, '\n')
		if idx < 0 {
			break
		}
		line := s[:idx]
		s = s[idx+1:]
		if lw.masker != nil {
			line = lw.masker.Mask(line)
		}
		_ = lw.client.AppendLog(context.Background(), lw.agentID, api.LogAppendRequest{
			RunID:     lw.runID,
			StepIndex: lw.stepIdx,
			Stream:    lw.stream,
			Timestamp: time.Now().UTC(),
			Line:      line,
		})
	}
	lw.buf.Reset()
	lw.buf.WriteString(s)
	return len(p), nil
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
