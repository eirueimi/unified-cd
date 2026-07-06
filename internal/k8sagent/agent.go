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

// imagePodStartTimeout bounds how long runImageStep waits for a throwaway
// image pod to reach Running. Under RestartPolicy: Never a pod stuck in
// Pending/ImagePullBackOff never transitions to Failed, so without a bound
// the wait would hang until the whole run is cancelled. This gives a bad
// image a fast, explicit failure instead.
const imagePodStartTimeout = 5 * time.Minute

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

	// scopePods tracks this claim's uses-scope pods (keyed by scopeKey: ScopeID
	// + MatrixKey), lazily created on first use by a scoped step and GC'd when
	// the claim ends (deferred below) regardless of how it finished. The main
	// stage/step loop (orchestrate, further below) executes every step —
	// including stage.Parallel groups — sequentially via a plain nested for
	// loop with no goroutines (unlike the host agent's runParallel), so this
	// map needs no mutex: ensureScopePod is never called concurrently within a
	// claim. If the k8s agent ever gains goroutine-based parallel execution,
	// this map and the check-then-create in ensureScopePod must be guarded by
	// a mutex, mirroring the host scopeManager's fix (see internal/agent/scope.go).
	scopePods := map[string]string{}
	ensureScopePod := func(execCtx context.Context, step api.ClaimStep) (string, error) {
		key := scopeKey(step)
		if name, ok := scopePods[key]; ok {
			return name, nil
		}
		env := imageStepEnv(step)
		pod := buildScopePod(c.RunID, a.cfg.Namespace, step.ScopeID, step.ScopeImage, env,
			SidecarSpec{Image: a.cfg.SidecarImage, S3SecretName: a.cfg.SidecarS3SecretName})
		created, err := a.pm.CreatePod(execCtx, pod)
		if err != nil {
			return "", fmt.Errorf("uses-scope %q (image %q): create pod: %w", step.ScopeID, step.ScopeImage, err)
		}
		name := created.Name
		waitCtx, cancel := context.WithTimeout(execCtx, imagePodStartTimeout)
		defer cancel()
		if err := a.pm.WaitForPodRunning(waitCtx, name); err != nil {
			// Best-effort cleanup of the pod that never became ready; the claim
			// end also sweeps scopePods, but this one never made it into the map.
			_ = a.pm.DeletePod(context.WithoutCancel(execCtx), name)
			return "", fmt.Errorf("uses-scope %q (image %q): pod did not become ready within %s: %w", step.ScopeID, step.ScopeImage, imagePodStartTimeout, err)
		}
		scopePods[key] = name
		return name, nil
	}
	defer func() {
		for key, name := range scopePods {
			if err := a.pm.DeletePod(context.WithoutCancel(ctx), name); err != nil {
				slog.Warn("k8s: failed to delete scope pod", "scopeKey", key, "pod", name, "error", err)
			}
		}
	}()

	stepExec := func(execCtx context.Context, step api.ClaimStep, expandedRun string) (int, string, error) {
		var stdoutBuf strings.Builder
		stderrPusher := agentlib.NewLogPusher(a.client, a.cfg.AgentID, c.RunID, step.Index, "stderr")
		stdoutWriter := io.MultiWriter(&stdoutBuf, &logLineWriter{
			client: a.client, agentID: a.cfg.AgentID, runID: c.RunID, stepIdx: step.Index, stream: "stdout",
		})

		var ec int
		var execErr error
		if step.ScopeID != "" {
			// uses-scope: exec into the step's dedicated scope pod (Task 9's
			// buildScopePod) instead of the pooled/run pod. Mutually exclusive
			// with RunsIn at the DSL level, so this takes precedence.
			podName, err := ensureScopePod(execCtx, step)
			if err != nil {
				return -1, "", err
			}
			ec, execErr = a.exec.ExecStep(execCtx, podName, "step", expandedRun, stdoutWriter, stderrPusher)
		} else if step.RunsIn != nil && step.RunsIn.Image != "" {
			// Isolated throwaway pod. UNIFIED_AGENT_OS mirrors the host agent's
			// convention; step.Env arrives already template-expanded (orchestrate).
			env := imageStepEnv(step)
			deadline := imageStepDeadline(step)
			ec, execErr = a.runImageStep(execCtx, c.RunID, step.RunsIn.Image, env, deadline, step.RunsIn.Resources, expandedRun, stdoutWriter, stderrPusher)
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

	sidecarExec := func(execCtx context.Context, targetPod, container string, argv []string) (int, error) {
		if targetPod == "" {
			targetPod = podName
		}
		stderrPusher := agentlib.NewLogPusher(a.client, a.cfg.AgentID, c.RunID, 0, "stderr")
		ec, err := a.exec.ExecStepArgv(execCtx, targetPod, container, argv, io.Discard, stderrPusher)
		stderrPusher.Flush(execCtx)
		return ec, err
	}

	a.orchestrate(ctx, c, stepExec, sidecarExec, mountPath, ensureScopePod)
}

// orchestrate runs the claim's stages, reporting step/run status, using stepExec
// to run each step's command. Pure of pod lifecycle so it is unit-testable.
// sidecarExec dispatches cache/artifact commands (argv, no shell) into a pod's
// unified-artifact sidecar container; its first argument selects the target
// pod (empty string means the default pooled/run pod).
// mountPath is the workspace volume's mount path inside the pod (default "/workspace").
// ensureScopePod lazily creates (or returns the cached) scope pod for a scoped
// cache/artifact step, so its sidecar call can target the scope pod's sidecar
// and scratch volume instead of the run pod's.
func (a *K8sAgent) orchestrate(ctx context.Context, c api.ClaimResponse, stepExec podStepExec, sidecarExec func(ctx context.Context, targetPod, container string, argv []string) (int, error), mountPath string, ensureScopePod func(ctx context.Context, step api.ClaimStep) (string, error)) {
	var anyStepFailed atomic.Bool
	var cancelledByMaster atomic.Bool
	statusView := func() dsl.RunStatusView {
		cancelled := cancelledByMaster.Load()
		return dsl.RunStatusView{Failed: anyStepFailed.Load() && !cancelled, Cancelled: cancelled}
	}

	// cacheSaveSpec captures a cache step's save parameters, deferred until
	// after the main stages complete (so the save captures the final
	// workspace state, matching the standard agent's cache semantics).
	// targetPod/sidecar record where the matching restore ran (the run pod or
	// a scoped step's scope pod), so the deferred save targets the same pod's
	// sidecar and filesystem.
	type cacheSaveSpec struct {
		key       string
		ttlDays   int
		path      string
		targetPod string
		sidecar   string
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
					slog.Info("k8s: received cancellation signal from master; interrupting run", "runID", c.RunID)
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
				// A scoped step's cache targets its scope pod's sidecar and
				// private scratch volume instead of the run pod's, so a scope's
				// cache never leaks into (or collides with) the shared workspace.
				sidecar, mount, targetPod := artifactSidecarName, mountPath, ""
				if step.ScopeID != "" {
					var err error
					targetPod, err = ensureScopePod(execCtx, step)
					if err != nil {
						slog.Warn("k8s: cache scope pod unavailable; skipping cache for step", "step", step.Name, "error", err)
						_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
							RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Succeeded", StartedAt: started, EndedAt: time.Now().UTC(),
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
					_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
						RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Failed", StartedAt: started, EndedAt: time.Now().UTC(),
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
					argv := []string{"unified-sidecar", "cache", "restore", "--key", key, "--path", cachePath}
					for _, rk := range restoreKeys {
						argv = append(argv, "--restore-key", rk)
					}
					// Best-effort: a miss/error never fails the step (the binary exits 0).
					_, _ = sidecarExec(execCtx, targetPod, sidecar, argv)

					ttlDays := step.Cache.TTLDays
					if ttlDays == 0 {
						ttlDays = 30
					}
					registerCacheSave(cacheSaveSpec{key: key, ttlDays: ttlDays, path: cachePath, targetPod: targetPod, sidecar: sidecar})
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
				// A scoped step's artifact upload reads from its scope pod's
				// private scratch volume via its sidecar, not the run pod's.
				sidecar, mount, targetPod := artifactSidecarName, mountPath, ""
				if step.ScopeID != "" {
					var err error
					targetPod, err = ensureScopePod(execCtx, step)
					if err != nil {
						// Artifacts are fail-loud: a scope pod that never becomes
						// available must fail the step, not silently upload from
						// the wrong (run pod) filesystem.
						_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
							RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Failed", StartedAt: started, EndedAt: time.Now().UTC(),
						})
						recordFailure(step)
						return
					}
					mount = scopeMountPath
				}
				argv := []string{"unified-sidecar", "artifact", "upload",
					"--run", c.RunID, "--name", step.UploadArtifact.Name,
					"--path", path.Join(mount, step.UploadArtifact.Path)}
				ec, err := sidecarExec(execCtx, targetPod, sidecar, argv)
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
				// A scoped step's artifact download writes into its scope pod's
				// private scratch volume via its sidecar, not the run pod's.
				sidecar, mount, targetPod := artifactSidecarName, mountPath, ""
				if step.ScopeID != "" {
					var err error
					targetPod, err = ensureScopePod(execCtx, step)
					if err != nil {
						// Artifacts are fail-loud: a scope pod that never becomes
						// available must fail the step, not silently download into
						// the wrong (run pod) filesystem.
						_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
							RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Failed", StartedAt: started, EndedAt: time.Now().UTC(),
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
				argv := []string{"unified-sidecar", "artifact", "download",
					"--run", c.RunID, "--name", step.DownloadArtifact.Name,
					"--dest", path.Join(mount, dest)}
				ec, err := sidecarExec(execCtx, targetPod, sidecar, argv)
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
					if step.MatrixKey != "" {
						sd := stepCtx.Steps[step.Name]
						if sd.Outputs == nil {
							sd.Outputs = map[string]any{}
						}
						for k, v := range outputs {
							m, _ := sd.Outputs[k].(map[string]string)
							if m == nil {
								m = map[string]string{}
							}
							m[step.MatrixKey] = v
							sd.Outputs[k] = m
						}
						stepCtx.Steps[step.Name] = sd
					} else {
						stepCtx.Steps[step.Name] = dsl.StepData{Outputs: dsl.StringOutputs(outputs)}
					}
					_ = a.client.SetStepOutputs(ctx, a.cfg.AgentID, c.RunID, step.Index, step.MatrixKey, outputs)
				}
				_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
					RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex,
					StepName: step.DisplayName(), Variant: step.MatrixKey, Status: status,
					ChildRunID: childRunID, CallJobName: step.Call.Job, EndedAt: time.Now().UTC(),
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
		// s.targetPod/s.sidecar were captured at restore time: for a scoped
		// step this is the scope pod (still alive here — it is only torn down
		// by executeRun's defer, after orchestrate returns), for a non-scoped
		// step it is the run pod (targetPod == "").
		if _, err := sidecarExec(saveCtx, s.targetPod, s.sidecar, argv); err != nil {
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
