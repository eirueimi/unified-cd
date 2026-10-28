package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/config"
	"github.com/eirueimi/unified-cd/internal/k8sagent"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	// --version answers before config loading so it works in an image with no
	// config file mounted. The image is distroless (no shell), so this flag is
	// the only way to read the build out of a running k8s-agent container:
	// `kubectl exec <pod> -- /k8s-agent --version`.
	if config.VersionRequested(os.Args[1:]) {
		fmt.Println(agentlib.Version)
		return
	}

	configPath := flag.String("config", os.Getenv("UNIFIED_K8S_CONFIG"), "config file path (env: UNIFIED_K8S_CONFIG)")
	logLevel := flag.String("log-level", os.Getenv("UNIFIED_K8S_LOG_LEVEL"), "log level: debug, info, warn, error (env: UNIFIED_K8S_LOG_LEVEL)")
	// Handled by the pre-scan above; registered so it appears in -h output
	// and doesn't trip "flag provided but not defined".
	_ = flag.Bool("version", false, "print the k8s-agent version and exit")
	flag.Parse()

	level, err := config.ParseLogLevel(*logLevel)
	if err != nil {
		slog.Error("invalid --log-level", "error", err)
		os.Exit(1)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	if *configPath == "" {
		slog.Error("--config or UNIFIED_K8S_CONFIG is required")
		os.Exit(1)
	}

	cfg, err := k8sagent.LoadConfig(*configPath)
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}
	if err := cfg.Validate(); err != nil {
		slog.Error("invalid config", "error", err)
		os.Exit(1)
	}
	// The effective threshold, logged like the host agent's "disk gate:" line:
	// the loader ignores misspelled keys, so this is the operator's check that
	// the setting they wrote took effect.
	switch th := cfg.EffectiveMinFreeDisk(); {
	case th.Off:
		slog.Info("volume disk gate: disabled")
	case th.Bytes != 0:
		slog.Info(fmt.Sprintf("volume disk gate: minFreeDisk=%d bytes", th.Bytes))
	default:
		slog.Info("volume disk gate: minFreeDisk=" + th.String())
	}
	if cfg.WorkspaceSizeLimitUnset() {
		slog.Info("workspaceSizeLimit is unset: a run can fill the node's ephemeral storage; set workspaceSizeLimit (or podTemplates.<name>.workspaceSizeLimit) to cap the workspace emptyDir")
	}
	slog.Info("k8s: job pod policy", "allowed", cfg.JobPodPolicy.Summary())
	if cfg.SidecarS3SecretName == "" {
		slog.Warn("sidecarS3SecretName is not set: cache steps will be no-ops (best-effort, reported Succeeded) and any claim carrying an unconditional, run-failing artifact upload/download step will be failed immediately, before its Pod is created; set it to a Secret carrying UNIFIED_S3_* that exists in the JOB Pod namespace (this agent's namespace: field) to enable sidecar transfers", "jobNamespace", cfg.Namespace)
	}

	restCfg, err := buildRestConfig(cfg.Kubeconfig)
	if err != nil {
		slog.Error("k8s rest config", "error", err)
		os.Exit(1)
	}
	applyClientRateLimits(restCfg, cfg.MaxConcurrent)
	k8sClient, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		slog.Error("k8s client", "error", err)
		os.Exit(1)
	}

	masterClient, err := bootstrapAgentClient(context.Background(), &cfg, nil)
	if err != nil {
		slog.Error("bootstrap agent credentials", "error", err)
		os.Exit(1)
	}
	pm := k8sagent.NewPodManager(k8sClient, cfg.Namespace, cfg.PodImage)
	exec := k8sagent.NewExecutor(k8sClient, restCfg, cfg.Namespace)
	pool := k8sagent.NewPodPool(k8sClient, cfg.Namespace, pm)
	if d := cfg.PoolIdleTimeoutDuration(); d > 0 {
		pool.SetIdleTimeout(d)
	}
	// The finally/cleanup-phase ceiling lives on the shared orchestration loop
	// (agentlib.RunClaim), which both agent binaries call and which has no
	// per-agent config struct of its own — so it is set here, once, before any
	// claim starts. FinallyTimeoutDuration already applies the 10m fallback.
	agentlib.FinallyBudget = cfg.FinallyTimeoutDuration()

	ag := k8sagent.NewK8sAgent(cfg, masterClient, pm, exec, pool)

	// First SIGINT/SIGTERM begins a graceful shutdown; a second signal forces an
	// immediate shutdown. On the force path, best-effort report the abandoned
	// in-flight runs so the controller fails them immediately instead of
	// waiting for the stuck-run reaper.
	ctx, cancel := agentlib.ShutdownContext(func() {
		fctx, fcancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer fcancel()
		if n, err := masterClient.ReconcileRuns(fctx, cfg.AgentID); err != nil {
			slog.Warn("force shutdown: reconcile runs failed", "error", err)
		} else if n > 0 {
			slog.Warn("force shutdown: reported abandoned in-flight runs", "count", n)
		}
	})
	defer cancel()

	pool.StartEviction(ctx)

	if err := ag.Run(ctx); err != nil && ctx.Err() == nil {
		slog.Error("k8s agent run", "error", err)
		os.Exit(1)
	}
}

