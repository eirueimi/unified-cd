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
	"sync/atomic"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

// approvalPollInterval is how often WaitForApproval polls the controller for a
// human decision. It is a var (not a const) so tests can shorten it.
var approvalPollInterval = 3 * time.Second

// cancelPollInterval is how often the cancel poller polls the controller to
// detect mid-run cancellation. It is a var (not a const) so tests can shorten it.
var cancelPollInterval = 5 * time.Second

// podStepExec runs a single already-expanded step inside the pod and returns
// the exit code, captured stdout, and any infrastructure error.
type podStepExec func(ctx context.Context, step api.ClaimStep, expandedRun string) (exitCode int, stdout string, err error)

// K8sAgent is an agent that claims Runs from the master and executes them inside a Kubernetes Pod.
type K8sAgent struct {
	cfg    Config
	client *agentlib.Client
	pm     *PodManager
	exec   *Executor
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

	usePool := c.PodTemplate != nil && c.PodTemplate.Reuse

	var pooledPod *PooledPod
	var podName string

	if usePool {
		templateName := ""
		if c.PodTemplate != nil {
			templateName = c.PodTemplate.Name
		}
		pp, err := a.pool.ClaimPod(ctx, c.RunID, templateName, a.cfg.PodTemplates, c.PodTemplate, a.cfg.PodImage,
			SidecarSpec{Image: a.cfg.SidecarImage, Server: a.cfg.Server, Token: a.cfg.Token})
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
			SidecarSpec{Image: a.cfg.SidecarImage, Server: a.cfg.Server, Token: a.cfg.Token})
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
				firstContainer = steps[0].Container
				break
			}
		}
		_, _ = a.exec.ExecStep(ctx, podName, firstContainer, fmt.Sprintf("rm -rf %s/*", mountPath), io.Discard, io.Discard)
	}

	stepExec := func(execCtx context.Context, step api.ClaimStep, expandedRun string) (int, string, error) {
		var stdoutBuf strings.Builder
		stderrPusher := agentlib.NewLogPusher(a.client, a.cfg.AgentID, c.RunID, step.Index, "stderr")
		stdoutWriter := io.MultiWriter(&stdoutBuf, &logLineWriter{
			client: a.client, agentID: a.cfg.AgentID, runID: c.RunID, stepIdx: step.Index, stream: "stdout",
		})
		ec, execErr := a.exec.ExecStep(execCtx, podName, step.Container, expandedRun, stdoutWriter, stderrPusher)
		stderrPusher.Flush(execCtx)
		return ec, stdoutBuf.String(), execErr
	}

	mountPath := "/workspace"
	if c.PodTemplate != nil && c.PodTemplate.Workspace != nil && c.PodTemplate.Workspace.MountPath != "" {
		mountPath = c.PodTemplate.Workspace.MountPath
	}

	artifactExec := func(execCtx context.Context, container, script string) (int, error) {
		stderrPusher := agentlib.NewLogPusher(a.client, a.cfg.AgentID, c.RunID, 0, "stderr")
		ec, err := a.exec.ExecStep(execCtx, podName, container, script, io.Discard, stderrPusher)
		stderrPusher.Flush(execCtx)
		return ec, err
	}

	a.orchestrate(ctx, c, stepExec, artifactExec, mountPath)
}

