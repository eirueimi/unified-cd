package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/cache"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
	"github.com/eirueimi/unified-cd/internal/secrets"
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

// approvalPollInterval is how often WaitForApproval polls the controller for a
// human decision. It is a var (not a const) so tests can shorten it.
var approvalPollInterval = 3 * time.Second

// heartbeatInterval is the interval Run uses when starting the liveness
// heartbeat. It is a var (not a const) so tests can shorten it; production code
// leaves it at DefaultHeartbeatInterval.
var heartbeatInterval = DefaultHeartbeatInterval

// postHookEntry is a post-processing entry executed after a step completes.
// scoped/sm/h carry the owning step's scope container, if any, so the post
// script runs inside the same isolated environment the step body ran in
// rather than on the host workspace. sm/h are only meaningful when scoped is
// true; the scope container is still alive when hookStack is drained (see
// executeRun: hookStack runs before the deferred scopeManager.closeAll).
type postHookEntry struct {
	stepName string
	post     api.PostStep
	scoped   bool
	sm       *scopeManager
	h        crt.ContainerHandle
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

// executeRun executes the stages contained in a ClaimResponse via RunPipeline.
// Stages run sequentially; parallel groups and foreach steps run concurrently within a stage.
func (a *Agent) executeRun(ctx context.Context, c api.ClaimResponse, workDir string) {
	slog.Info("running", "runId", c.RunID, "job", c.JobName)

	if c.PodTemplate != nil {
		slog.Error("job requires k8s-agent (podTemplate set); this agent cannot execute it", "runId", c.RunID, "job", c.JobName)
		retryUntilSuccess(ctx, func(callCtx context.Context) error {
			return a.Client.FinishRun(callCtx, a.ID, c.RunID, api.RunFailed)
		})
		return
	}

	// Apply job-level timeout to the context if one is configured
	if c.TimeoutMinutes > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(c.TimeoutMinutes*float64(time.Minute)))
		defer cancel()
	}

	var cancelledByMaster atomic.Bool
	// anyStepFailed: a non-continueOnError step failed (used for if: status).
	// Benign race: a step failing at the exact instant cancellation arrives may be
	// reported as Failed vs Cancelled, but both are terminal non-success — no corruption.
	var anyStepFailed atomic.Bool

	statusView := func() dsl.RunStatusView {
		cancelled := cancelledByMaster.Load()
		return dsl.RunStatusView{
			Failed:    anyStepFailed.Load() && !cancelled,
			Cancelled: cancelled,
		}
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				run, err := a.Client.GetRun(runCtx, c.RunID)
				if err != nil {
					slog.Warn("cancel poller: get run failed", "runID", c.RunID, "error", err)
					continue
				}
				if run.Status == api.RunCancelled {
					slog.Info("received cancellation signal from master; interrupting run", "runID", c.RunID)
					cancelledByMaster.Store(true)
					cancelRun()
					return
				}
			}
		}
	}()

	sctx := &safeStepCtx{
		data: dsl.TemplateData{
			Params: c.Params,
			Steps:  map[string]dsl.StepData{},
		},
	}

	// Fetch the secrets needed for this Run and build the masker
	var masker *secrets.Masker
	if len(c.SecretsNeeded) > 0 {
		secretValues, err := a.Client.FetchSecrets(ctx, a.ID, c.SecretsNeeded)
		if err != nil {
			slog.Warn("failed to fetch secrets, continuing without secrets", "runId", c.RunID, "error", err)
			secretValues = map[string]string{}
		}
		sctx.mu.Lock()
		sctx.data.Secrets = secretValues
		sctx.mu.Unlock()
		vals := make([]string, 0, len(secretValues))
		for _, v := range secretValues {
			vals = append(vals, v)
		}
		masker = secrets.NewMasker(vals)
	} else {
		masker = secrets.NoOpMasker
	}

	// deferred hooks: run after RunPipeline completes (cache save, etc.)
	//
	// parallel: steps in the same claim run concurrently as goroutines (see
	// runParallel in pipeline.go), and both postHooks (cache save, appended
	// from executeCacheStep) and hookStack (post: hooks, appended below in
	// makeStepRunner) are appended from inside that concurrently-invoked step
	// runner. postHooksMu guards every append to either slice so concurrent
	// parallel-group members with a post:/cache: don't race on the shared
	// backing array. The drain loops below run after RunPipeline returns
	// (i.e. after all step-runner goroutines have finished), so the drain
	// itself does not need the lock.
	var postHooksMu sync.Mutex
	var postHooks []func(context.Context)
	var hookStack []postHookEntry

	// scopes: one scopeManager for the whole claim, created lazily on first use
	// by a uses-scope step. Torn down at claim end regardless of how the claim
	// finished (success, failure, or cancellation).
	//
	// parallel: steps in the same claim run concurrently as goroutines (see
	// runParallel in pipeline.go), so getScopes can be called from multiple
	// goroutines at once. scopesMu guards the nil-check-and-assign so exactly
	// one scopeManager is ever created per claim, instead of a data race that
	// could construct two managers (leaking/duplicating scope containers) or
	// trip Go's "concurrent map writes" panic in scopeManager.open.
	var scopes *scopeManager
	var scopesMu sync.Mutex
	getScopes := func() (*scopeManager, error) {
		scopesMu.Lock()
		defer scopesMu.Unlock()
		if scopes != nil {
			return scopes, nil
		}
		rt, err := a.containerRuntime()
		if err != nil {
			return nil, fmt.Errorf("uses-scope requires a container runtime: %w", err)
		}
		scopes = newScopeManager(rt)
		return scopes, nil
	}
	defer func() {
		if scopes != nil {
			scopes.closeAll(context.WithoutCancel(ctx))
		}
	}()

	getData := func() dsl.TemplateData { return sctx.snapshot() }

	// makeStepRunner builds the per-step execution function. It is reused for the
	// main DAG and for the finally block, parametrized by:
	//   statusFn        — supplies the RunStatusView used to evaluate if:
	//                     (live status for the main DAG, frozen status for finally)
	//   implicitSuccess — true for the main DAG (auto-skip after a failure),
	//                     false for finally (no-if steps always run)
	//   failedFlag      — set when a non-continueOnError step fails
	//   suppressOnCancel — true for the main DAG (cancellation does not count as a
	//                      failure), false for finally (a genuine finally failure
	//                      counts even when the run was cancelled)
	makeStepRunner := func(statusFn func() dsl.RunStatusView, implicitSuccess bool, failedFlag *atomic.Bool, suppressOnCancel bool) func(context.Context, api.ClaimStep) error {
		return func(stepCtx context.Context, step api.ClaimStep) error {
			// recordFailure records a non-continueOnError failure into failedFlag,
			// honouring suppressOnCancel (cancellation does not mask finally failures).
			recordFailure := func() {
				if step.ContinueOnError {
					return
				}
				if suppressOnCancel && cancelledByMaster.Load() {
					return
				}
				failedFlag.Store(true)
			}

			// markFailed records a failure (via recordFailure) and reports the step as
			// Failed. Used by the cache/artifact branches, which otherwise would not
			// report a Failed status when their internal helper returns an error.
			markFailed := func(reportCtx context.Context) {
				recordFailure()
				_ = a.Client.ReportStep(reportCtx, a.ID, api.StepReportRequest{
					RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex,
					StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Failed", EndedAt: time.Now().UTC(),
				})
			}

			// if: evaluate condition against the supplied run status. For the main DAG
			// every step is evaluated — including steps with an empty if: — so that a
			// normal step auto-skips once a prior step has failed (implicitSuccess). For
			// finally the status is frozen and implicitSuccess is false. If false, skip.
			ifData := sctx.snapshot()
			ok, err := dsl.EvalCondition(step.If, ifData, statusFn(), implicitSuccess)
			if err != nil {
				slog.Warn("if: condition eval failed, running step", "step", step.Name, "error", err)
			}
			if !ok {
				retryUntilSuccess(ctx, func(callCtx context.Context) error {
					return a.Client.ReportStep(callCtx, a.ID, api.StepReportRequest{
						RunID:      c.RunID,
						StepIndex:  step.Index,
						StageIndex: step.StageIndex,
						StepName:   step.DisplayName(),
						Variant:    step.MatrixKey,
						Status:     "Skipped",
					})
				})
				return nil
			}
			// Apply step-level timeout to the context if one is configured
			if step.TimeoutMinutes > 0 {
				var stepCancel context.CancelFunc
				stepCtx, stepCancel = context.WithTimeout(stepCtx, time.Duration(step.TimeoutMinutes*float64(time.Minute)))
				defer stepCancel()
			}

			// approval gate: report WaitingApproval, poll for the human decision.
			// Placed after the if: gate so an approval step can itself be if:-gated.
			if step.Approval != nil {
				approved := WaitForApproval(stepCtx, a.Client, a.ID, c.RunID, step, approvalPollInterval)
				if approved {
					_ = a.Client.ReportStep(stepCtx, a.ID, api.StepReportRequest{
						RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex,
						StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Succeeded", EndedAt: time.Now().UTC(),
					})
				} else {
					_ = a.Client.ReportStep(stepCtx, a.ID, api.StepReportRequest{
						RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex,
						StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Failed", EndedAt: time.Now().UTC(),
					})
					recordFailure()
				}
				return nil
			}

			// cache steps: restore immediately, defer save to postHooks
			if step.Cache != nil {
				sm, h, serr := resolveScopeHandle(stepCtx, step, getScopes)
				if serr != nil {
					// Cache stays warn+skip (lenient policy), matching the
					// k8s agent: a scope pod/container that never becomes
					// available must not fail the step, just skip the cache
					// operation (no restore, no deferred save). Unlike
					// artifact upload/download, which remain fail-loud.
					slog.Warn("cache scope unavailable; skipping cache for step", "step", step.Name, "error", serr)
					_ = a.Client.ReportStep(context.WithoutCancel(stepCtx), a.ID, api.StepReportRequest{
						RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex,
						StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Succeeded", EndedAt: time.Now().UTC(),
					})
					return nil
				}
				if err := a.executeCacheStep(stepCtx, step, c.RunID, sctx, &postHooksMu, &postHooks, sm, h); err != nil {
					slog.Error("cache step failed", "step", step.Name, "error", err)
					markFailed(context.WithoutCancel(stepCtx))
				}
				return nil
			}
			if step.UploadArtifact != nil {
				sm, h, serr := resolveScopeHandle(stepCtx, step, getScopes)
				if serr != nil {
					slog.Error("upload artifact failed", "step", step.Name, "error", serr)
					markFailed(context.WithoutCancel(stepCtx))
					return nil
				}
				if err := a.executeUploadArtifact(stepCtx, step, c.RunID, workDir, sm, h); err != nil {
					slog.Error("upload artifact failed", "step", step.Name, "error", err)
					markFailed(context.WithoutCancel(stepCtx))
				}
				return nil
			}
			if step.DownloadArtifact != nil {
				sm, h, serr := resolveScopeHandle(stepCtx, step, getScopes)
				if serr != nil {
					slog.Error("download artifact failed", "step", step.Name, "error", serr)
					markFailed(context.WithoutCancel(stepCtx))
					return nil
				}
				if err := a.executeDownloadArtifact(stepCtx, step, c.RunID, workDir, sm, h); err != nil {
					slog.Error("download artifact failed", "step", step.Name, "error", err)
					markFailed(context.WithoutCancel(stepCtx))
				}
				return nil
			}

			started := time.Now().UTC()
			_ = a.Client.ReportStep(stepCtx, a.ID, api.StepReportRequest{
				RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Running", StartedAt: started,
			})

			status := "Succeeded"
			exitCode := 0
			tplData := sctx.snapshot()
			if step.MatrixValues != nil {
				tplData.Matrix = step.MatrixValues
				tplData.Foreach = step.MatrixValues // foreach sugar compatibility: {{ .Foreach.key }}
			}

			var callChildRunID, callJobName string
			// stepScope/stepScopeHandle capture this step's scope container (if
			// any), set inside the isScopedStep run-branch case below. A step's
			// post: hook must execute inside the same scope container the step
			// body ran in (not the host workspace), so postHookEntry carries
			// these through to the hookStack drain in the finally block.
			var stepScope *scopeManager
			var stepScopeHandle crt.ContainerHandle

			if step.Call != nil {
				childOutputs, childRunID, callErr := a.executeCallStep(stepCtx, c.RunID, step, tplData)
				callChildRunID = childRunID
				callJobName = step.Call.Job
				if callErr != nil {
					slog.Error("call step failed", "step", step.Name, "error", callErr)
					status = "Failed"
				} else {
					if step.MatrixKey != "" {
						sctx.setStepMatrixOutputs(step.Name, step.MatrixKey, childOutputs)
					} else {
						sctx.setStep(step.Name, dsl.StepData{Outputs: dsl.StringOutputs(childOutputs)})
					}
					if len(childOutputs) > 0 {
						_ = a.Client.SetStepOutputs(stepCtx, a.ID, c.RunID, step.Index, step.MatrixKey, childOutputs)
					}
				}
			} else {
				expandedRun, tplErr := dsl.ExpandTemplate(step.Run, tplData)
				if tplErr != nil {
					slog.Error("template expansion failed", "step", step.Name, "error", tplErr)
					expandedRun = step.Run
				}

				// UNIFIED_AGENT_OS lets job authors determine the running OS from within a step.
				// Scoped / runsIn.image steps run in a Linux container, not on the host — see agentOSForStep.
				extraEnv := []string{"UNIFIED_AGENT_OS=" + agentOSForStep(step)}
				for k, v := range step.Env {
					expanded, _ := dsl.ExpandTemplate(v, tplData)
					extraEnv = append(extraEnv, k+"="+expanded)
				}

				stderrPusher := NewLogPusher(a.Client, a.ID, c.RunID, step.Index, "stderr")
				stderrPusher.SetMasker(masker)
				// stdout is teed: streamed to the server while the step runs
				// (mirroring the k8s agent's io.MultiWriter approach) AND kept in
				// stdoutBuf for {{ .Stdout }} output-template evaluation below.
				stdoutPusher := NewLogPusher(a.Client, a.ID, c.RunID, step.Index, "stdout")
				stdoutPusher.SetMasker(masker)
				flushCtx, stopAutoFlush := context.WithCancel(stepCtx)
				stderrPusher.StartAutoFlush(flushCtx, logPusherAutoFlushEvery)
				stdoutPusher.StartAutoFlush(flushCtx, logPusherAutoFlushEvery)
				var stdoutBuf bytes.Buffer
				stdoutTee := io.MultiWriter(&stdoutBuf, stdoutPusher)
				var ec int
				var runErr error
				switch {
				case isScopedStep(step):
					// Scoped steps never carry RunsIn (mutually exclusive at the DSL
					// level), so this case takes precedence over the RunsIn cases below.
					sm, serr := getScopes()
					if serr != nil {
						runErr = serr
						ec = -1
						break
					}
					h, herr := sm.ensure(stepCtx, step, extraEnv)
					if herr != nil {
						runErr = herr
						ec = -1
						break
					}
					stepScope, stepScopeHandle = sm, h
					ec, runErr = sm.exec(stepCtx, h, expandedRun, extraEnv, stdoutTee, stderrPusher)
				case step.RunsIn != nil && step.RunsIn.Container != "":
					runErr = fmt.Errorf("runsIn.container (%q) is not supported on the host agent; use runsIn.image or the k8s agent", step.RunsIn.Container)
					ec = -1
				case step.RunsIn != nil && step.RunsIn.Image != "":
					rt, derr := a.containerRuntime()
					if derr != nil {
						runErr = fmt.Errorf("runsIn.image %q requires a container runtime: %w", step.RunsIn.Image, derr)
						ec = -1
					} else {
						cpuLimit, memLimit := hostContainerLimits(step.RunsIn.Resources)
						ec, runErr = RunStepContainer(stepCtx, rt, step.RunsIn.Image, expandedRun, stdoutTee, stderrPusher, extraEnv, cpuLimit, memLimit)
					}
				default:
					ec, runErr = RunStep(stepCtx, expandedRun, stdoutTee, stderrPusher, extraEnv, workDir)
				}
				capturedStdout := stdoutBuf.String()
				exitCode = ec
				stopAutoFlush()
				stderrPusher.Flush(stepCtx)
				stdoutPusher.Flush(stepCtx)

				if runErr != nil || ec != 0 {
					status = "Failed"
					// A step interrupted specifically because the master cancelled the
					// run (as opposed to a step/job timeout, which is a genuine
					// failure) should be reported as Cancelled rather than Failed so it
					// doesn't linger as "Running" in the UI/DB — Cancelled is a terminal
					// status the step-status CHECK constraint already allows.
					if runErr != nil && cancelledByMaster.Load() {
						status = "Cancelled"
					}
				} else {
					capturedOutputs := map[string]string{}
					outputCtx := dsl.TemplateData{
						Params:  tplData.Params,
						Steps:   tplData.Steps,
						Stdout:  capturedStdout,
						Secrets: tplData.Secrets,
						Matrix:  tplData.Matrix,
						Foreach: tplData.Foreach,
					}
					for outKey, outTpl := range step.Outputs {
						val, err := dsl.ExpandTemplate(outTpl, outputCtx)
						if err != nil {
							slog.Warn("output template evaluation failed", "step", step.Name, "key", outKey, "error", err)
							continue
						}
						capturedOutputs[outKey] = val
					}
					if step.MatrixKey != "" {
						sctx.setStepMatrixOutputs(step.Name, step.MatrixKey, capturedOutputs)
					} else {
						sctx.setStep(step.Name, dsl.StepData{Outputs: dsl.StringOutputs(capturedOutputs)})
					}
					if len(capturedOutputs) > 0 {
						_ = a.Client.SetStepOutputs(stepCtx, a.ID, c.RunID, step.Index, step.MatrixKey, capturedOutputs)
					}
				}
			}

			if status == "Succeeded" && step.Post != nil {
				postHooksMu.Lock()
				hookStack = append(hookStack, postHookEntry{
					stepName: step.Name,
					post:     *step.Post,
					scoped:   isScopedStep(step),
					sm:       stepScope,
					h:        stepScopeHandle,
				})
				postHooksMu.Unlock()
			}

			ended := time.Now().UTC()
			// Use a non-cancelling context for the retry so that ReportStep is reliably called
			// even when stepCtx has been cancelled due to timeout or other reasons.
			reportCtx := context.WithoutCancel(stepCtx)
			reportReq := api.StepReportRequest{
				RunID:       c.RunID,
				StepIndex:   step.Index,
				StageIndex:  step.StageIndex,
				StepName:    step.DisplayName(),
				Variant:     step.MatrixKey,
				Status:      status,
				ExitCode:    exitCode,
				StartedAt:   started,
				EndedAt:     ended,
				ChildRunID:  callChildRunID,
				CallJobName: callJobName,
			}
			retryUntilSuccess(reportCtx, func(callCtx context.Context) error {
				return a.Client.ReportStep(callCtx, a.ID, reportReq)
			})
			if status == "Failed" {
				recordFailure()
				return nil
			}
			return nil
		}
	}

	mainRunner := makeStepRunner(statusView, true, &anyStepFailed, true)
	dagErr := RunPipeline(runCtx, c.Stages, getData, c.MatrixMaxCombinations, mainRunner)

	// post-hooks run regardless of DAG success/failure (cache save should always attempt).
	// Use WithoutCancel so a cancelled parent context doesn't skip cache saves.
	hookCtx := context.WithoutCancel(ctx)
	for _, fn := range postHooks {
		fn(hookCtx)
	}
	for i := len(hookStack) - 1; i >= 0; i-- {
		entry := hookStack[i]
		cmd := entry.post.Run
		var extraEnv []string
		for k, v := range entry.post.Env {
			extraEnv = append(extraEnv, k+"="+v)
		}
		if entry.scoped {
			// The owning step ran in an isolated scope container; its post:
			// hook must run there too, not on the host workspace. The scope
			// container is still alive here — hookStack is drained before the
			// deferred scopeManager.closeAll runs (see the `defer` registered
			// alongside getScopes above).
			if _, runErr := entry.sm.exec(hookCtx, entry.h, cmd, extraEnv, nil, nil); runErr != nil {
				slog.Warn("post step failed", "step", entry.stepName, "error", runErr)
			}
			continue
		}
		if _, _, runErr := RunStepCapture(hookCtx, cmd, nil, extraEnv, workDir); runErr != nil {
			slog.Warn("post step failed", "step", entry.stepName, "error", runErr)
		}
	}

	// Freeze the main-DAG status for finally if: evaluation.
	cancelled := cancelledByMaster.Load()
	mainFailed := anyStepFailed.Load() || (dagErr != nil && !cancelled)

	// finally runs after the main DAG on success, failure, AND cancellation. Its if:
	// conditions are evaluated against a frozen main status (so finally steps never
	// auto-skip one another) with implicitSuccess=false (a no-if finally step always
	// runs). A finally step failure flips the run to Failed even on cancellation.
	var finallyFailed atomic.Bool
	if len(c.Finally) > 0 {
		frozen := dsl.RunStatusView{Failed: mainFailed, Cancelled: cancelled}
		finallyStatus := func() dsl.RunStatusView { return frozen }
		finallyRunner := makeStepRunner(finallyStatus, false, &finallyFailed, false)
		// Use a non-cancelling context so finally runs even when the run was cancelled.
		finallyCtx := context.WithoutCancel(ctx)
		if err := RunPipeline(finallyCtx, c.Finally, getData, c.MatrixMaxCombinations, finallyRunner); err != nil {
			slog.Warn("finally: structural error", "runId", c.RunID, "error", err)
			finallyFailed.Store(true)
		}
	}

	var overallStatus api.RunStatus
	switch {
	case mainFailed || finallyFailed.Load():
		overallStatus = api.RunFailed
	case cancelled:
		overallStatus = api.RunCancelled
	default:
		overallStatus = api.RunSucceeded
	}

	// Use a non-cancelling context so that FinishRun and SetRunOutputs are reliably called
	// even when ctx has been cancelled due to timeout or other reasons.
	finishCtx := context.WithoutCancel(ctx)

	// Promote declared job outputs (only from steps that actually executed)
	runOutputs := map[string]string{}
	finalData := sctx.snapshot()
	for _, outName := range c.JobOutputs {
		for _, stage := range c.Stages {
			for _, step := range api.StageSteps(stage) {
				if sd, ok := finalData.Steps[step.Name]; ok {
					if val, ok := sd.Outputs[outName]; ok {
						runOutputs[outName] = dsl.OutputValueString(val)
					}
				}
			}
		}
	}
	if len(runOutputs) > 0 {
		_ = a.Client.SetRunOutputs(finishCtx, a.ID, c.RunID, runOutputs)
	}

	retryUntilSuccess(finishCtx, func(callCtx context.Context) error {
		return a.Client.FinishRun(callCtx, a.ID, c.RunID, overallStatus)
	})
}

