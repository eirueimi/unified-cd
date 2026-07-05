package k8sagent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	corev1 "k8s.io/api/core/v1"
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
	ExecStep(ctx context.Context, podName, container, script string, stdout, stderr io.Writer) (int, error)
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
		_, _ = a.exec.ExecStep(ctx, podName, firstContainer, fmt.Sprintf("rm -rf %s/*", mountPath), io.Discard, io.Discard)
	}

	stepExec := func(execCtx context.Context, step api.ClaimStep, expandedRun string) (int, string, error) {
		var stdoutBuf strings.Builder
		stderrPusher := agentlib.NewLogPusher(a.client, a.cfg.AgentID, c.RunID, step.Index, "stderr")
		stdoutWriter := io.MultiWriter(&stdoutBuf, &logLineWriter{
			client: a.client, agentID: a.cfg.AgentID, runID: c.RunID, stepIdx: step.Index, stream: "stdout",
		})

		var ec int
		var execErr error
		if step.RunsIn != nil && step.RunsIn.Image != "" {
			// Isolated throwaway pod. UNIFIED_AGENT_OS mirrors the host agent's
			// convention; step.Env arrives already template-expanded (orchestrate).
			env := step.Env
			if env == nil {
				env = map[string]string{}
			}
			env["UNIFIED_AGENT_OS"] = runtime.GOOS
			deadline := int64(3600)
			if step.TimeoutMinutes > 0 {
				deadline = int64(step.TimeoutMinutes * 60)
			}
			ec, execErr = a.runImageStep(execCtx, c.RunID, step.RunsIn.Image, env, deadline, expandedRun, stdoutWriter, stderrPusher)
		} else {
			ec, execErr = a.exec.ExecStep(execCtx, podName, execContainer(step), expandedRun, stdoutWriter, stderrPusher)
		}

		stderrPusher.Flush(execCtx)
		return ec, stdoutBuf.String(), execErr
	}

	mountPath := "/workspace"
	if c.PodTemplate != nil && c.PodTemplate.Workspace != nil && c.PodTemplate.Workspace.MountPath != "" {
		mountPath = c.PodTemplate.Workspace.MountPath
	}

	sidecarExec := func(execCtx context.Context, container string, argv []string) (int, error) {
		stderrPusher := agentlib.NewLogPusher(a.client, a.cfg.AgentID, c.RunID, 0, "stderr")
		ec, err := a.exec.ExecStepArgv(execCtx, podName, container, argv, io.Discard, stderrPusher)
		stderrPusher.Flush(execCtx)
		return ec, err
	}

	a.orchestrate(ctx, c, stepExec, sidecarExec, mountPath)
}