// defaultMaxConcurrent is what Config.Validate gives an unset maxConcurrent, and
// what the rate limit is sized for when maxConcurrent is negative (unlimited),
// where there is no bound to size against.
const defaultMaxConcurrent = 100

// clientRateLimits returns the client-side QPS and burst for the agent's
// Kubernetes clientset, sized for maxConcurrent runs.
//
// client-go defaults to 5 QPS and a burst of 10, and ONE limiter is shared by
// every call made through the clientset. The dominant consumer is not what one
// would guess: PodManager.WaitForPodRunning polls Pods().Get every 500 ms for
// the whole of each pod's start, which is 2 QPS per starting run, so a fleet of
// maxConcurrent runs starting together asks for 2*maxConcurrent QPS (200 at the
// default). Behind it come the waiting-run pod lists (one per run per 5 s, about
// maxConcurrent/5) and the create, delete and PVC lock calls, for which a flat
// 20 QPS is left. A token bucket queues without bound under over-demand, so
// at the defaults those polls make a pod create wait behind them for minutes.
//
// Step exec is NOT a consumer: remotecommand.NewSPDYExecutor builds its own
// round tripper and never touches this limiter. The burst is one second of the
// sustained rate.
func clientRateLimits(maxConcurrent int) (qps float32, burst int) {
	if maxConcurrent <= 0 {
		maxConcurrent = defaultMaxConcurrent
	}
	n := 2*maxConcurrent + maxConcurrent/5 + 20
	return float32(n), n
}

// applyClientRateLimits sets the clientset's rate limit for maxConcurrent runs.
// It overwrites unconditionally: neither rest.InClusterConfig nor the
// clientcmd loaders set QPS or Burst, so there is no operator-chosen value to
// preserve.
func applyClientRateLimits(c *rest.Config, maxConcurrent int) {
	c.QPS, c.Burst = clientRateLimits(maxConcurrent)
}

func buildRestConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig == "" {
		if cfg, err := rest.InClusterConfig(); err == nil {
			return cfg, nil
		}
		kubeconfig = clientcmd.RecommendedHomeFile
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

// bootstrapAgentClient resolves a Kubernetes workload identity before the
// agent registers, via the projected-ServiceAccount enrollment path. The
// controller, not the Pod name, is authoritative for the enrolled identity
// and labels.
func bootstrapAgentClient(ctx context.Context, cfg *k8sagent.Config, httpClient *http.Client) (*agentlib.Client, error) {
	source := k8sagent.NewKubernetesCredentialSource(k8sagent.KubernetesCredentialSourceConfig{
		Server: cfg.Server, Policy: cfg.EnrollmentPolicy, ServiceAccountTokenFile: cfg.ServiceAccountTokenFile,
		Labels: cfg.Labels, Capabilities: []string{"pod", "container"}, HTTPClient: httpClient,
	})
	if _, err := source.Token(ctx); err != nil {
		return nil, err
	}
	cfg.AgentID = source.AgentID()
	cfg.Labels = source.Labels()
	return agentlib.NewClientWithTokenSource(cfg.Server, source, httpClient), nil
}
