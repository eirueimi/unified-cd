package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/config"
	"github.com/eirueimi/unified-cd/internal/objectstore"
)

func main() {
	// Pre-scan os.Args for -f so we can load the config file before defining
	// other flags. This gives priority: env vars → config file → CLI flags.
	configFile := config.FindFlag(os.Args[1:], "f")
	if configFile == "" {
		if _, err := os.Stat("unified-agent.yaml"); err == nil {
			configFile = "unified-agent.yaml"
		}
	}

	eff, err := config.AgentEffective(configFile)
	if err != nil {
		slog.Error("config file", "error", err)
		os.Exit(1)
	}

	// ID defaults to the hostname. Explicit specification is only needed when running multiple agents on the same host.
	defaultID := eff.ID
	if defaultID == "" {
		if h, err := os.Hostname(); err == nil {
			defaultID = h
		}
	}

	// Register flags with merged (env+file) defaults. Explicit flags override.
	f := flag.String("f", configFile, "config file path (YAML) (default: unified-agent.yaml if exists)")
	server := flag.String("server", eff.Server, "master base URL")
	token := flag.String("token", eff.Token, "agent bearer token")
	id := flag.String("id", defaultID, "agent ID (default: hostname)")
	labelsStr := flag.String("labels", eff.LabelsString(), "comma-separated agent labels (env: UNIFIED_AGENT_LABELS)")
	exposeEnvStr := flag.String("expose-env", strings.Join(eff.ExposeEnv, ","), "comma-separated environment variable names to expose (env: UNIFIED_AGENT_EXPOSE_ENV)")
	cacheEndpoint := flag.String("cache-endpoint", eff.CacheEndpoint, "MinIO endpoint for cache (e.g. localhost:9000)")
	cacheKey := flag.String("cache-key", eff.CacheKey, "MinIO access key ID")
	cacheSecret := flag.String("cache-secret", eff.CacheSecret, "MinIO secret access key")
	cacheBucket := flag.String("cache-bucket", eff.CacheBucket, "MinIO bucket name")
	maxConcurrentDefault := eff.MaxConcurrent
	if maxConcurrentDefault == 0 {
		maxConcurrentDefault = 1
	}
	maxConcurrent := flag.Int("max-concurrent", maxConcurrentDefault, "maximum number of runs that can execute concurrently")
	cleanWorkspace := flag.Bool("clean-workspace", eff.CleanWorkspace, "delete and recreate the workspace before starting a run")
	workspaceDir := flag.String("workspace-dir", eff.WorkspaceDir, "base directory for run workspaces (default: ~/workspace) (env: UNIFIED_AGENT_WORKSPACE_DIR)")
	drainTimeout := flag.Duration("drain-timeout", eff.DrainTimeout, "maximum drain wait time after SIGTERM (0=wait indefinitely). Applies to running steps; post-hooks such as cache saves always wait for completion to preserve data")
	logLevel := flag.String("log-level", os.Getenv("UNIFIED_AGENT_LOG_LEVEL"), "log level: debug, info, warn, error (env: UNIFIED_AGENT_LOG_LEVEL)")
	containerRuntime := flag.String("container-runtime", "", "container runtime for runsIn.image steps (docker|podman|nerdctl|wslc|container); empty = auto-detect")
	flag.Parse()
	_ = f // registered to prevent "flag provided but not defined" error

	level, err := config.ParseLogLevel(*logLevel)
	if err != nil {
		slog.Error("invalid --log-level", "error", err)
		os.Exit(1)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	if *server == "" || *token == "" {
		slog.Error("--server and --token are required")
		os.Exit(1)
	}
	if err := agent.RequireShell(); err != nil {
		slog.Error("shell check failed", "error", err)
		os.Exit(1)
	}

	var labels []string
	if *labelsStr != "" {
		for _, l := range strings.Split(*labelsStr, ",") {
			if l = strings.TrimSpace(l); l != "" {
				labels = append(labels, l)
			}
		}
	}
	var exposeEnv []string
	if *exposeEnvStr != "" {
		for _, e := range strings.Split(*exposeEnvStr, ",") {
			if e = strings.TrimSpace(e); e != "" {
				exposeEnv = append(exposeEnv, e)
			}
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cli := agent.NewClient(*server, *token)
	a := agent.NewWithLabels(*id, labels, cli)
	a.ExposeEnv = exposeEnv
	a.MaxConcurrent = *maxConcurrent
	a.CleanWorkspace = *cleanWorkspace
	a.WorkspaceDir = *workspaceDir
	a.DrainTimeout = *drainTimeout
	a.RuntimePref = *containerRuntime

	if *cacheEndpoint != "" && *cacheKey != "" && *cacheSecret != "" && *cacheBucket != "" {
		cs, err := objectstore.NewS3ObjectStore(ctx, objectstore.S3Config{
			Endpoint:        *cacheEndpoint,
			Bucket:          *cacheBucket,
			AccessKeyID:     *cacheKey,
			SecretAccessKey: *cacheSecret,
		})
		if err != nil {
			slog.Warn("cache store init failed, cache disabled", "error", err)
		} else {
			a.CacheStore = cs
			slog.Info("cache enabled", "endpoint", *cacheEndpoint, "bucket", *cacheBucket)
		}
	}

	if err := a.Run(ctx); err != nil && ctx.Err() == nil {
		slog.Error("agent run", "error", err)
		os.Exit(1)
	}
}
