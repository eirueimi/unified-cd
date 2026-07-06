package k8sagent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/secrets"
	corev1 "k8s.io/api/core/v1"
)

// approvalPollInterval is how often WaitForApproval polls the controller for a
// human decision. It is a var (not a const) so tests can shorten it.
var approvalPollInterval = 3 * time.Second

// cancelPollInterval is how often the cancel poller polls the controller to
// detect mid-run cancellation. It is a var (not a const) so tests can shorten it.
var cancelPollInterval = 5 * time.Second

// stderrAutoFlushInterval is how often a step's stderr LogPusher is flushed
// while the step is still running, so sparse stderr output appears in the
// WebUI before the step completes (mirrors the host agent's stdout streaming
// behavior). It is a var (not a const) so tests can shorten it.
var stderrAutoFlushInterval = 2 * time.Second

// imagePodStartTimeout bounds how long runImageStep waits for a throwaway
// image pod to reach Running. Under RestartPolicy: Never a pod stuck in
// Pending/ImagePullBackOff never transitions to Failed, so without a bound
// the wait would hang until the whole run is cancelled. This gives a bad
// image a fast, explicit failure instead.
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
	ExecStep(ctx context.Context, podName, container, script string, env []string, stdout, stderr io.Writer) (int, error)
	ExecStepArgv(ctx context.Context, podName, container string, argv []string, stdout, stderr io.Writer) (int, error)
}

// K8sAgent is an agent that claims Runs from the master and executes them inside a Kubernetes Pod.
type K8sAgent struct {
	cfg    Config
	client *agentlib.Client
	pm     podManager
	exec   stepExecutor
	pool   *PodPool
}

// NewK8sAgent creates a new K8sAgent.
func NewK8sAgent(cfg Config, agentClient *agentlib.Client, pm *PodManager, exec *Executor, pool *PodPool) *K8sAgent {
	return &K8sAgent{cfg: cfg, client: agentClient, pm: pm, exec: exec, pool: pool}
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

	// The k8s agent has no drain/cordon: ctx is cancelled only on full shutdown,
	// so binding the heartbeat to ctx keeps it alive for the agent's whole lifetime
	// and stops it cleanly on shutdown. (Unlike the host agent, there is no separate
	// claimCtx that is cancelled before in-flight runs finish, so no divergence here.)
	agentlib.StartHeartbeat(ctx, a.client, a.cfg.AgentID, agentlib.DefaultHeartbeatInterval)
	go a.runPodGC(ctx, time.Minute)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		resp, err := a.client.Claim(ctx, a.cfg.AgentID, "30s", labels)
		if err != nil {
			slog.Error("claim error", "error", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}
		if resp.RunID == "" {
			continue
		}
		go a.executeRun(ctx, resp)
	}
}

// executeRun executes the steps contained in a ClaimResponse sequentially inside a Kubernetes Pod.
// On step failure, subsequent steps are skipped and the Run is transitioned to Failed state.
func (a *K8sAgent) executeRun(ctx context.Context, c api.ClaimResponse) {
	slog.Info("k8s: executing Run", "runId", c.RunID, "job", c.JobName)

	// Fetch the secrets needed for this Run and build the masker, mirroring
	// the standard host agent (internal/agent/agent.go): on fetch failure,
	// warn and continue without secrets rather than failing the run.
	var secretValues map[string]string
	var masker *secrets.Masker
	if len(c.SecretsNeeded) > 0 {
		var err error
		secretValues, err = a.client.FetchSecrets(ctx, a.cfg.AgentID, c.SecretsNeeded)
		if err != nil {
			slog.Warn("k8s: failed to fetch secrets, continuing without secrets", "runId", c.RunID, "error", err)
			secretValues = map[string]string{}
		}
		vals := make([]string, 0, len(secretValues))
		for _, v := range secretValues {
			vals = append(vals, v)
		}
		masker = secrets.NewMasker(vals)
	} else {
		secretValues = map[string]string{}
		masker = secrets.NoOpMasker
	}

	usePool := c.PodTemplate != nil && c.PodTemplate.Reuse

	var pooledPod *PooledPod
	var podName string

	if usePool {
		templateName := ""
		if c.PodTemplate != nil {
			templateName = c.PodTemplate.Name
		}
		pp, err := a.pool.ClaimPod(ctx, c.RunID, templateName, a.cfg.PodTemplates, c.PodTemplate, a.cfg.PodImage,
			SidecarSpec{Image: a.cfg.SidecarImage, S3SecretName: a.cfg.SidecarS3SecretName})
		if err != nil {
			slog.Error("k8s: failed to acquire Pod", "runId", c.RunID, "error", err)
			_ = a.client.FinishRun(ctx, a.cfg.AgentID, c.RunID, api.RunFailed)
			return
		}
		pooledPod = pp
		podName = pp.PodName
		defer func() {
			if err := a.pool.ReleasePod(context.Background(), pooledPod, true); err != nil {
				slog.Warn("k8s: failed to release Pod", "pod", podName, "error", err)
			}
		}()
	} else {
		pod, err := BuildPod(c.RunID, a.cfg.Namespace, a.cfg.PodTemplates, c.PodTemplate, a.cfg.PodImage,
			SidecarSpec{Image: a.cfg.SidecarImage, S3SecretName: a.cfg.SidecarS3SecretName})
		if err != nil {
			slog.Error("k8s: failed to build Pod spec", "runId", c.RunID, "error", err)
			_ = a.client.FinishRun(ctx, a.cfg.AgentID, c.RunID, api.RunFailed)
			return
		}
		created, err := a.pm.CreatePod(ctx, pod)
		if err != nil {
			slog.Error("k8s: failed to create Pod", "runId", c.RunID, "error", err)
			_ = a.client.FinishRun(ctx, a.cfg.AgentID, c.RunID, api.RunFailed)
			return
		}
		podName = created.Name
		defer func() {
			if err := a.pm.DeletePod(context.Background(), podName); err != nil {
				slog.Warn("k8s: failed to delete Pod", "pod", podName, "error", err)
			}
		}()
	}

	if err := a.pm.WaitForPodRunning(ctx, podName); err != nil {
		slog.Error("k8s: failed waiting for Pod to start", "runId", c.RunID, "error", err)
		_ = a.client.FinishRun(ctx, a.cfg.AgentID, c.RunID, api.RunFailed)
		return
	}

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
		_, _ = a.exec.ExecStep(ctx, podName, firstContainer, fmt.Sprintf("rm -rf %s/*", mountPath), nil, io.Discard, io.Discard)
	}

	mountPath := "/workspace"
	if c.PodTemplate != nil && c.PodTemplate.Workspace != nil && c.PodTemplate.Workspace.MountPath != "" {
		mountPath = c.PodTemplate.Workspace.MountPath
	}

	// backend is the seam between the shared step-orchestration loop
	// (orchestrate) and this pod's concrete execution environment. Its
	// scope-pod map is torn down at claim end (deferred below) regardless of
	// how the claim finished, mirroring the pre-refactor scopePods defer.
	backend := newK8sBackend(a, c.RunID, podName, mountPath)
	backend.SetMasker(masker)
	defer backend.CloseScopes(context.WithoutCancel(ctx))

	a.orchestrate(ctx, c, backend, secretValues)
}

