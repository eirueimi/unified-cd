package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/config"
	"github.com/eirueimi/unified-cd/internal/objectstore"
)

// orDefault returns s if it is non-empty, otherwise def. Used to give
// image flags a built-in default when neither env, config file, nor an
// explicit flag supplied one.
func orDefault(s, def string) string {
	if s != "" {
		return s
	}
	return def
}

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
	containerRuntime := flag.String("container-runtime", "", "container runtime for runsIn.image steps; empty = auto-detect (docker|podman|nerdctl|wslc). Apple's 'container' is explicit-only (not auto-detected) and cannot run isolated/claim-pod jobs")
	pauseImage := flag.String("pause-image", orDefault(eff.PauseImage, "busybox:1.36"), "image for the claim pod's pause (netns-holder) container")
	runnerImage := flag.String("runner-image", orDefault(eff.RunnerImage, "ghcr.io/eirueimi/unified-cd-runner:v0.0.3"), "default primary container image for isolated jobs without a podTemplate job container")
	minFreeDisk := flag.Uint64("min-free-disk", eff.MinFreeDisk, "minimum free space in bytes on the workspace filesystem required to keep claiming runs; 0 disables the check (host agent only) (env: UNIFIED_AGENT_MIN_FREE_DISK)")
	workspaceRetentionDays := flag.Int("workspace-retention-days", eff.WorkspaceRetentionDays, "age in days after which an inactive per-job workspace directory becomes eligible for removal by the opt-in workspace GC; 0 disables it (default; persistent workspaces are a feature) (host agent only) (env: UNIFIED_AGENT_WORKSPACE_RETENTION_DAYS)")
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

	// InstallShim writes the embedded ucd-sh binary into the agent's tools
	// dir (a ".ucd-tools" subdirectory INSIDE --workspace-dir, so it shares
	// whatever mount makes --workspace-dir visible to a possibly-remote
	// container runtime), which every claim-pod/uses-scope/cleanup
	// container this agent creates afterward bind-mounts read-only at
	// /.ucd. Called here (not inside agent.Run) so unit tests driving
	// Run() directly are unaffected by whether the two-stage build
	// populated internal/shim/embedded — see agent.InstallShim's doc
	// comment. A zero-byte embed is a hard, actionable startup failure.
	toolsDir, err := agent.InstallShim(*workspaceDir)
	if err != nil {
		slog.Error("install ucd-sh shim failed", "error", err)
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

	cli := agent.NewClient(*server, *token)

	// First SIGINT/SIGTERM begins a graceful shutdown (drain in-flight runs up to
	// DrainTimeout); a second signal forces an immediate shutdown. On the force
	// path, best-effort report the abandoned in-flight runs so the controller
	// fails them immediately instead of waiting for the stuck-run reaper.
	ctx, cancel := agent.ShutdownContext(func() {
		fctx, fcancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer fcancel()
		if n, err := cli.ReconcileRuns(fctx, *id); err != nil {
			slog.Warn("force shutdown: reconcile runs failed", "error", err)
		} else if n > 0 {
			slog.Warn("force shutdown: reported abandoned in-flight runs", "count", n)
		}
	})
	defer cancel()
	a := agent.NewWithLabels(*id, labels, cli)
	a.ExposeEnv = exposeEnv
	a.MaxConcurrent = *maxConcurrent
	a.CleanWorkspace = *cleanWorkspace
	a.WorkspaceDir = *workspaceDir
	a.DrainTimeout = *drainTimeout
	a.RuntimePref = *containerRuntime
	a.PauseImage = *pauseImage
	a.RunnerImage = *runnerImage
	a.ToolsDir = toolsDir
	a.MinFreeDisk = *minFreeDisk
	a.WorkspaceRetentionDays = *workspaceRetentionDays

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