// orchestrate runs the claim's stages, reporting step/run status, using stepExec
// to run each step's command. Pure of pod lifecycle so it is unit-testable.
// sidecarExec dispatches cache/artifact commands (argv, no shell) into the
// unified-artifact sidecar container.
// mountPath is the workspace volume's mount path inside the pod (default "/workspace").
func (a *K8sAgent) orchestrate(ctx context.Context, c api.ClaimResponse, stepExec podStepExec, sidecarExec func(ctx context.Context, container string, argv []string) (int, error), mountPath string) {
	var anyStepFailed atomic.Bool
	var cancelledByMaster atomic.Bool
	statusView := func() dsl.RunStatusView {
		cancelled := cancelledByMaster.Load()
		return dsl.RunStatusView{Failed: anyStepFailed.Load() && !cancelled, Cancelled: cancelled}
	}

	// cacheSaveSpec captures a cache step's save parameters, deferred until
	// after the main stages complete (so the save captures the final
	// workspace state, matching the standard agent's cache semantics).
	type cacheSaveSpec struct {
		key     string
		ttlDays int
		path    string
	}
	var cacheSavesMu sync.Mutex
	var cacheSaves []cacheSaveSpec
	registerCacheSave := func(s cacheSaveSpec) {
		cacheSavesMu.Lock()
		cacheSaves = append(cacheSaves, s)
		cacheSavesMu.Unlock()
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
			// Build template data; expose matrix/foreach values if set
			tplData := dsl.TemplateData{Params: stepCtx.Params, Steps: stepCtx.Steps}
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
				_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
					RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Skipped",
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
				_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
					RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Running", StartedAt: started,
				})
				key, kerr := dsl.ExpandTemplate(step.Cache.Key, tplData)
				expandedPath, perr := dsl.ExpandTemplate(step.Cache.Path, tplData)
				if kerr != nil || key == "" {
					// A bad/empty key template must not silently collide caches
					// across runs. Skip the cache operation entirely (no restore,
					// no deferred save) but keep cache best-effort: the step still
					// succeeds.
					slog.Warn("k8s: cache key template failed; skipping cache for step", "step", step.Name, "keyTemplate", step.Cache.Key, "error", kerr)
				} else if perr != nil || expandedPath == "" {
					// A bad/empty path template would make the cache target the
					// workspace mount root (or a wrong directory). Skip the cache
					// operation entirely, same as a bad key.
					slog.Warn("k8s: cache path template failed; skipping cache for step", "step", step.Name, "pathTemplate", step.Cache.Path, "error", perr)
				} else {
					var restoreKeys []string
					for _, rk := range step.Cache.RestoreKeys {
						if v, _ := dsl.ExpandTemplate(rk, tplData); v != "" {
							restoreKeys = append(restoreKeys, v)
						}
					}
					cachePath := path.Join(mountPath, expandedPath)
					argv := []string{"unified-sidecar", "cache", "restore", "--key", key, "--path", cachePath}
					for _, rk := range restoreKeys {
						argv = append(argv, "--restore-key", rk)
					}
					// Best-effort: a miss/error never fails the step (the binary exits 0).
					_, _ = sidecarExec(execCtx, artifactSidecarName, argv)

					ttlDays := step.Cache.TTLDays
					if ttlDays == 0 {
						ttlDays = 30
					}
					registerCacheSave(cacheSaveSpec{key: key, ttlDays: ttlDays, path: cachePath})
				}

				_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
					RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Succeeded", StartedAt: started, EndedAt: time.Now().UTC(),
				})
				return
			}

			// artifact upload: exec the unified-sidecar binary via argv into the
			// sidecar. Artifacts are fail-loud (not best-effort).
			if step.UploadArtifact != nil {
				started := time.Now().UTC()
				_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
					RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Running", StartedAt: started,
				})
				argv := []string{"unified-sidecar", "artifact", "upload",
					"--run", c.RunID, "--name", step.UploadArtifact.Name,
					"--path", path.Join(mountPath, step.UploadArtifact.Path)}
				ec, err := sidecarExec(execCtx, artifactSidecarName, argv)
				status := "Succeeded"
				if err != nil || ec != 0 {
					status = "Failed"
				}
				_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
					RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: status, ExitCode: ec, StartedAt: started, EndedAt: time.Now().UTC(),
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
				_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
					RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Running", StartedAt: started,
				})
				dest := step.DownloadArtifact.DestDir
				if dest == "" {
					dest = "."
				}
				argv := []string{"unified-sidecar", "artifact", "download",
					"--run", c.RunID, "--name", step.DownloadArtifact.Name,
					"--dest", path.Join(mountPath, dest)}
				ec, err := sidecarExec(execCtx, artifactSidecarName, argv)
				status := "Succeeded"
				if err != nil || ec != 0 {
					status = "Failed"
				}
				_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
					RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: status, ExitCode: ec, StartedAt: started, EndedAt: time.Now().UTC(),
				})
				if status == "Failed" {
					recordFailure(step)
				}
				return
			}

			started := time.Now().UTC()
			_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
				RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Running", StartedAt: started,
			})

			// Attempt template expansion; fall back to the original script on failure
			expandedRun, _ := dsl.ExpandTemplate(step.Run, tplData)
			if expandedRun == "" {
				expandedRun = step.Run
			}

			stepForExec := step
			stepForExec.Env = expandStepEnv(step.Env, tplData)
			ec, capturedStdout, execErr := stepExec(execCtx, stepForExec, expandedRun)

			status := "Succeeded"
			if execErr != nil || ec != 0 {
				status = "Failed"
			} else {
				// Evaluate output templates against the captured stdout
				capturedOutputs := map[string]string{}
				outCtx := dsl.TemplateData{Params: stepCtx.Params, Steps: stepCtx.Steps, Stdout: capturedStdout, Matrix: tplData.Matrix, Foreach: tplData.Foreach}
				for outKey, outTpl := range step.Outputs {
					if val, err := dsl.ExpandTemplate(outTpl, outCtx); err == nil {
						capturedOutputs[outKey] = val
					}
				}
				if len(capturedOutputs) > 0 {
					if step.MatrixKey != "" {
						sd := stepCtx.Steps[step.Name]
						if sd.Outputs == nil {
							sd.Outputs = map[string]any{}
						}
						for k, v := range capturedOutputs {
							m, _ := sd.Outputs[k].(map[string]string)
							if m == nil {
								m = map[string]string{}
							}
							m[step.MatrixKey] = v
							sd.Outputs[k] = m
						}
						stepCtx.Steps[step.Name] = sd
					} else {
						stepCtx.Steps[step.Name] = dsl.StepData{Outputs: dsl.StringOutputs(capturedOutputs)}
					}
					_ = a.client.SetStepOutputs(ctx, a.cfg.AgentID, c.RunID, step.Index, step.MatrixKey, capturedOutputs)
				}
			}

			ended := time.Now().UTC()
			_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
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
			data := dsl.TemplateData{Params: c.Params, Steps: stepCtx.Steps}
			variants, err := agentlib.ExpandMatrixStep(step, data, c.MatrixMaxCombinations)
			if err != nil {
				slog.Error("k8s: matrix expansion failed", "step", step.Name, "error", err)
				anyStepFailed.Store(true)
				continue
			}
			for _, v := range variants {
				mainRun(v)
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
						runOutputs[outName] = dsl.OutputValueString(val)
					}
				}
			}
		}
	}
	if len(runOutputs) > 0 {
		_ = a.client.SetRunOutputs(ctx, a.cfg.AgentID, c.RunID, runOutputs)
	}

	// Deferred cache saves: capture the final workspace after the main stages
	// (before finally, which is cleanup/notify). Best-effort — never flips status.
	// Use a non-cancelling context so a parent-context cancellation (process
	// shutdown) doesn't abort the save, matching the standard agent and the
	// k8s finally block below.
	saveCtx := context.WithoutCancel(ctx)
	for _, s := range cacheSaves {
		argv := []string{"unified-sidecar", "cache", "save", "--key", s.key, "--ttl-days", strconv.Itoa(s.ttlDays), "--path", s.path}
		if _, err := sidecarExec(saveCtx, artifactSidecarName, argv); err != nil {
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
		for _, stage := range c.Finally {
			for _, step := range api.StageSteps(stage) {
				data := dsl.TemplateData{Params: c.Params, Steps: stepCtx.Steps}
				variants, err := agentlib.ExpandMatrixStep(step, data, c.MatrixMaxCombinations)
				if err != nil {
					slog.Error("k8s: finally matrix expansion failed", "step", step.Name, "error", err)
					finallyFailed.Store(true)
					continue
				}
				for _, v := range variants {
					finallyRun(v)
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

// runImageStep runs a runsIn.image step in a throwaway, isolated pod: create a
// pod from the image (kept alive with sleep infinity), wait until it is
// running, exec the step's script into the single "step" container, then delete
// the pod. The pod is always deleted (defer, non-cancellable context) so a
// cancelled or failed step never leaks a pod. A failure to create/start the pod
// is a hard error surfaced to the step — never a silent fallback.
func (a *K8sAgent) runImageStep(ctx context.Context, runID, image string, env map[string]string, deadlineSeconds int64, script string, stdout, stderr io.Writer) (int, error) {
	pod := buildImageStepPod(runID, a.cfg.Namespace, image, env, deadlineSeconds)
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

	if err := a.pm.WaitForPodRunning(ctx, name); err != nil {
		return -1, fmt.Errorf("runsIn.image %q: pod did not start: %w", image, err)
	}
	return a.exec.ExecStep(ctx, name, "step", script, stdout, stderr)
}

func appendLabelIfMissing(labels []string, label string) []string {
	for _, l := range labels {
		if l == label {
			return labels
		}
	}
	return append(labels, label)
}
