package main

import (
	"context"
	"flag"
	"log/slog"
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
	configPath := flag.String("config", os.Getenv("UNIFIED_K8S_CONFIG"), "config file path (env: UNIFIED_K8S_CONFIG)")
	secretPath := flag.String("secret", os.Getenv("UNIFIED_K8S_SECRET"), "secret override file path (env: UNIFIED_K8S_SECRET)")
	logLevel := flag.String("log-level", os.Getenv("UNIFIED_K8S_LOG_LEVEL"), "log level: debug, info, warn, error (env: UNIFIED_K8S_LOG_LEVEL)")
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

	cfg, err := k8sagent.LoadConfig(*configPath, *secretPath)
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}
	if err := cfg.Validate(); err != nil {
		slog.Error("invalid config", "error", err)
		os.Exit(1)
	}
	if cfg.SidecarS3SecretName == "" {
		slog.Warn("sidecarS3SecretName is not set: cache steps will be no-ops (best-effort, reported Succeeded) and artifact upload/download steps will fail; set it to a Secret carrying UNIFIED_S3_* to enable sidecar transfers")
	}

	restCfg, err := buildRestConfig(cfg.Kubeconfig)
	if err != nil {
		slog.Error("k8s rest config", "error", err)
		os.Exit(1)
	}
	k8sClient, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		slog.Error("k8s client", "error", err)
		os.Exit(1)
	}

	masterClient := agentlib.NewClient(cfg.Server, cfg.Token)
	pm := k8sagent.NewPodManager(k8sClient, cfg.Namespace, cfg.PodImage)
	exec := k8sagent.NewExecutor(k8sClient, restCfg, cfg.Namespace)
	pool := k8sagent.NewPodPool(k8sClient, cfg.Namespace, pm)
	if d := cfg.PoolIdleTimeoutDuration(); d > 0 {
		pool.SetIdleTimeout(d)
	}
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

func buildRestConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig == "" {
		if cfg, err := rest.InClusterConfig(); err == nil {
			return cfg, nil
		}
		kubeconfig = clientcmd.RecommendedHomeFile
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}