// orchestrate runs the claim's stages, reporting step/run status, dispatching
// each step's execution through b (the ExecBackend seam between this shared
// step-orchestration loop and the pod's concrete execution environment).
// secretValues holds the resolved values for c.SecretsNeeded (fetched by
// executeRun before orchestrate is called); it is exposed to step templates
// as {{ .Secrets.X }}, mirroring the standard host agent. May be nil/empty
// when the claim needs no secrets.
func (a *K8sAgent) orchestrate(ctx context.Context, c api.ClaimResponse, b agentlib.ExecBackend, secretValues map[string]string) {
	// reportCtx is derived from the ORIGINAL incoming ctx (before any job-level
	// timeout is applied below) via WithoutCancel, so a StepReportRequest for a
	// step that failed BECAUSE its context deadline was exceeded can still
	// reach the controller instead of itself being aborted by the very
	// deadline it is reporting on. Used for every ReportStep call in this
	// function; mirrors the host agent's per-report context.WithoutCancel
	// pattern (internal/agent/agent.go:679).
	reportCtx := context.WithoutCancel(ctx)

	// Apply job-level timeout to the context if one is configured, mirroring
	// the host agent (internal/agent/agent.go:264-268).
	if c.TimeoutMinutes > 0 {
		var jobCancel context.CancelFunc
		ctx, jobCancel = context.WithTimeout(ctx, time.Duration(c.TimeoutMinutes*float64(time.Minute)))
		defer jobCancel()
	}

	// mountPath is the default pod's workspace volume mount path, used to join
	// cache/artifact steps' relative paths (mirrors the same computation in
	// executeRun, which also feeds it into the backend's own field for
	// resolveSidecarTarget's non-scoped case). Duplicated here (rather than
	// read off the backend) because ExecBackend does not expose it: tests
	// construct a fake backend that has no notion of a pod's mount path.
	mountPath := "/workspace"
	if c.PodTemplate != nil && c.PodTemplate.Workspace != nil && c.PodTemplate.Workspace.MountPath != "" {
		mountPath = c.PodTemplate.Workspace.MountPath
	}

	var anyStepFailed atomic.Bool
	var cancelledByMaster atomic.Bool
	statusView := func() dsl.RunStatusView {
		cancelled := cancelledByMaster.Load()
		return dsl.RunStatusView{Failed: anyStepFailed.Load() && !cancelled, Cancelled: cancelled}
	}

	// cacheSaveSpec captures a cache step's save parameters, deferred until
	// after the main stages complete (so the save captures the final
	// workspace state, matching the standard agent's cache semantics). scope
	// records where the matching restore ran (the run pod or a scoped step's
	// scope pod), so the deferred save targets the same pod's sidecar and
	// filesystem via backend.CacheSave.
	type cacheSaveSpec struct {
		key     string
		ttlDays int
		path    string
		scope   agentlib.ScopeHandle
	}
	var cacheSavesMu sync.Mutex
	var cacheSaves []cacheSaveSpec
	registerCacheSave := func(s cacheSaveSpec) {
		cacheSavesMu.Lock()
		cacheSaves = append(cacheSaves, s)
		cacheSavesMu.Unlock()
	}

	// postHookEntry is a post: hook queued after a step Succeeds, mirroring the
	// host agent's postHookEntry (internal/agent/agent.go:57-69). scope carries
	// the owning step's ScopeHandle (zero for a default-pod step) so the drain
	// below (after the main stages, before finally) can route the hook via
	// backend.RunPostHook into the same container the step body ran in: the
	// step's scope pod's "step" container when scope is non-zero, otherwise
	// the run pod's default container. The k8s agent runs a claim's steps one
	// at a time via agentlib.RunPipeline in Sequential mode (no goroutines
	// within orchestrate — see k8sBackend's scopePods map doc comment), so
	// hookStack needs no mutex, unlike the host's postHooksMu (which guards
	// concurrent `parallel:` step-runner goroutines).
	type postHookEntry struct {
		stepName  string
		post      api.PostStep
		scope     agentlib.ScopeHandle
		container string
	}
	var hookStack []postHookEntry

	// runCtx is cancelled when the master cancels the run mid-flight. Passing it
	// as the exec context interrupts an in-flight pod exec (and unblocks an
	// approval wait). Cancelling it here (defer) also stops the poller goroutine.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	// Cancel poller: periodically ask the controller whether the run was
	// cancelled. On cancellation, record it and cancel runCtx to interrupt any
	// in-flight step/approval, then exit.
	go func() {
		ticker := time.NewTicker(cancelPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				run, err := a.client.GetRun(runCtx, c.RunID)
				if err != nil {
					slog.Warn("k8s: cancel poller: get run failed", "runID", c.RunID, "error", err)
					continue
				}
				if run.Status == api.RunCancelled {
					slog.Info("k8s: received cancellation signal from master; interrupting run", "runID", c.RunID)
					cancelledByMaster.Store(true)
					cancelRun()
					return
				}
			}
		}
	}()

	// Accumulate step outputs for template expansion in subsequent steps
	stepCtx := dsl.TemplateData{Params: c.Params, Steps: map[string]dsl.StepData{}, Secrets: secretValues}

	// makeRunStep builds the per-step execution function. It is reused for the
	// main stages and for the finally block, parametrized by:
	//   statusFn:        the run status used to evaluate if: conditions
	//                    (live status for main stages, frozen for finally)
	//   implicitSuccess: true for main steps (a no-if step is gated by success(),
	//                    so it auto-skips after a failure); false for finally
	//                    (a no-if finally step always runs)
	//   failedFlag:      the flag a non-continueOnError failure records into
	//   execCtx:         the context passed to stepExec/approval; runCtx for the
	//                    main stages (so a cancel interrupts the in-flight step),
	//                    a non-cancelling context for finally (so it still runs)
	//   suppressOnCancel: true for the main stages (a cancel-induced failure is
	//                    not recorded as a real failure), false for finally (a
	//                    genuine finally failure counts even on cancellation)
	// It records failures into failedFlag instead of aborting the loop.
	makeRunStep := func(statusFn func() dsl.RunStatusView, implicitSuccess bool, failedFlag *atomic.Bool, execCtx context.Context, suppressOnCancel bool) func(api.ClaimStep) {
		// recordFailure records a non-continueOnError failure, honouring
		// suppressOnCancel (a cancel-induced failure on the main path is not a
		// real failure; a finally failure counts even when the run was cancelled).
		recordFailure := func(step api.ClaimStep) {
			if step.ContinueOnError {
				return
			}
			if suppressOnCancel && cancelledByMaster.Load() {
				return
			}
			failedFlag.Store(true)
		}
		return func(step api.ClaimStep) {
			// Build template data; expose matrix/foreach values if set
			tplData := dsl.TemplateData{Params: stepCtx.Params, Steps: stepCtx.Steps, Secrets: stepCtx.Secrets}
			if step.MatrixValues != nil {
				tplData.Matrix = step.MatrixValues
				tplData.Foreach = step.MatrixValues
			}

			// Every step is gated by if:. On eval error the step runs (fail-safe);
			// on false it is reported Skipped and not run.
			ok, err := dsl.EvalCondition(step.If, tplData, statusFn(), implicitSuccess)
			if err != nil {
				slog.Warn("k8s: if condition eval failed, running step", "step", step.Name, "error", err)
			}
			if !ok {
				agentlib.RetryUntilSuccess(reportCtx, func(callCtx context.Context) error {
					return a.client.ReportStep(callCtx, a.cfg.AgentID, api.StepReportRequest{
						RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Skipped",
					})
				})
				return
			}

			// Apply step-level timeout to the exec context if one is configured,
			// mirroring the host agent (internal/agent/agent.go:443-447). This
			// covers every step kind below (approval, cache, artifact, run) just
			// like the host. Not applied to runsIn.image steps' pod lifetime,
			// which already gets its own bound via imageStepDeadline/
			// ActiveDeadlineSeconds (podbuilder.go) — a shorter exec-context
			// timeout here would only make that path fail earlier/redundantly,
			// never later, so this is still safe to apply uniformly.
			if step.TimeoutMinutes > 0 {
				var stepCancel context.CancelFunc
				execCtx, stepCancel = context.WithTimeout(execCtx, time.Duration(step.TimeoutMinutes*float64(time.Minute)))
				defer stepCancel()
			}

			// approval gate: WaitForApproval reports WaitingApproval and polls
			// for the human decision. Placed after the if: gate so an approval
			// step can itself be if:-gated; reports only the terminal status.
			if step.Approval != nil {
				approved := agentlib.WaitForApproval(execCtx, a.client, a.cfg.AgentID, c.RunID, step, approvalPollInterval)
				status := "Succeeded"
				if !approved {
					status = "Failed"
				}
				_ = a.client.ReportStep(reportCtx, a.cfg.AgentID, api.StepReportRequest{
					RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: status, EndedAt: time.Now().UTC(),
				})
				if !approved {
					recordFailure(step)
				}
				return
			}

			// cache restore: exec the unified-sidecar binary's "cache restore" into
			// the sidecar; best-effort, so a miss/error never fails the step. The
			// matching save is deferred until after the main stages complete.
			if step.Cache != nil {
				started := time.Now().UTC()
				_ = a.client.ReportStep(reportCtx, a.cfg.AgentID, api.StepReportRequest{
					RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Running", StartedAt: started,
				})
				// A scoped step's cache targets its scope pod's sidecar and
				// private scratch volume instead of the run pod's, so a scope's
				// cache never leaks into (or collides with) the shared workspace.
				mount := mountPath
				var scope agentlib.ScopeHandle
				if step.ScopeID != "" {
					var err error
					scope, err = b.EnsureScope(execCtx, step, nil)
					if err != nil {
						slog.Warn("k8s: cache scope pod unavailable; skipping cache for step", "step", step.Name, "error", err)
						agentlib.RetryUntilSuccess(reportCtx, func(callCtx context.Context) error {
							return a.client.ReportStep(callCtx, a.cfg.AgentID, api.StepReportRequest{
								RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Succeeded", StartedAt: started, EndedAt: time.Now().UTC(),
							})
						})
						return
					}
					mount = scopeMountPath
				}
				key, kerr := dsl.ExpandTemplate(step.Cache.Key, tplData)
				expandedPath, perr := dsl.ExpandTemplate(step.Cache.Path, tplData)
				// A template PARSE/EXPAND error on cache.key or cache.path is a hard
				// failure (matching the standard host agent, which fails the step
				// loudly on the same condition) — it means the job author wrote a
				// malformed template and silently succeeding would hide the bug and
				// let the cache target the wrong directory. Empty-but-valid expansion
				// (key/path resolves to "") stays a legitimate best-effort skip: the
				// step succeeds without any cache op.
				if kerr != nil || perr != nil {
					tplErr := kerr
					which := "cache.key"
					if kerr == nil {
						tplErr = perr
						which = "cache.path"
					}
					slog.Error("k8s: cache template expansion failed; failing step", "step", step.Name, "which", which, "error", tplErr)
					recordFailure(step)
					agentlib.RetryUntilSuccess(reportCtx, func(callCtx context.Context) error {
						return a.client.ReportStep(callCtx, a.cfg.AgentID, api.StepReportRequest{
							RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Failed", StartedAt: started, EndedAt: time.Now().UTC(),
						})
					})
					return
				}
				if key == "" {
					// A valid-but-empty key must not silently collide caches across
					// runs. Skip the cache operation entirely (no restore, no deferred
					// save) but keep cache best-effort: the step still succeeds.
					slog.Warn("k8s: cache key expanded to empty; skipping cache for step", "step", step.Name, "keyTemplate", step.Cache.Key)
				} else if expandedPath == "" {
					// A valid-but-empty path would make the cache target the workspace
					// mount root (or a wrong directory). Skip the cache operation
					// entirely, same as an empty key.
					slog.Warn("k8s: cache path expanded to empty; skipping cache for step", "step", step.Name, "pathTemplate", step.Cache.Path)
				} else {
					var restoreKeys []string
					for _, rk := range step.Cache.RestoreKeys {
						if v, _ := dsl.ExpandTemplate(rk, tplData); v != "" {
							restoreKeys = append(restoreKeys, v)
						}
					}
					cachePath := path.Join(mount, expandedPath)
					// Best-effort: a miss/error never fails the step.
					if _, err := b.CacheRestore(execCtx, scope, key, restoreKeys, cachePath); err != nil {
						slog.Warn("k8s: cache restore failed", "step", step.Name, "error", err)
					}

					ttlDays := step.Cache.TTLDays
					if ttlDays == 0 {
						ttlDays = 30
					}
					registerCacheSave(cacheSaveSpec{key: key, ttlDays: ttlDays, path: cachePath, scope: scope})
				}

				agentlib.RetryUntilSuccess(reportCtx, func(callCtx context.Context) error {
					return a.client.ReportStep(callCtx, a.cfg.AgentID, api.StepReportRequest{
						RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Succeeded", StartedAt: started, EndedAt: time.Now().UTC(),
					})
				})
				return
			}

			// artifact upload: exec the unified-sidecar binary via argv into the
			// sidecar. Artifacts are fail-loud (not best-effort).
			if step.UploadArtifact != nil {
				started := time.Now().UTC()
				_ = a.client.ReportStep(reportCtx, a.cfg.AgentID, api.StepReportRequest{
					RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Running", StartedAt: started,
				})
				// A scoped step's artifact upload reads from its scope pod's
				// private scratch volume via its sidecar, not the run pod's.
				mount := mountPath
				var scope agentlib.ScopeHandle
				if step.ScopeID != "" {
					var err error
					scope, err = b.EnsureScope(execCtx, step, nil)
					if err != nil {
						// Artifacts are fail-loud: a scope pod that never becomes
						// available must fail the step, not silently upload from
						// the wrong (run pod) filesystem.
						agentlib.RetryUntilSuccess(reportCtx, func(callCtx context.Context) error {
							return a.client.ReportStep(callCtx, a.cfg.AgentID, api.StepReportRequest{
								RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Failed", StartedAt: started, EndedAt: time.Now().UTC(),
							})
						})
						recordFailure(step)
						return
					}
					mount = scopeMountPath
				}
				artifactPath := path.Join(mount, step.UploadArtifact.Path)
				err := b.UploadArtifact(execCtx, scope, c.RunID, step.UploadArtifact.Name, artifactPath)
				status := "Succeeded"
				ec := 0
				if err != nil {
					status = "Failed"
					ec = 1
				}
				agentlib.RetryUntilSuccess(reportCtx, func(callCtx context.Context) error {
					return a.client.ReportStep(callCtx, a.cfg.AgentID, api.StepReportRequest{
						RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: status, ExitCode: ec, StartedAt: started, EndedAt: time.Now().UTC(),
					})
				})
				if status == "Failed" {
					recordFailure(step)
				}
				return
			}

			// artifact download: exec the unified-sidecar binary via argv into the
			// sidecar. Artifacts are fail-loud (not best-effort).
			if step.DownloadArtifact != nil {
				started := time.Now().UTC()
				_ = a.client.ReportStep(reportCtx, a.cfg.AgentID, api.StepReportRequest{
					RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Running", StartedAt: started,
				})
				// A scoped step's artifact download writes into its scope pod's
				// private scratch volume via its sidecar, not the run pod's.
				mount := mountPath
				var scope agentlib.ScopeHandle
				if step.ScopeID != "" {
					var err error
					scope, err = b.EnsureScope(execCtx, step, nil)
					if err != nil {
						// Artifacts are fail-loud: a scope pod that never becomes
						// available must fail the step, not silently download into
						// the wrong (run pod) filesystem.
						agentlib.RetryUntilSuccess(reportCtx, func(callCtx context.Context) error {
							return a.client.ReportStep(callCtx, a.cfg.AgentID, api.StepReportRequest{
								RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Failed", StartedAt: started, EndedAt: time.Now().UTC(),
							})
						})
						recordFailure(step)
						return
					}
					mount = scopeMountPath
				}
				dest := step.DownloadArtifact.DestDir
				if dest == "" {
					dest = "."
				}
				destDir := path.Join(mount, dest)
				err := b.DownloadArtifact(execCtx, scope, c.RunID, step.DownloadArtifact.Name, destDir)
				status := "Succeeded"
				ec := 0
				if err != nil {
					status = "Failed"
					ec = 1
				}
				agentlib.RetryUntilSuccess(reportCtx, func(callCtx context.Context) error {
					return a.client.ReportStep(callCtx, a.cfg.AgentID, api.StepReportRequest{
						RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: status, ExitCode: ec, StartedAt: started, EndedAt: time.Now().UTC(),
					})
				})
				if status == "Failed" {
					recordFailure(step)
				}
				return
			}

			// call: launch a child Run and wait for it. Shared with the host
			// agent via agentlib.ExecuteCallStep so the two backends cannot
			// diverge; where the child actually runs is decided by the child
			// job's own scheduling (k8s -> k8s or k8s -> a dedicated host).
			if step.Call != nil {
				outputs, childRunID, callErr := agentlib.ExecuteCallStep(execCtx, a.client, a.cfg.AgentID, c.RunID, step, tplData)
				status := "Succeeded"
				if callErr != nil {
					slog.Error("k8s: call step failed", "step", step.Name, "error", callErr)
					status = "Failed"
				} else if len(outputs) > 0 {
					// k8s runs steps sequentially, so no lock is needed here.
					agentlib.ApplyStepOutputs(stepCtx.Steps, step.Name, step.MatrixKey, outputs)
					agentlib.RetryUntilSuccess(reportCtx, func(callCtx context.Context) error {
						return a.client.SetStepOutputs(callCtx, a.cfg.AgentID, c.RunID, step.Index, step.MatrixKey, outputs)
					})
				}
				agentlib.RetryUntilSuccess(reportCtx, func(callCtx context.Context) error {
					return a.client.ReportStep(callCtx, a.cfg.AgentID, api.StepReportRequest{
						RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex,
						StepName: step.DisplayName(), Variant: step.MatrixKey, Status: status,
						ChildRunID: childRunID, CallJobName: step.Call.Job, EndedAt: time.Now().UTC(),
					})
				})
				if status == "Failed" {
					recordFailure(step)
				}
				return
			}

			started := time.Now().UTC()
			_ = a.client.ReportStep(reportCtx, a.cfg.AgentID, api.StepReportRequest{
				RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Running", StartedAt: started,
			})

			// Attempt template expansion; fall back to the original script on failure
			expandedRun, _ := dsl.ExpandTemplate(step.Run, tplData)
			if expandedRun == "" {
				expandedRun = step.Run
			}

			stepForExec := step
			stepForExec.Env = expandStepEnv(step.Env, tplData)

			shippedStdout, shippedStderr, finishLogs := b.StepLogWriters(execCtx, step.Index)
			// stdout is teed: streamed to the server while the step runs AND
			// kept in stdoutBuf for {{ .Stdout }} output-template evaluation
			// below (mirrors the pre-refactor stepExec closure's io.MultiWriter).
			var stdoutBuf strings.Builder
			stdoutTee := io.MultiWriter(&stdoutBuf, shippedStdout)

			var ec int
			var execErr error
			var scope agentlib.ScopeHandle
			if step.ScopeID != "" {
				// uses-scope: exec into the step's dedicated scope pod instead of
				// the pooled/run pod. Mutually exclusive with RunsIn at the DSL
				// level, so this takes precedence.
				h, herr := b.EnsureScope(execCtx, stepForExec, execStepEnv(stepForExec))
				if herr != nil {
					execErr = herr
					ec = -1
				} else {
					scope = h
					ec, execErr = b.RunInScope(execCtx, h, expandedRun, execStepEnv(stepForExec), stdoutTee, shippedStderr)
				}
			} else if step.RunsIn != nil && step.RunsIn.Image != "" {
				// Isolated throwaway pod.
				ec, execErr = b.RunImage(execCtx, stepForExec, expandedRun, nil, stdoutTee, shippedStderr)
			} else if step.RunsIn != nil && step.RunsIn.Container != "" {
				ec, execErr = b.RunNamedContainer(execCtx, stepForExec, step.RunsIn.Container, expandedRun, execStepEnv(stepForExec), stdoutTee, shippedStderr)
			} else {
				// Default/main-pod (pooled or per-run) path: env has no native
				// Kubernetes-exec equivalent, so it is applied inside the exec'd
				// command itself (see execStepEnv/buildEnvShellCommand).
				ec, execErr = b.RunDefault(execCtx, stepForExec, expandedRun, execStepEnv(stepForExec), stdoutTee, shippedStderr)
			}
			finishLogs(execCtx)
			capturedStdout := stdoutBuf.String()

			status := "Succeeded"
			if execErr != nil || ec != 0 {
				status = "Failed"
			} else {
				// Evaluate output templates against the captured stdout
				capturedOutputs := map[string]string{}
				outCtx := dsl.TemplateData{Params: stepCtx.Params, Steps: stepCtx.Steps, Stdout: capturedStdout, Matrix: tplData.Matrix, Foreach: tplData.Foreach, Secrets: stepCtx.Secrets}
				for outKey, outTpl := range step.Outputs {
					if val, err := dsl.ExpandTemplate(outTpl, outCtx); err == nil {
						capturedOutputs[outKey] = val
					}
				}
				if len(capturedOutputs) > 0 {
					// k8s runs steps sequentially, so no lock is needed here.
					agentlib.ApplyStepOutputs(stepCtx.Steps, step.Name, step.MatrixKey, capturedOutputs)
					agentlib.RetryUntilSuccess(reportCtx, func(callCtx context.Context) error {
						return a.client.SetStepOutputs(callCtx, a.cfg.AgentID, c.RunID, step.Index, step.MatrixKey, capturedOutputs)
					})
				}
			}

			if status == "Succeeded" && step.Post != nil {
				hookStack = append(hookStack, postHookEntry{
					stepName:  step.Name,
					post:      *step.Post,
					scope:     scope,
					container: execContainer(step),
				})
			}

			ended := time.Now().UTC()
			agentlib.RetryUntilSuccess(reportCtx, func(callCtx context.Context) error {
				return a.client.ReportStep(callCtx, a.cfg.AgentID, api.StepReportRequest{
					RunID:      c.RunID,
					StepIndex:  step.Index,
					StageIndex: step.StageIndex,
					StepName:   step.DisplayName(),
					Variant:    step.MatrixKey,
					Status:     status,
					ExitCode:   ec,
					StartedAt:  started,
					EndedAt:    ended,
				})
			})
			if status == "Failed" {
				recordFailure(step)
			}
		}
	}

	// mainRun executes a main-stage step with live status and implicit success()
	// gating, recording non-continueOnError failures into anyStepFailed. It runs
	// with runCtx so a mid-run cancellation interrupts the in-flight step, and
	// suppresses cancel-induced failures.
	mainRun := makeRunStep(statusView, true, &anyStepFailed, runCtx, true)

	// getData snapshots the current step outputs for RunPipeline's internal
	// matrix expansion (mirrors the host agent's getData/sctx.snapshot). k8s
	// runs steps sequentially (Sequential mode below), so stepCtx.Steps is
	// never mutated concurrently with this read.
	getData := func() dsl.TemplateData {
		return dsl.TemplateData{Params: c.Params, Steps: stepCtx.Steps, Secrets: stepCtx.Secrets}
	}

	// Visit every stage/step; the if: auto-skip (implicit success()) handles
	// post-failure behavior, so the pipeline never aborts on failure. Matrix
	// expansion is handled internally by RunPipeline; Sequential mode preserves
	// the k8s agent's documented one-at-a-time execution (scopePods/hookStack
	// are not concurrency-safe).
	if err := agentlib.RunPipeline(runCtx, c.Stages, getData, c.MatrixMaxCombinations, b.ConcurrencyMode(),
		func(_ context.Context, step api.ClaimStep) error {
			mainRun(step)
			return nil
		}); err != nil {
		slog.Error("k8s: main pipeline structural error", "runId", c.RunID, "error", err)
		anyStepFailed.Store(true)
	}

	// post: hooks run regardless of DAG success/failure (mirrors the host
	// agent's postHooks/hookStack drain, internal/agent/agent.go:707-734),
	// draining LIFO so hooks unwind in the reverse order their steps
	// succeeded — before finally, using a non-cancelling context so a
	// cancelled/timed-out parent context doesn't skip cleanup. A hook failure
	// is only logged; it never flips the run status (matching the host).
	hookCtx := context.WithoutCancel(ctx)
	for i := len(hookStack) - 1; i >= 0; i-- {
		entry := hookStack[i]
		var extraEnv []string
		for k, v := range entry.post.Env {
			extraEnv = append(extraEnv, k+"="+v)
		}
		// Route the hook into the same container the step body ran in: the
		// step's scope pod (if any, via entry.scope) or the default run/pooled
		// pod otherwise. b.RunPostHook resolves scope/container the same way
		// backend.RunInScope did at step-exec time (container is meaningful on
		// k8s, unlike the host backend, which ignores it).
		if err := b.RunPostHook(hookCtx, entry.scope, entry.container, entry.post.Run, extraEnv); err != nil {
			slog.Warn("k8s: post step failed", "step", entry.stepName, "error", err)
		}
	}

	// Promote declared job outputs
	runOutputs := map[string]string{}
	for _, stage := range c.Stages {
		for _, step := range api.StageSteps(stage) {
			if sd, ok := stepCtx.Steps[step.Name]; ok {
				for _, outName := range c.JobOutputs {
					if val, ok := sd.Outputs[outName]; ok {
						runOutputs[outName] = dsl.OutputValueString(val)
					}
				}
			}
		}
	}
	if len(runOutputs) > 0 {
		agentlib.RetryUntilSuccess(reportCtx, func(callCtx context.Context) error {
			return a.client.SetRunOutputs(callCtx, a.cfg.AgentID, c.RunID, runOutputs)
		})
	}

	// Deferred cache saves: capture the final workspace after the main stages
	// (before finally, which is cleanup/notify). Best-effort — never flips status.
	// Use a non-cancelling context so a parent-context cancellation (process
	// shutdown) doesn't abort the save, matching the standard agent and the
	// k8s finally block below.
	saveCtx := context.WithoutCancel(ctx)
	for _, s := range cacheSaves {
		// s.scope was captured at restore time: for a scoped step this is the
		// scope pod (still alive here — it is only torn down by executeRun's
		// deferred backend.CloseScopes, after orchestrate returns), for a
		// non-scoped step it is the zero ScopeHandle (the run pod).
		if err := b.CacheSave(saveCtx, s.scope, s.key, s.path, s.ttlDays); err != nil {
			slog.Warn("k8s: cache save exec failed", "key", s.key, "error", err)
		}
	}

	// anyStepFailed already excludes cancel-induced failures (suppressOnCancel).
	cancelled := cancelledByMaster.Load()
	mainFailed := anyStepFailed.Load()

	// finally runs after the main stages (and output promotion) against a FROZEN
	// status snapshot, so finally steps never auto-skip one another. A no-if
	// finally step always runs (implicitSuccess=false); a finally step failure
	// flips the run to Failed. It must run even when the run was cancelled, so
	// its steps execute with a non-cancelling context (WithoutCancel), and its
	// failures are never suppressed by the cancellation.
	var finallyFailed atomic.Bool
	if len(c.Finally) > 0 {
		frozen := dsl.RunStatusView{Failed: mainFailed, Cancelled: cancelled}
		finallyCtx := context.WithoutCancel(ctx)
		finallyRun := makeRunStep(func() dsl.RunStatusView { return frozen }, false, &finallyFailed, finallyCtx, false)
		if err := agentlib.RunPipeline(finallyCtx, c.Finally, getData, c.MatrixMaxCombinations, b.ConcurrencyMode(),
			func(_ context.Context, step api.ClaimStep) error {
				finallyRun(step)
				return nil
			}); err != nil {
			slog.Error("k8s: finally pipeline structural error", "runId", c.RunID, "error", err)
			finallyFailed.Store(true)
		}
	}

	// Final status precedence: Failed > Cancelled > Succeeded.
	var overallStatus api.RunStatus
	switch {
	case mainFailed || finallyFailed.Load():
		overallStatus = api.RunFailed
	case cancelled:
		overallStatus = api.RunCancelled
	default:
		overallStatus = api.RunSucceeded
	}
	// Use a non-cancelling context so FinishRun is reliably delivered even when
	// the run was cancelled, and retry until the controller accepts it
	// (mirroring the host agent — internal/agent/agent.go:809-811).
	finishCtx := context.WithoutCancel(ctx)
	agentlib.RetryUntilSuccess(finishCtx, func(callCtx context.Context) error {
		return a.client.FinishRun(callCtx, a.cfg.AgentID, c.RunID, overallStatus)
	})
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
// normalization the canonical source is RunsIn.Container (the flat Container
// field is cleared at validation time); nil/absent means the default container.
func execContainer(s api.ClaimStep) string {
	if s.RunsIn != nil {
		return s.RunsIn.Container
	}
	return ""
}

