package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/cache"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/secrets"
)

// approvalPollInterval is how often WaitForApproval polls the controller for a
// human decision. It is a var (not a const) so tests can shorten it.
var approvalPollInterval = 3 * time.Second

// postHookEntry is a post-processing entry executed after a step completes.
type postHookEntry struct {
	stepName string
	post     api.PostStep
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

	StartHeartbeat(ctx, a.Client, a.ID, DefaultHeartbeatInterval)

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
	var postHooks []func(context.Context)
	var hookStack []postHookEntry

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
				if err := a.executeCacheStep(stepCtx, step, c.RunID, sctx, &postHooks); err != nil {
					slog.Error("cache step failed", "step", step.Name, "error", err)
					markFailed(context.WithoutCancel(stepCtx))
				}
				return nil
			}
			if step.UploadArtifact != nil {
				if err := a.executeUploadArtifact(stepCtx, step, c.RunID, workDir); err != nil {
					slog.Error("upload artifact failed", "step", step.Name, "error", err)
					markFailed(context.WithoutCancel(stepCtx))
				}
				return nil
			}
			if step.DownloadArtifact != nil {
				if err := a.executeDownloadArtifact(stepCtx, step, c.RunID, workDir); err != nil {
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
				tplData.Foreach = step.MatrixValues // foreach シュガー互換: {{ .Foreach.key }}
			}

			if step.Call != nil {
				childOutputs, callErr := a.executeCallStep(stepCtx, step, tplData)
				if callErr != nil {
					slog.Error("call step failed", "step", step.Name, "error", callErr)
					status = "Failed"
				} else {
					sctx.setStep(step.Name, dsl.StepData{Outputs: dsl.StringOutputs(childOutputs)})
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

				// UNIFIED_AGENT_OS allows job authors to determine the running OS from within a step.
				extraEnv := []string{"UNIFIED_AGENT_OS=" + runtime.GOOS}
				for k, v := range step.Env {
					expanded, _ := dsl.ExpandTemplate(v, tplData)
					extraEnv = append(extraEnv, k+"="+expanded)
				}

				stderrPusher := NewLogPusher(a.Client, a.ID, c.RunID, step.Index, "stderr")
				stderrPusher.SetMasker(masker)
				capturedStdout, ec, runErr := RunStepCapture(stepCtx, expandedRun, stderrPusher, extraEnv, workDir)
				exitCode = ec
				stderrPusher.Flush(stepCtx)

				for _, line := range splitLines(capturedStdout) {
					maskedLine := masker.Mask(line)
					_ = a.Client.AppendLog(stepCtx, a.ID, api.LogAppendRequest{
						RunID:     c.RunID,
						StepIndex: step.Index,
						Stream:    "stdout",
						Timestamp: time.Now().UTC(),
						Line:      maskedLine,
					})
				}

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
				hookStack = append(hookStack, postHookEntry{
					stepName: step.Name,
					post:     *step.Post,
				})
			}

			ended := time.Now().UTC()
			// Use a non-cancelling context for the retry so that ReportStep is reliably called
			// even when stepCtx has been cancelled due to timeout or other reasons.
			reportCtx := context.WithoutCancel(stepCtx)
			reportReq := api.StepReportRequest{
				RunID:      c.RunID,
				StepIndex:  step.Index,
				StageIndex: step.StageIndex,
				StepName:   step.DisplayName(),
				Variant:    step.MatrixKey,
				Status:     status,
				ExitCode:   exitCode,
				StartedAt:  started,
				EndedAt:    ended,
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
// Returns the child Run's outputs.
func (a *Agent) executeCallStep(ctx context.Context, step api.ClaimStep, tplData dsl.TemplateData) (map[string]string, error) {
	// Expand templates in the call parameters.
	// Stdout is not exposed to prevent previous step output from leaking into child job parameters.
	callCtx := dsl.TemplateData{Params: tplData.Params, Steps: tplData.Steps}
	expandedParams := map[string]string{}
	for k, v := range step.Call.Params {
		expanded, err := dsl.ExpandTemplate(v, callCtx)
		if err != nil {
			slog.Warn("call param template expand failed", "step", step.Name, "key", k, "error", err)
			expanded = v
		}
		expandedParams[k] = expanded
	}

	childRun, err := a.Client.CreateRun(ctx, step.Call.Job, expandedParams)
	if err != nil {
		return nil, fmt.Errorf("create child run for job %q: %w", step.Call.Job, err)
	}
	slog.Info("call: child run created", "childRunId", childRun.ID, "job", step.Call.Job)

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
				return outputs, nil
			case api.RunFailed, api.RunCancelled:
				return nil, fmt.Errorf("call: child run %s finished with status %s", childRun.ID, run.Status)
			}
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("call: child run %s timed out after %s", childRun.ID, maxWait)
		}
		select {
		case <-ctx.Done():
			// child run orphaned; log for visibility
			slog.Warn("call: parent context cancelled, child run may be orphaned", "childRunId", childRun.ID)
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// resolveWorkspacePath joins a relative path against the run's workspace working
// directory (the same directory ExecStep/shell steps use as their cwd, e.g.
// "<workspaceDir>/working<N>"). Absolute paths are returned unchanged.
func resolveWorkspacePath(workDir, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(workDir, path)
}

func (a *Agent) executeUploadArtifact(ctx context.Context, step api.ClaimStep, runID string, workDir string) error {
	started := time.Now().UTC()
	_ = a.Client.ReportStep(ctx, a.ID, api.StepReportRequest{
		RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Running", StartedAt: started,
	})

	ua := step.UploadArtifact
	path := resolveWorkspacePath(workDir, ua.Path)
	if err := a.Client.UploadArtifact(ctx, runID, ua.Name, path); err != nil {
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

func (a *Agent) executeDownloadArtifact(ctx context.Context, step api.ClaimStep, runID string, workDir string) error {
	started := time.Now().UTC()
	_ = a.Client.ReportStep(ctx, a.ID, api.StepReportRequest{
		RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Running", StartedAt: started,
	})

	da := step.DownloadArtifact
	destDir := da.DestDir
	if destDir == "" {
		destDir = "."
	}
	destDir = resolveWorkspacePath(workDir, destDir)
	if err := a.Client.DownloadArtifact(ctx, runID, da.Name, destDir); err != nil {
		slog.Error("download-artifact failed", "step", step.Name, "error", err)
		_ = a.Client.ReportStep(ctx, a.ID, api.StepReportRequest{
			RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Failed",
			StartedAt: started, EndedAt: time.Now().UTC(),
		})
		return fmt.Errorf("download-artifact %q: %w", da.Name, err)
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
	postHooks *[]func(context.Context),
) error {
	started := time.Now().UTC()
	_ = a.Client.ReportStep(ctx, a.ID, api.StepReportRequest{
		RunID: runID, StepIndex: step.Index, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Running", StartedAt: started,
	})

	cs := step.Cache
	tplData := sctx.snapshot()

	key, err := dsl.ExpandTemplate(cs.Key, tplData)
	if err != nil {
		return fmt.Errorf("cache key template: %w", err)
	}
	var restoreKeys []string
	for _, rk := range cs.RestoreKeys {
		expanded, _ := dsl.ExpandTemplate(rk, tplData)
		if expanded != "" {
			restoreKeys = append(restoreKeys, expanded)
		}
	}

	if a.CacheStore != nil {
		hit, err := cache.Restore(ctx, a.CacheStore, cs.Path, key, restoreKeys)
		if err != nil && !errors.Is(err, cache.ErrCacheMiss) {
			slog.Warn("cache restore error", "step", step.Name, "error", err)
		} else if hit {
			slog.Info("cache hit", "step", step.Name, "key", key)
		} else {
			slog.Info("cache miss", "step", step.Name, "key", key)
		}
	}

	ttlDays := cs.TTLDays
	if ttlDays == 0 {
		ttlDays = 30
	}
	capturedPath := cs.Path
	capturedKey := key
	*postHooks = append(*postHooks, func(hookCtx context.Context) {
		if a.CacheStore == nil {
			return
		}
		if err := cache.Save(hookCtx, a.CacheStore, capturedPath, capturedKey, ttlDays); err != nil {
			slog.Warn("cache save failed", "key", capturedKey, "error", err)
		} else {
			slog.Info("cache saved", "key", capturedKey)
		}
	})

	_ = a.Client.ReportStep(ctx, a.ID, api.StepReportRequest{
		RunID: runID, StepIndex: step.Index, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Succeeded", StartedAt: started, EndedAt: time.Now().UTC(),
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