// executeCallStep launches a child Run and polls until it completes.
// Returns the child Run's outputs and the child Run's ID (so the caller can
// report it on the step's terminal StepReport for caller→child linking in the
// WebUI). childRunID is "" only if the child run was never created (param
// template failure, or the create request itself failed); on every other
// path (success, failure, cancellation, timeout) it is returned alongside
// the error so the link is preserved even for failed calls.
// runID is the PARENT run's ID, used to publish the child link on a
// non-terminal step report as soon as the child is created (see below).
func (a *Agent) executeCallStep(ctx context.Context, runID string, step api.ClaimStep, tplData dsl.TemplateData) (outputs map[string]string, childRunID string, err error) {
	// Expand templates in the call parameters.
	// Stdout is not exposed to prevent previous step output from leaking into child job parameters.
	// Expansion errors fail the step: these values become the child run's
	// inputs, and silently forwarding a raw unexpanded template (e.g. a
	// literal "{{ .RunID }}") hides the mistake until it surfaces in the
	// child job or an external webhook. Matches the cache-step precedent.
	callCtx := dsl.TemplateData{Params: tplData.Params, Steps: tplData.Steps}
	expandedParams := map[string]string{}
	for k, v := range step.Call.Params {
		expanded, err := dsl.ExpandTemplate(v, callCtx)
		if err != nil {
			return nil, "", fmt.Errorf("call param %q template: %w", k, err)
		}
		expandedParams[k] = expanded
	}

	childRun, err := a.Client.CreateRun(ctx, step.Call.Job, expandedParams)
	if err != nil {
		return nil, "", fmt.Errorf("create child run for job %q: %w", step.Call.Job, err)
	}
	slog.Info("call: child run created", "childRunId", childRun.ID, "job", step.Call.Job)

	// Publish the caller→child link immediately on a non-terminal report so the
	// WebUI can navigate to the child while it is still running (long child jobs
	// are exactly when the link matters). StartedAt/EndedAt stay zero: the
	// controller maps zero times to NULL and the UPSERT's COALESCE preserves the
	// values from the initial Running report. The terminal report re-sends the
	// link, so a report lost here self-heals; failure to send is non-fatal.
	_ = a.Client.ReportStep(ctx, a.ID, api.StepReportRequest{
		RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex,
		StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Running",
		ChildRunID: childRun.ID, CallJobName: step.Call.Job,
	})

	const maxWait = 30 * time.Minute
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	deadline := time.Now().Add(maxWait)

	for {
		run, err := a.Client.GetRun(ctx, childRun.ID)
		if err != nil {
			slog.Warn("call: poll child run failed", "childRunId", childRun.ID, "error", err)
		} else {
			switch run.Status {
			case api.RunSucceeded:
				outputs, oErr := a.Client.GetRunOutputs(ctx, childRun.ID)
				if oErr != nil {
					slog.Warn("call: get child outputs failed", "childRunId", childRun.ID, "error", oErr)
					outputs = map[string]string{}
				}
				return outputs, childRun.ID, nil
			case api.RunFailed, api.RunCancelled:
				return nil, childRun.ID, fmt.Errorf("call: child run %s finished with status %s", childRun.ID, run.Status)
			}
		}

		if time.Now().After(deadline) {
			return nil, childRun.ID, fmt.Errorf("call: child run %s timed out after %s", childRun.ID, maxWait)
		}
		select {
		case <-ctx.Done():
			// child run orphaned; log for visibility
			slog.Warn("call: parent context cancelled, child run may be orphaned", "childRunId", childRun.ID)
			return nil, childRun.ID, ctx.Err()
		case <-ticker.C:
		}
	}
}