// expandStepEnv template-expands each env value against the run's template data
// so a runsIn.image container receives resolved values (mirrors the host agent).
func expandStepEnv(env map[string]string, td dsl.TemplateData) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, len(env)+1)
	for k, v := range env {
		ev, err := dsl.ExpandTemplate(v, td)
		if err != nil {
			ev = v
		}
		out[k] = ev
	}
	return out
}

// imageStepEnv returns a fresh env map for a runsIn.image container: the step's
// env plus UNIFIED_AGENT_OS. Always a new map, so callers never mutate the claim.
// The throwaway pod runs a Linux container image regardless of the agent's host
// OS, so UNIFIED_AGENT_OS is "linux" — not the agent process's runtime.GOOS.
func imageStepEnv(step api.ClaimStep) map[string]string {
	env := make(map[string]string, len(step.Env)+1)
	for k, v := range step.Env {
		env[k] = v
	}
	env["UNIFIED_AGENT_OS"] = "linux"
	return env
}

// execStepEnv returns the "KEY=VALUE" pairs to apply at exec time for a
// default-pod or scope-pod step: UNIFIED_AGENT_OS=linux (pods are always
// Linux containers, mirroring the host agent's agentOSForStep for scoped/
// containerized steps — see internal/agent/agent.go:565) plus the step's own
// env: map (already template-expanded by the caller). Kubernetes exec has no
// native env option, so these pairs are threaded into the exec'd command via
// the `env` binary (buildEnvShellCommand) rather than baked into the pod spec
// — this keeps per-step env correct even when a step reuses a pooled/scope
// pod that was created (and had its env baked) by a different step.
func execStepEnv(step api.ClaimStep) []string {
	env := make([]string, 0, len(step.Env)+1)
	env = append(env, "UNIFIED_AGENT_OS=linux")
	for k, v := range step.Env {
		env = append(env, k+"="+v)
	}
	return env
}

