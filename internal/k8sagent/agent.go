package k8sagent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/secrets"
	corev1 "k8s.io/api/core/v1"
)

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
	// (agentlib.RunClaim) and this pod's concrete execution environment. Its
	// scope-pod map is torn down at claim end by RunClaim's own deferred
	// b.CloseScopes, mirroring the pre-refactor scopePods defer (RunClaim
	// installs the masker itself via SetMasker after fetching secrets, so
	// this wrapper does neither).
	backend := newK8sBackend(a, c.RunID, podName, mountPath)

	agentlib.RunClaim(ctx, a.client, a.cfg.AgentID, c, backend)
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
// plus UNIFIED_AGENT_OS. Always a new map, so callers never mutate the claim.
// The scope pod runs a Linux container image regardless of the agent's host
// OS, so UNIFIED_AGENT_OS is "linux" — not the agent process's runtime.GOOS.
func imageStepEnv(step api.ClaimStep) map[string]string {
	env := make(map[string]string, len(step.Env)+1)
	for k, v := range step.Env {
		env[k] = v
	}
	env["UNIFIED_AGENT_OS"] = "linux"
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