// orchestrate runs the claim's stages, reporting step/run status, using stepExec
// to run each step's command. Pure of pod lifecycle so it is unit-testable.
// artifactExec dispatches uploadArtifact/downloadArtifact commands into the sidecar.
// mountPath is the workspace volume's mount path inside the pod (default "/workspace").
func (a *K8sAgent) orchestrate(ctx context.Context, c api.ClaimResponse, stepExec podStepExec, artifactExec func(ctx context.Context, container, script string) (int, error), mountPath string) {
	var anyStepFailed atomic.Bool
	var cancelledByMaster atomic.Bool
	statusView := func() dsl.RunStatusView {
		cancelled := cancelledByMaster.Load()
		return dsl.RunStatusView{Failed: anyStepFailed.Load() && !cancelled, Cancelled: cancelled}
	}

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
					cancelledByMaster.Store(true)
					cancelRun()
					return
				}
			}
		}
	}()

	// Accumulate step outputs for template expansion in subsequent steps
	stepCtx := dsl.TemplateData{Params: c.Params, Steps: map[string]dsl.StepData{}}

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
			// Build template data; expose foreach variable if set
			tplData := dsl.TemplateData{Params: stepCtx.Params, Steps: stepCtx.Steps}
			if step.ForeachKey != "" {
				tplData.Foreach = map[string]string{step.ForeachKey: step.ForeachValue}
			}

			// Every step is gated by if:. On eval error the step runs (fail-safe);
			// on false it is reported Skipped and not run.
			ok, err := dsl.EvalCondition(step.If, tplData, statusFn(), implicitSuccess)
			if err != nil {
				slog.Warn("k8s: if condition eval failed, running step", "step", step.Name, "error", err)
			}
			if !ok {
				_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
					RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.Name, Status: "Skipped",
				})
				return
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
				_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
					RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.Name, Status: status, EndedAt: time.Now().UTC(),
				})
				if !approved {
					recordFailure(step)
				}
				return
			}

			// artifact upload: exec a tar|zstd|curl PUT into the sidecar
			if step.UploadArtifact != nil {
				started := time.Now().UTC()
				_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
					RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.Name, Status: "Running", StartedAt: started,
				})
				src := path.Join(mountPath, step.UploadArtifact.Path)
				url := fmt.Sprintf("$UNIFIED_SERVER/api/v1/runs/%s/artifacts/%s", c.RunID, step.UploadArtifact.Name)
				script := fmt.Sprintf(
					`set -e; tar cf - -C %q . | zstd -q | curl -fsS -X PUT -H "Authorization: Bearer $UNIFIED_AGENT_TOKEN" --data-binary @- %q`,
					src, url)
				ec, err := artifactExec(execCtx, artifactSidecarName, script)
				status := "Succeeded"
				if err != nil || ec != 0 {
					status = "Failed"
				}
				_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
					RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.Name, Status: status, ExitCode: ec, StartedAt: started, EndedAt: time.Now().UTC(),
				})
				if status == "Failed" && !step.ContinueOnError {
					failedFlag.Store(true)
				}
				return
			}

			// artifact download: exec a curl|zstd|tar extract into the sidecar
			if step.DownloadArtifact != nil {
				started := time.Now().UTC()
				_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
					RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.Name, Status: "Running", StartedAt: started,
				})
				dest := step.DownloadArtifact.DestDir
				if dest == "" {
					dest = "."
				}
				destAbs := path.Join(mountPath, dest)
				url := fmt.Sprintf("$UNIFIED_SERVER/api/v1/runs/%s/artifacts/%s", c.RunID, step.DownloadArtifact.Name)
				script := fmt.Sprintf(
					`set -e; mkdir -p %q; curl -fsS -H "Authorization: Bearer $UNIFIED_AGENT_TOKEN" %q | zstd -dq | tar xf - -C %q`,
					destAbs, url, destAbs)
				ec, err := artifactExec(execCtx, artifactSidecarName, script)
				status := "Succeeded"
				if err != nil || ec != 0 {
					status = "Failed"
				}
				_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
					RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.Name, Status: status, ExitCode: ec, StartedAt: started, EndedAt: time.Now().UTC(),
				})
				if status == "Failed" && !step.ContinueOnError {
					failedFlag.Store(true)
				}
				return
			}

			started := time.Now().UTC()
			_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
				RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.Name, Status: "Running", StartedAt: started,
			})

			// Attempt template expansion; fall back to the original script on failure
			expandedRun, _ := dsl.ExpandTemplate(step.Run, tplData)
			if expandedRun == "" {
				expandedRun = step.Run
			}

			ec, capturedStdout, execErr := stepExec(execCtx, step, expandedRun)

			status := "Succeeded"
			if execErr != nil || ec != 0 {
				status = "Failed"
			} else {
				// Evaluate output templates against the captured stdout
				capturedOutputs := map[string]string{}
				outCtx := dsl.TemplateData{Params: stepCtx.Params, Steps: stepCtx.Steps, Stdout: capturedStdout}
				for outKey, outTpl := range step.Outputs {
					if val, err := dsl.ExpandTemplate(outTpl, outCtx); err == nil {
						capturedOutputs[outKey] = val
					}
				}
				if len(capturedOutputs) > 0 {
					stepCtx.Steps[step.Name] = dsl.StepData{Outputs: capturedOutputs}
					_ = a.client.SetStepOutputs(ctx, a.cfg.AgentID, c.RunID, step.Index, capturedOutputs)
				}
			}

			ended := time.Now().UTC()
			_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
				RunID:      c.RunID,
				StepIndex:  step.Index,
				StageIndex: step.StageIndex,
				StepName:   step.Name,
				Status:     status,
				ExitCode:   ec,
				StartedAt:  started,
				EndedAt:    ended,
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

	// Visit every stage/step; the if: auto-skip (implicit success()) handles
	// post-failure behavior, so the loop never aborts on failure.
	for _, stage := range c.Stages {
		for _, step := range api.StageSteps(stage) {
			if step.Foreach != nil {
				// Expand foreach and run each variant sequentially inside the pod
				data := dsl.TemplateData{Params: c.Params, Steps: stepCtx.Steps}
				items, err := agentlib.EvalForeachSource(step.Foreach.Source, data)
				if err != nil {
					slog.Error("k8s: foreach expansion failed", "step", step.Name, "error", err)
					anyStepFailed.Store(true)
					continue
				}
				for _, item := range items {
					variant := step
					variant.Foreach = nil
					variant.ForeachKey = step.Foreach.Key
					variant.ForeachValue = item
					mainRun(variant)
				}
			} else {
				mainRun(step)
			}
		}
	}

	// Promote declared job outputs
	runOutputs := map[string]string{}
	for _, stage := range c.Stages {
		for _, step := range api.StageSteps(stage) {
			if sd, ok := stepCtx.Steps[step.Name]; ok {
				for _, outName := range c.JobOutputs {
					if val, ok := sd.Outputs[outName]; ok {
						runOutputs[outName] = val
					}
				}
			}
		}
	}
	if len(runOutputs) > 0 {
		_ = a.client.SetRunOutputs(ctx, a.cfg.AgentID, c.RunID, runOutputs)
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
		for _, stage := range c.Finally {
			for _, step := range api.StageSteps(stage) {
				if step.Foreach != nil {
					data := dsl.TemplateData{Params: c.Params, Steps: stepCtx.Steps}
					items, err := agentlib.EvalForeachSource(step.Foreach.Source, data)
					if err != nil {
						slog.Error("k8s: finally foreach expansion failed", "step", step.Name, "error", err)
						finallyFailed.Store(true)
						continue
					}
					for _, item := range items {
						variant := step
						variant.Foreach = nil
						variant.ForeachKey = step.Foreach.Key
						variant.ForeachValue = item
						finallyRun(variant)
					}
				} else {
					finallyRun(step)
				}
			}
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
	// the run was cancelled.
	_ = a.client.FinishRun(context.WithoutCancel(ctx), a.cfg.AgentID, c.RunID, overallStatus)
}

// logLineWriter is a Writer that sends each line of stdout to the master server via AppendLog.
type logLineWriter struct {
	client  *agentlib.Client
	agentID string
	runID   string
	stepIdx int
	stream  string
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

func appendLabelIfMissing(labels []string, label string) []string {
	for _, l := range labels {
		if l == label {
			return labels
		}
	}
	return append(labels, label)
}
