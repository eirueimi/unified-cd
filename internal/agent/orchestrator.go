package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/secrets"
)

// CancelPollInterval is how often RunClaim's cancellation poller asks the
// controller whether the run was cancelled mid-flight. It is an exported var
// (not a const) so tests — in this package and in the k8s agent, which used
// to own its own unexported cancelPollInterval before this loop was shared —
// can shorten it instead of waiting through a real 5s tick.
var CancelPollInterval = 5 * time.Second

// RunClaim is the single shared step-orchestration loop driving both the host
// and k8s agents. It owns: secrets fetch -> masker construction -> installing
// the masker on b (SetMasker) -> the cancellation poller -> per-step context
// (timeouts, if:, approval, cache/artifact/call/run dispatch via b) ->
// RunPipeline for the main DAG (concurrency mode decided by b) -> post-hook
// LIFO drain -> deferred cache-save drain -> `finally` -> job-output
// promotion -> FinishRun.
//
// b is the ONLY seam between this orchestration logic and a concrete
// execution environment (host process vs k8s pod); the loop itself never
// branches on which backend it is driving. This is what makes drift between
// the two agents' orchestration logic structurally impossible: there is only
// one copy of this logic.
//
// client/agentID identify who reports progress; c is the claimed run. Callers
// (the host and k8s executeRun wrappers) are responsible for everything
// backend-specific that must happen BEFORE this call: acquiring the execution
// environment (workDir / pod), constructing b, and any host/k8s-only handling
// (e.g. the host agent's podTemplate handling — it warns that host-unsupported
// features are ignored and threads the podTemplate into hostBackend for
// runsIn.container resolution).
func RunClaim(ctx context.Context, client *Client, agentID string, c api.ClaimResponse, b ExecBackend) {
	slog.Info("running", "runId", c.RunID, "job", c.JobName)

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
		ticker := time.NewTicker(CancelPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				run, err := client.GetRun(runCtx, c.RunID)
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
		secretValues, err := client.FetchSecrets(ctx, agentID, c.SecretsNeeded)
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

	// The masker is born here (after secrets are fetched), so it is installed
	// via SetMasker rather than passed to the backend's constructor.
	b.SetMasker(masker)

	// warnSkippedOutput surfaces a dropped output both to the agent log and
	// into the run's own logs (stepIndex -1 renders as "System" in the UI).
	warnSkippedOutput := func(ctx context.Context, stepIndex int, key string) {
		slog.Warn("output skipped: value may contain a secret",
			"runId", c.RunID, "stepIndex", stepIndex, "key", key)
		_ = client.AppendLogBulk(ctx, agentID, c.RunID, stepIndex, []api.LogAppendRequest{{
			RunID:     c.RunID,
			StepIndex: stepIndex,
			Stream:    "stderr",
			Timestamp: time.Now().UTC(),
			Line:      fmt.Sprintf("output %q skipped: value may contain a secret", key),
		}})
	}

	// deferred hooks: run after RunPipeline completes (cache save, etc.)
	//
	// parallel: steps in the same claim run concurrently as goroutines under
	// Concurrent mode (see runParallel in pipeline.go), and both postHooks
	// (cache save, appended from executeCacheStep) and hookStack (post: hooks,
	// appended below in makeStepRunner) are appended from inside that
	// concurrently-invoked step runner. postHooksMu guards every append to
	// either slice so concurrent parallel-group members with a post:/cache:
	// don't race on the shared backing array. Under Sequential mode (k8s) the
	// lock is uncontended but still correct. The drain loops below run after
	// RunPipeline returns (i.e. after all step-runner invocations have
	// finished), so the drain itself does not need the lock.
	var postHooksMu sync.Mutex
	var postHooks []func(context.Context)
	var hookStack []postHookEntry

	// scopes: one scope-tracking structure for the whole claim, created lazily
	// on first use by a uses-scope step (owned by b). Torn down at claim end
	// regardless of how the claim finished (success, failure, or
	// cancellation).
	defer b.CloseScopes(context.WithoutCancel(ctx))

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
				_ = client.ReportStep(reportCtx, agentID, api.StepReportRequest{
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
					return client.ReportStep(callCtx, agentID, api.StepReportRequest{
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
				approved := WaitForApproval(stepCtx, client, agentID, c.RunID, step, ApprovalPollInterval)
				if approved {
					_ = client.ReportStep(stepCtx, agentID, api.StepReportRequest{
						RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex,
						StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Succeeded", EndedAt: time.Now().UTC(),
					})
				} else {
					_ = client.ReportStep(stepCtx, agentID, api.StepReportRequest{
						RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex,
						StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Failed", EndedAt: time.Now().UTC(),
					})
					recordFailure()
				}
				return nil
			}

			// cache steps: restore immediately, defer save to postHooks
			if step.Cache != nil {
				scope, serr := resolveScope(stepCtx, step, b)
				if serr != nil {
					// Cache stays warn+skip (lenient policy), matching the
					// k8s agent: a scope pod/container that never becomes
					// available must not fail the step, just skip the cache
					// operation (no restore, no deferred save). Unlike
					// artifact upload/download, which remain fail-loud.
					slog.Warn("cache scope unavailable; skipping cache for step", "step", step.Name, "error", serr)
					_ = client.ReportStep(context.WithoutCancel(stepCtx), agentID, api.StepReportRequest{
						RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex,
						StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Succeeded", EndedAt: time.Now().UTC(),
					})
					return nil
				}
				if err := executeCacheStep(stepCtx, client, agentID, step, c.RunID, sctx, &postHooksMu, &postHooks, b, scope); err != nil {
					slog.Error("cache step failed", "step", step.Name, "error", err)
					markFailed(context.WithoutCancel(stepCtx))
				}
				return nil
			}
			if step.UploadArtifact != nil {
				scope, serr := resolveScope(stepCtx, step, b)
				if serr != nil {
					slog.Error("upload artifact failed", "step", step.Name, "error", serr)
					markFailed(context.WithoutCancel(stepCtx))
					return nil
				}
				if err := executeUploadArtifact(stepCtx, client, agentID, step, c.RunID, b, scope); err != nil {
					slog.Error("upload artifact failed", "step", step.Name, "error", err)
					markFailed(context.WithoutCancel(stepCtx))
				}
				return nil
			}
			if step.DownloadArtifact != nil {
				scope, serr := resolveScope(stepCtx, step, b)
				if serr != nil {
					slog.Error("download artifact failed", "step", step.Name, "error", serr)
					markFailed(context.WithoutCancel(stepCtx))
					return nil
				}
				if err := executeDownloadArtifact(stepCtx, client, agentID, step, c.RunID, b, scope); err != nil {
					slog.Error("download artifact failed", "step", step.Name, "error", err)
					markFailed(context.WithoutCancel(stepCtx))
				}
				return nil
			}

			started := time.Now().UTC()
			_ = client.ReportStep(stepCtx, agentID, api.StepReportRequest{
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
			// stepScope captures this step's scope handle (if any), set inside
			// the isScopedStep run-branch case below. A step's post: hook must
			// execute inside the same scope container the step body ran in
			// (not the host workspace), so postHookEntry carries this through
			// to the hookStack drain in the finally block.
			var stepScope ScopeHandle

			if step.Call != nil {
				childOutputs, childRunID, callErr := ExecuteCallStep(stepCtx, client, agentID, c.RunID, step, tplData)
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
						safe := FilterSecretOutputs(childOutputs, masker, func(k string) {
							warnSkippedOutput(stepCtx, step.Index, k)
						})
						if len(safe) > 0 {
							_ = client.SetStepOutputs(stepCtx, agentID, c.RunID, step.Index, step.MatrixKey, safe)
						}
					}
				}
			} else {
				expandedRun, tplErr := dsl.ExpandTemplate(step.Run, tplData)
				if tplErr != nil {
					slog.Error("template expansion failed", "step", step.Name, "error", tplErr)
					expandedRun = step.Run
				}

				// UNIFIED_AGENT_OS lets job authors determine the running OS from within a step.
				// Scoped / runsIn.image steps run in a Linux container regardless of
				// backend; every other step reports b.DefaultAgentOS() — see agentOSForStep.
				extraEnv := []string{"UNIFIED_AGENT_OS=" + agentOSForStep(step, b.DefaultAgentOS())}
				for k, v := range step.Env {
					expanded, _ := dsl.ExpandTemplate(v, tplData)
					extraEnv = append(extraEnv, k+"="+expanded)
				}

				shippedStdout, shippedStderr, finishLogs := b.StepLogWriters(stepCtx, step.Index)
				// stdout is teed: streamed to the server while the step runs
				// (mirroring the k8s agent's io.MultiWriter approach) AND kept in
				// stdoutBuf for {{ .Stdout }} output-template evaluation below.
				var stdoutBuf bytes.Buffer
				stdoutTee := io.MultiWriter(&stdoutBuf, shippedStdout)
				var ec int
				var runErr error
				switch {
				case isScopedStep(step):
					// Scoped steps never carry a container: exec target (mutually
					// exclusive at the DSL level), so this case takes precedence over
					// the container case below.
					h, herr := b.EnsureScope(stepCtx, step, extraEnv)
					if herr != nil {
						runErr = herr
						ec = -1
						break
					}
					stepScope = h
					ec, runErr = b.RunInScope(stepCtx, h, expandedRun, extraEnv, stdoutTee, shippedStderr)
				case step.Container != "":
					ec, runErr = b.RunNamedContainer(stepCtx, step, step.Container, expandedRun, extraEnv, stdoutTee, shippedStderr)
				default:
					ec, runErr = b.RunDefault(stepCtx, step, expandedRun, extraEnv, stdoutTee, shippedStderr)
				}
				capturedStdout := stdoutBuf.String()
				exitCode = ec
				finishLogs(stepCtx)

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
						safe := FilterSecretOutputs(capturedOutputs, masker, func(k string) {
							warnSkippedOutput(stepCtx, step.Index, k)
						})
						if len(safe) > 0 {
							_ = client.SetStepOutputs(stepCtx, agentID, c.RunID, step.Index, step.MatrixKey, safe)
						}
					}
				}
			}

			if status == "Succeeded" && step.Post != nil {
				container := step.Container
				postHooksMu.Lock()
				hookStack = append(hookStack, postHookEntry{
					stepName:  step.Name,
					post:      *step.Post,
					scope:     stepScope,
					container: container,
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
				return client.ReportStep(callCtx, agentID, reportReq)
			})
			if status == "Failed" {
				recordFailure()
				return nil
			}
			return nil
		}
	}

	mainRunner := makeStepRunner(statusView, true, &anyStepFailed, true)
	dagErr := RunPipeline(runCtx, c.Stages, getData, c.MatrixMaxCombinations, b.ConcurrencyMode(), mainRunner)

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
		// The owning step's scope (if any) is still alive here — hookStack is
		// drained before the deferred b.CloseScopes runs (see the `defer`
		// registered alongside masker installation above).
		if runErr := b.RunPostHook(hookCtx, entry.scope, entry.container, cmd, extraEnv); runErr != nil {
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
		if err := RunPipeline(finallyCtx, c.Finally, getData, c.MatrixMaxCombinations, b.ConcurrencyMode(), finallyRunner); err != nil {
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
		safeRunOutputs := FilterSecretOutputs(runOutputs, masker, func(k string) {
			warnSkippedOutput(finishCtx, -1, k)
		})
		if len(safeRunOutputs) > 0 {
			// Retried until success (unlike the pre-refactor host single-shot
			// call): a transient failure here must not silently drop job outputs,
			// matching the pre-refactor k8s agent's RetryUntilSuccess wrapping.
			// Deliberate reconciliation pick — see Task 8 report.
			retryUntilSuccess(finishCtx, func(callCtx context.Context) error {
				return client.SetRunOutputs(callCtx, agentID, c.RunID, safeRunOutputs)
			})
		}
	}

	retryUntilSuccess(finishCtx, func(callCtx context.Context) error {
		return client.FinishRun(callCtx, agentID, c.RunID, overallStatus)
	})
}

// executeUploadArtifact runs an upload-artifact step: b.ResolveArtifactPath
// resolves ua.Path against the right root for scope (host workDir / k8s pod
// mount path when non-scoped, the scope container's fixed working directory
// when scoped — see ExecBackend.ResolveArtifactPath), and b.UploadArtifact
// routes the actual transfer (host file vs scope-container copyOutToTemp vs
// k8s sidecar exec). Fail-loud: artifact operations do not silently skip on
// error.
func executeUploadArtifact(ctx context.Context, client *Client, agentID string, step api.ClaimStep, runID string, b ExecBackend, scope ScopeHandle) error {
	started := time.Now().UTC()
	_ = client.ReportStep(ctx, agentID, api.StepReportRequest{
		RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Running", StartedAt: started,
	})

	ua := step.UploadArtifact
	artifactPath := b.ResolveArtifactPath(scope, ua.Path)
	if err := b.UploadArtifact(ctx, scope, runID, ua.Name, artifactPath); err != nil {
		slog.Error("upload-artifact failed", "step", step.Name, "error", err)
		_ = client.ReportStep(ctx, agentID, api.StepReportRequest{
			RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Failed",
			StartedAt: started, EndedAt: time.Now().UTC(),
		})
		return fmt.Errorf("upload-artifact %q: %w", ua.Name, err)
	}
	_ = client.ReportStep(ctx, agentID, api.StepReportRequest{
		RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Succeeded",
		StartedAt: started, EndedAt: time.Now().UTC(),
	})
	return nil
}

// executeDownloadArtifact runs a download-artifact step, mirroring
// executeUploadArtifact's path resolution (see ExecBackend.ResolveArtifactPath).
func executeDownloadArtifact(ctx context.Context, client *Client, agentID string, step api.ClaimStep, runID string, b ExecBackend, scope ScopeHandle) error {
	started := time.Now().UTC()
	_ = client.ReportStep(ctx, agentID, api.StepReportRequest{
		RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Running", StartedAt: started,
	})

	da := step.DownloadArtifact
	destDir := da.DestDir
	if destDir == "" {
		destDir = "."
	}
	resolvedDestDir := b.ResolveArtifactPath(scope, destDir)

	if err := b.DownloadArtifact(ctx, scope, runID, da.Name, resolvedDestDir); err != nil {
		slog.Error("download-artifact failed", "step", step.Name, "error", err)
		_ = client.ReportStep(ctx, agentID, api.StepReportRequest{
			RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Failed",
			StartedAt: started, EndedAt: time.Now().UTC(),
		})
		return fmt.Errorf("download-artifact %q: %w", da.Name, err)
	}

	_ = client.ReportStep(ctx, agentID, api.StepReportRequest{
		RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Succeeded",
		StartedAt: started, EndedAt: time.Now().UTC(),
	})
	return nil
}

// executeCacheStep runs a cache step: restore immediately (best-effort,
// lenient — a miss/error never fails the step), deferring the matching save
// into postHooks so it captures the final workspace state at claim end.
// cachePath resolution mirrors executeUploadArtifact (see
// ExecBackend.ResolveArtifactPath) for the scoped case; the non-scoped path
// is left exactly as authored (unresolved), matching the pre-refactor host
// agent's cache.Restore/cache.Save calls, which treat a relative path as
// relative to the process's own CWD rather than the claim's workDir.
func executeCacheStep(
	ctx context.Context,
	client *Client,
	agentID string,
	step api.ClaimStep,
	runID string,
	sctx *safeStepCtx,
	postHooksMu *sync.Mutex,
	postHooks *[]func(context.Context),
	b ExecBackend,
	scope ScopeHandle,
) error {
	started := time.Now().UTC()
	_ = client.ReportStep(ctx, agentID, api.StepReportRequest{
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
	// A key/path template that expands SUCCESSFULLY to an empty string is
	// warn+skip (cache operation skipped, step still Succeeded), matching the
	// k8s agent's empty-key/empty-path branches.
	// A valid-but-empty key must not silently collide caches across runs, and
	// a valid-but-empty path would target the wrong directory (workspace
	// root, mount root, etc.) — either way the safe behavior is to skip, not
	// hard-fail: only a template EXPANSION ERROR (above) is a hard failure.
	if key == "" {
		slog.Warn("cache key expanded to empty; skipping cache for step", "step", step.Name, "keyTemplate", cs.Key)
		_ = client.ReportStep(ctx, agentID, api.StepReportRequest{
			RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Succeeded", StartedAt: started, EndedAt: time.Now().UTC(),
		})
		return nil
	}
	if cachePath == "" {
		slog.Warn("cache path expanded to empty; skipping cache for step", "step", step.Name, "pathTemplate", cs.Path)
		_ = client.ReportStep(ctx, agentID, api.StepReportRequest{
			RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Succeeded", StartedAt: started, EndedAt: time.Now().UTC(),
		})
		return nil
	}
	// b.ResolveCachePath resolves cachePath for scope: on the host, a scoped
	// path is resolved against the scope container's working directory (so
	// it is always absolute before copyIn/copyOut) while a non-scoped path is
	// left unresolved (as authored); on k8s, both cases resolve against the
	// pod's mount path (mirrors the pre-refactor path.Join(mount,
	// expandedPath)) — see ExecBackend.ResolveCachePath's doc comment.
	scopedCachePath := b.ResolveCachePath(scope, cachePath)
	var restoreKeys []string
	for _, rk := range cs.RestoreKeys {
		expanded, _ := dsl.ExpandTemplate(rk, tplData)
		if expanded != "" {
			restoreKeys = append(restoreKeys, expanded)
		}
	}

	// Cache stays warn+skip on error (lenient policy): a restore/save problem
	// should not fail the step, unlike artifact upload/download.
	if hit, err := b.CacheRestore(ctx, scope, key, restoreKeys, scopedCachePath); err != nil {
		slog.Warn("cache restore error", "step", step.Name, "error", err)
	} else if hit {
		slog.Info("cache hit", "step", step.Name, "key", key)
	} else {
		slog.Info("cache miss", "step", step.Name, "key", key)
	}

	ttlDays := cs.TTLDays
	if ttlDays == 0 {
		ttlDays = 30
	}
	capturedPath := scopedCachePath
	capturedKey := key
	postHooksMu.Lock()
	*postHooks = append(*postHooks, func(hookCtx context.Context) {
		// NOTE: on the host backend with a nil CacheStore (cache disabled),
		// b.CacheSave is a silent no-op that returns nil, so this still logs
		// "cache saved" even though nothing was saved (hostBackend.CacheSave
		// logs its own DEBUG-level "cache disabled; save skipped" instead).
		// Fixing this precisely would require an ExecBackend interface change
		// (e.g. a bool "did it actually save" return) or a type assertion
		// across the host/k8s seam — too big a change for what is an
		// imprecise log line with no functional impact, so it is left as-is.
		if err := b.CacheSave(hookCtx, scope, capturedKey, capturedPath, ttlDays); err != nil {
			slog.Warn("cache save failed", "key", capturedKey, "error", err)
		} else {
			slog.Info("cache saved", "key", capturedKey)
		}
	})
	postHooksMu.Unlock()

	_ = client.ReportStep(ctx, agentID, api.StepReportRequest{
		RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Succeeded", StartedAt: started, EndedAt: time.Now().UTC(),
	})
	return nil
}