// resolveScopeHandle returns the (scopeManager, ContainerHandle) pair for a
// scoped step's cache/artifact operations, creating the claim's scopeManager
// and the step's scope container on first use. For non-scoped steps it
// returns (nil, zero handle, nil) so callers can branch on sm == nil.
// A scoped step that cannot obtain a runtime or container is a hard error
// (no silent fallback to the host workspace).
func resolveScopeHandle(ctx context.Context, step api.ClaimStep, getScopes func() (*scopeManager, error)) (*scopeManager, crt.ContainerHandle, error) {
	if !isScopedStep(step) {
		return nil, crt.ContainerHandle{}, nil
	}
	sm, err := getScopes()
	if err != nil {
		return nil, crt.ContainerHandle{}, err
	}
	h, err := sm.ensure(ctx, step, nil)
	if err != nil {
		return nil, crt.ContainerHandle{}, err
	}
	return sm, h, nil
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

func (a *Agent) executeUploadArtifact(ctx context.Context, step api.ClaimStep, runID string, workDir string, sm *scopeManager, h crt.ContainerHandle) error {
	started := time.Now().UTC()
	_ = a.Client.ReportStep(ctx, a.ID, api.StepReportRequest{
		RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Running", StartedAt: started,
	})

	ua := step.UploadArtifact
	var artifactPath string
	if sm != nil {
		// ua.Path is a CONTAINER-side path; resolve it against the scope
		// container's working directory before copyOutToTemp so it is always
		// absolute (mirrors the k8s agent's path.Join(mountPath, ...)).
		p, cleanup, err := sm.copyOutToTemp(ctx, h, resolveScopePath(ua.Path))
		if err != nil {
			// fail-loud: artifact operations do not silently skip on error.
			slog.Error("upload-artifact failed", "step", step.Name, "error", err)
			_ = a.Client.ReportStep(ctx, a.ID, api.StepReportRequest{
				RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Failed",
				StartedAt: started, EndedAt: time.Now().UTC(),
			})
			return fmt.Errorf("upload-artifact %q: copy from scope: %w", ua.Name, err)
		}
		defer cleanup()
		artifactPath = p
	} else {
		artifactPath = resolveWorkspacePath(workDir, ua.Path)
	}
	if err := a.Client.UploadArtifact(ctx, runID, ua.Name, artifactPath); err != nil {
		slog.Error("upload-artifact failed", "step", step.Name, "error", err)
		_ = a.Client.ReportStep(ctx, a.ID, api.StepReportRequest{
			RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Failed",
			StartedAt: started, EndedAt: time.Now().UTC(),
		})
		return fmt.Errorf("upload-artifact %q: %w", ua.Name, err)
	}
	_ = a.Client.ReportStep(ctx, a.ID, api.StepReportRequest{
		RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Succeeded",
		StartedAt: started, EndedAt: time.Now().UTC(),
	})
	return nil
}

func (a *Agent) executeDownloadArtifact(ctx context.Context, step api.ClaimStep, runID string, workDir string, sm *scopeManager, h crt.ContainerHandle) error {
	started := time.Now().UTC()
	_ = a.Client.ReportStep(ctx, a.ID, api.StepReportRequest{
		RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Running", StartedAt: started,
	})

	da := step.DownloadArtifact
	destDir := da.DestDir
	if destDir == "" {
		destDir = "."
	}

	var hostDestDir string
	var cleanup func()
	if sm != nil {
		tmp, err := os.MkdirTemp("", "ucd-scope-download-")
		if err != nil {
			slog.Error("download-artifact failed", "step", step.Name, "error", err)
			_ = a.Client.ReportStep(ctx, a.ID, api.StepReportRequest{
				RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Failed",
				StartedAt: started, EndedAt: time.Now().UTC(),
			})
			return fmt.Errorf("download-artifact %q: create temp dir: %w", da.Name, err)
		}
		hostDestDir = tmp
		cleanup = func() { _ = os.RemoveAll(tmp) }
	} else {
		hostDestDir = resolveWorkspacePath(workDir, destDir)
	}
	if cleanup != nil {
		defer cleanup()
	}

	if err := a.Client.DownloadArtifact(ctx, runID, da.Name, hostDestDir); err != nil {
		slog.Error("download-artifact failed", "step", step.Name, "error", err)
		_ = a.Client.ReportStep(ctx, a.ID, api.StepReportRequest{
			RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Failed",
			StartedAt: started, EndedAt: time.Now().UTC(),
		})
		return fmt.Errorf("download-artifact %q: %w", da.Name, err)
	}

	if sm != nil {
		// destDir (default ".") is a CONTAINER-side path; resolve it against
		// the scope container's working directory before copyIn so it is
		// always absolute (mirrors the k8s agent's path.Join(mountPath, dest)).
		if err := sm.copyIn(ctx, h, hostDestDir, resolveScopePath(destDir)); err != nil {
			slog.Error("download-artifact failed", "step", step.Name, "error", err)
			_ = a.Client.ReportStep(ctx, a.ID, api.StepReportRequest{
				RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Failed",
				StartedAt: started, EndedAt: time.Now().UTC(),
			})
			return fmt.Errorf("download-artifact %q: copy into scope: %w", da.Name, err)
		}
	}

	_ = a.Client.ReportStep(ctx, a.ID, api.StepReportRequest{
		RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Succeeded",
		StartedAt: started, EndedAt: time.Now().UTC(),
	})
	return nil
}

func (a *Agent) executeCacheStep(
	ctx context.Context,
	step api.ClaimStep,
	runID string,
	sctx *safeStepCtx,
	postHooksMu *sync.Mutex,
	postHooks *[]func(context.Context),
	sm *scopeManager,
	h crt.ContainerHandle,
) error {
	started := time.Now().UTC()
	_ = a.Client.ReportStep(ctx, a.ID, api.StepReportRequest{
		RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Running", StartedAt: started,
	})

	cs := step.Cache
	tplData := sctx.snapshot()

	key, err := dsl.ExpandTemplate(cs.Key, tplData)
	if err != nil {
		return fmt.Errorf("cache key template: %w", err)
	}
	cachePath, err := dsl.ExpandTemplate(cs.Path, tplData)
	if err != nil {
		return fmt.Errorf("cache path template: %w", err)
	}
	if cachePath == "" {
		return fmt.Errorf("cache path template %q expanded to empty string", cs.Path)
	}
	// When scoped, cachePath is a CONTAINER-side path; resolve it against the
	// scope container's working directory so it is always absolute before
	// copyIn/copyOut (mirrors the k8s agent's path.Join(mount, expandedPath)).
	// Left unresolved (as-authored) for the non-scoped host-workspace path.
	scopedCachePath := cachePath
	if sm != nil {
		scopedCachePath = resolveScopePath(cachePath)
	}
	var restoreKeys []string
	for _, rk := range cs.RestoreKeys {
		expanded, _ := dsl.ExpandTemplate(rk, tplData)
		if expanded != "" {
			restoreKeys = append(restoreKeys, expanded)
		}
	}

	// Cache stays warn+skip on error (lenient policy): a restore/save problem
	// should not fail the step, unlike artifact upload/download.
	if a.CacheStore != nil {
		if sm != nil {
			hostDir, err := os.MkdirTemp("", "ucd-scope-cache-restore-")
			if err != nil {
				slog.Warn("cache restore error", "step", step.Name, "error", err)
			} else {
				hit, rerr := cache.Restore(ctx, a.CacheStore, hostDir, key, restoreKeys)
				if rerr != nil && !errors.Is(rerr, cache.ErrCacheMiss) {
					slog.Warn("cache restore error", "step", step.Name, "error", rerr)
				} else if hit {
					if cerr := sm.copyIn(ctx, h, hostDir, scopedCachePath); cerr != nil {
						slog.Warn("cache restore error", "step", step.Name, "error", cerr)
					} else {
						slog.Info("cache hit", "step", step.Name, "key", key)
					}
				} else {
					slog.Info("cache miss", "step", step.Name, "key", key)
				}
				_ = os.RemoveAll(hostDir)
			}
		} else {
			hit, err := cache.Restore(ctx, a.CacheStore, cachePath, key, restoreKeys)
			if err != nil && !errors.Is(err, cache.ErrCacheMiss) {
				slog.Warn("cache restore error", "step", step.Name, "error", err)
			} else if hit {
				slog.Info("cache hit", "step", step.Name, "key", key)
			} else {
				slog.Info("cache miss", "step", step.Name, "key", key)
			}
		}
	}

	ttlDays := cs.TTLDays
	if ttlDays == 0 {
		ttlDays = 30
	}
	capturedPath := scopedCachePath
	capturedKey := key
	postHooksMu.Lock()
	*postHooks = append(*postHooks, func(hookCtx context.Context) {
		if a.CacheStore == nil {
			return
		}
		if sm != nil {
			hostPath, cleanup, err := sm.copyOutToTemp(hookCtx, h, capturedPath)
			if err != nil {
				slog.Warn("cache save failed", "key", capturedKey, "error", err)
				return
			}
			defer cleanup()
			if err := cache.Save(hookCtx, a.CacheStore, hostPath, capturedKey, ttlDays); err != nil {
				slog.Warn("cache save failed", "key", capturedKey, "error", err)
			} else {
				slog.Info("cache saved", "key", capturedKey)
			}
			return
		}
		if err := cache.Save(hookCtx, a.CacheStore, capturedPath, capturedKey, ttlDays); err != nil {
			slog.Warn("cache save failed", "key", capturedKey, "error", err)
		} else {
			slog.Info("cache saved", "key", capturedKey)
		}
	})
	postHooksMu.Unlock()

	_ = a.Client.ReportStep(ctx, a.ID, api.StepReportRequest{
		RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Succeeded", StartedAt: started, EndedAt: time.Now().UTC(),
	})
	return nil
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