// imageStepDeadline returns the throwaway pod's activeDeadlineSeconds: the step
// timeout if set, else a 1-hour default.
func imageStepDeadline(step api.ClaimStep) int64 {
	if step.TimeoutMinutes > 0 {
		return int64(step.TimeoutMinutes * 60)
	}
	return 3600
}

// runImageStep runs a runsIn.image step in a throwaway, isolated pod: create a
// pod from the image (the pod, built by buildImageStepPod, stays alive via
// sleep infinity), wait until it is running, exec the step's script into the
// single "step" container, then delete the pod. The pod is always deleted
// (defer, non-cancellable context) so a cancelled or failed step never leaks a
// pod. A failure to create/start the pod is a hard error surfaced to the step
// — never a silent fallback. The start wait is bounded by imagePodStartTimeout
// so a bad image (stuck Pending/ImagePullBackOff, which never reaches Failed
// under RestartPolicy: Never) fails fast instead of hanging until the run is
// cancelled.
func (a *K8sAgent) runImageStep(ctx context.Context, runID, image string, env map[string]string, deadlineSeconds int64, resources *dsl.ResourceSpec, script string, stdout, stderr io.Writer) (int, error) {
	pod := buildImageStepPod(runID, a.cfg.Namespace, image, env, deadlineSeconds, resources)
	created, err := a.pm.CreatePod(ctx, pod)
	if err != nil {
		return -1, fmt.Errorf("runsIn.image %q: create pod: %w", image, err)
	}
	name := created.Name
	defer func() {
		if derr := a.pm.DeletePod(context.WithoutCancel(ctx), name); derr != nil {
			slog.Warn("k8s: failed to delete image-step pod", "pod", name, "error", derr)
		}
	}()

	waitCtx, cancel := context.WithTimeout(ctx, imagePodStartTimeout)
	defer cancel()
	if err := a.pm.WaitForPodRunning(waitCtx, name); err != nil {
		return -1, fmt.Errorf("runsIn.image %q: pod did not become ready within %s (image pull may have failed): %w", image, imagePodStartTimeout, err)
	}
	// env is already baked into the pod's container spec at creation time
	// (buildImageStepPod/imageStepEnv), so no exec-time env is needed here.
	return a.exec.ExecStep(ctx, name, "step", script, nil, stdout, stderr)
}

func appendLabelIfMissing(labels []string, label string) []string {
	for _, l := range labels {
		if l == label {
			return labels
		}
	}
	return append(labels, label)
}
