package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/eirueimi/unified-cd/internal/config"
	"github.com/eirueimi/unified-cd/internal/controller"
	"github.com/eirueimi/unified-cd/internal/gittemplate"
	"github.com/eirueimi/unified-cd/internal/metrics"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/secrets"
	"github.com/eirueimi/unified-cd/internal/store"
)

// auditRetentionDaysDefault resolves the --audit-retention-days flag default
// from UNIFIED_AUDIT_RETENTION_DAYS, falling back to 90 days when unset or
// invalid. 0 means keep forever.
func auditRetentionDaysDefault() int {
	const defaultDays = 90
	v := os.Getenv("UNIFIED_AUDIT_RETENTION_DAYS")
	if v == "" {
		return defaultDays
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		slog.Warn("invalid UNIFIED_AUDIT_RETENTION_DAYS, using default", "value", v, "default", defaultDays)
		return defaultDays
	}
	return n
}

func main() {
	// Pre-scan os.Args for -f so we can load the config file before defining
	// other flags. This gives priority: env vars → config file → CLI flags.
	configFile := config.FindFlag(os.Args[1:], "f")

	eff, err := config.ControllerEffective(configFile)
	if err != nil {
		slog.Error("config file", "error", err)
		os.Exit(1)
	}

	// Register flags with merged (env+file) defaults. Explicit flags override.
	f := flag.String("f", configFile, "config file path (YAML)")
	dsn := flag.String("dsn", eff.DSN, "postgres DSN (env: UNIFIED_DB_DSN)")
	addr := flag.String("addr", func() string {
		if eff.Addr != "" {
			return eff.Addr
		}
		return ":8080"
	}(), "listen address")
	token := flag.String("token", eff.Token, "static bearer token (env: UNIFIED_TOKEN)")
	s3Endpoint := flag.String("s3-endpoint", eff.S3Endpoint, "S3-compatible endpoint (env: UNIFIED_S3_ENDPOINT)")
	s3Bucket := flag.String("s3-bucket", eff.S3Bucket, "S3 bucket name (env: UNIFIED_S3_BUCKET)")
	s3Key := flag.String("s3-key", eff.S3Key, "S3 access key (env: UNIFIED_S3_KEY)")
	s3Secret := flag.String("s3-secret", eff.S3Secret, "S3 secret key (env: UNIFIED_S3_SECRET)")
	dataDir := flag.String("data-dir", eff.DataDir, "local object store directory (env: UNIFIED_DATA_DIR)")
	webDir := flag.String("web-dir", eff.WebDir, "static web assets directory; if empty /ui/* returns 404 (env: UNIFIED_WEB_DIR)")
	uiProxyTarget := flag.String("ui-proxy-target", eff.UIProxyTarget, "Vite dev server URL to reverse-proxy /ui/* to when --web-dir is empty, e.g. http://localhost:5173 (env: UNIFIED_UI_PROXY_TARGET)")
	logLevel := flag.String("log-level", os.Getenv("UNIFIED_LOG_LEVEL"), "log level: debug, info, warn, error (env: UNIFIED_LOG_LEVEL)")
	auditRetentionDays := flag.Int("audit-retention-days", auditRetentionDaysDefault(), "days to keep audit_logs rows; 0 = keep forever (env: UNIFIED_AUDIT_RETENTION_DAYS)")
	var matrixMaxEnvWarning string
	matrixMax := flag.Int("matrix-max-combinations", envIntOr("UNIFIED_MATRIX_MAX_COMBINATIONS", 64, &matrixMaxEnvWarning), "max combinations a matrix step may expand to (env: UNIFIED_MATRIX_MAX_COMBINATIONS)")
	flag.Parse()
	_ = f // registered to prevent "flag provided but not defined" error

	level, err := config.ParseLogLevel(*logLevel)
	if err != nil {
		slog.Error("invalid --log-level", "error", err)
		os.Exit(1)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	// envIntOr runs during flag registration, before the logger above exists,
	// so a malformed value's warning is collected then and only logged now.
	if matrixMaxEnvWarning != "" {
		slog.Warn(matrixMaxEnvWarning)
	}

	if *dsn == "" {
		slog.Error("dsn is required (--dsn, UNIFIED_DB_DSN, or config file)")
		os.Exit(1)
	}
	ssoConfigured := config.OIDCConfigured(eff.OIDC)
	if *token == "" && !ssoConfigured {
		slog.Error("token is required when SSO is not configured (--token, UNIFIED_TOKEN, config file, or UNIFIED_OIDC_ISSUER/UNIFIED_OIDC_CLIENT_ID)")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pg, err := store.NewPostgres(ctx, *dsn)
	if err != nil {
		slog.Error("store init", "error", err)
		os.Exit(1)
	}
	defer pg.Close()
	if err := pg.Migrate(*dsn); err != nil {
		slog.Error("migrate", "error", err)
		os.Exit(1)
	}

	// Metrics: DB-backed gauges + store decorator counting run/step
	// transitions. staleAfter=90s matches the stuck-run reaper's window.
	m := metrics.New()
	m.RegisterDBCollector(pg, 90*time.Second)
	st := metrics.NewInstrumentedStore(pg, m)

	// UNIFIED_TOKEN is synced to the DB as a PAT. This allows it to be listed
	// and deleted (revoked) via /api/v1/tokens just like regular PATs. If the
	// token is later unset, the previously synced row is removed.
	if *token != "" {
		if _, err := pg.UpsertBootstrapPAT(ctx, controller.BootstrapPATName, controller.HashToken(*token)); err != nil {
			slog.Error("sync UNIFIED_TOKEN as bootstrap PAT", "error", err)
			os.Exit(1)
		}
	} else if err := pg.DeleteBootstrapPATByName(ctx, controller.BootstrapPATName); err != nil {
		slog.Error("remove stale bootstrap PAT", "error", err)
		os.Exit(1)
	}

	// controllerKey: config file > env var > persisted DB value > generated on first start and saved to DB
	controllerKeyHex := eff.ControllerKey
	if controllerKeyHex == "" {
		candidate := hex.EncodeToString(secrets.GenerateKey())
		controllerKeyHex, err = pg.EnsureControllerKey(ctx, candidate)
		if err != nil {
			slog.Error("ensure controller key", "error", err)
			os.Exit(1)
		}
		if controllerKeyHex == candidate {
			slog.Info("controllerKey not set — generated a new key and persisted it to the database")
		} else {
			slog.Info("controllerKey not set — loaded previously persisted key from the database")
		}
	}
	km, err := secrets.NewLocalKeyManager(controllerKeyHex)
	if err != nil {
		slog.Error("key manager init", "error", err)
		os.Exit(1)
	}

	var obj objectstore.ObjectStore
	if *s3Endpoint != "" && *s3Bucket != "" {
		s3, err := objectstore.NewS3ObjectStore(ctx, objectstore.S3Config{
			Endpoint:        *s3Endpoint,
			Bucket:          *s3Bucket,
			AccessKeyID:     *s3Key,
			SecretAccessKey: *s3Secret,
		})
		if err != nil {
			slog.Error("s3 object store init", "error", err)
			os.Exit(1)
		}
		obj = s3
		slog.Info("using S3-compatible object store", "endpoint", *s3Endpoint, "bucket", *s3Bucket)
	} else if *dataDir != "" {
		obj = objectstore.NewLocalObjectStore(*dataDir)
		slog.Info("using local object store", "dir", *dataDir)
	} else {
		slog.Warn("no object store configured — log archival disabled")
	}

	srv := controller.NewServer(controller.Config{Token: *token, AgentToken: *token, ListenAddr: *addr, WebDir: *webDir, UIProxyTarget: *uiProxyTarget, MatrixMaxCombinations: *matrixMax}, st)
	srv.SetMetrics(m)
	srv.SetKeyManager(km)
	if obj != nil {
		srv.SetObjectStore(obj)
		srv.SetCacheStore(obj)
	}

	// OIDC configuration (config file > env vars)
	if eff.OIDC != nil && eff.OIDC.Issuer != "" && eff.OIDC.ClientID != "" {
		srv.SetOIDCConfig(&controller.OIDCConfig{
			Issuer:         eff.OIDC.Issuer,
			IssuerInternal: eff.OIDC.IssuerInternal,
			ExternalURL:    eff.OIDC.ExternalURL,
			ClientID:       eff.OIDC.ClientID,
			ClientSecret:   eff.OIDC.ClientSecret,
			DeviceClientID: eff.OIDC.DeviceClientID,
			RolesClaim:     eff.OIDC.RolesClaim,
			RoleMap:        eff.OIDC.RoleMap,
			UserMap:        eff.OIDC.UserMap,
			DefaultRole:    eff.OIDC.DefaultRole,
		})
		slog.Info("OIDC configured", "issuer", eff.OIDC.Issuer, "issuerInternal", eff.OIDC.IssuerInternal, "browserSSO", eff.OIDC.ClientSecret != "")
		if eff.OIDC.IssuerInternal == "" {
			slog.Warn("UNIFIED_OIDC_ISSUER_INTERNAL not set: /dex/* proxy is disabled. CLI device flow will fail. Use docker-compose.sso.yml")
		}
	}

	go controller.RunScheduler(ctx, st, 200*time.Millisecond)
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := pg.DeleteExpiredOIDCStates(context.Background()); err != nil {
					slog.Warn("oidc state cleanup", "error", err)
				}
			}
		}
	}()
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				n, err := pg.DeleteStaleAgents(ctx, 5*time.Minute)
				if err != nil {
					slog.Warn("deleteStaleAgents", "error", err)
				} else if n > 0 {
					slog.Info("deleteStaleAgents", "deleted", n)
				}
			}
		}
	}()
	if obj != nil {
		go controller.RunLogArchiver(ctx, st, obj, 30*time.Second)
		go controller.RunCacheCleanup(ctx, st, obj)
	}
	go controller.RunApprovalReaper(ctx, st, time.Minute)
	go controller.RunStuckRunReaper(ctx, st, 30*time.Second, 90*time.Second, 60*time.Second)
	// Fail runs that have sat Queued for >5m with no live agent able to claim
	// them (the agent they need disconnected), so they don't stay "in progress"
	// forever. staleAfter=90s matches the stuck-run reaper's agent-liveness window.
	go controller.RunQueuedRunReaper(ctx, st, 30*time.Second, 5*time.Minute, 90*time.Second)
	// Reap AppSources stuck in "Syncing" (bug #33): the manual sync-trigger API sets
	// sync_status="Syncing" synchronously, so a reconciler crash / restart / leadership
	// change mid-sync can strand the row forever. Reset any Syncing row older than 5m.
	go controller.RunAppSourceSyncReaper(ctx, st, 30*time.Second, 5*time.Minute)
	if *auditRetentionDays > 0 {
		slog.Info("audit log retention enabled", "retentionDays", *auditRetentionDays)
	} else {
		slog.Info("audit log retention disabled (keep forever)")
	}
	go controller.RunAuditRetention(ctx, st, time.Hour, *auditRetentionDays)
	go func() {
		var gitCache *gittemplate.Cache
		if obj != nil {
			gitCache = gittemplate.NewCache(obj)
		}
		fetcher := gittemplate.NewFetcher()
		resolver := gittemplate.NewResolver(fetcher, gitCache)
		controller.RunGitResolver(ctx, st, resolver, km, 200*time.Millisecond)
	}()
	go func() {
		fetcher := gittemplate.NewFetcher()
		controller.RunAppSourceReconciler(ctx, st, fetcher, km, 30*time.Second)
	}()

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Router(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		slog.Info("received shutdown signal, draining...")
		srv.SetShuttingDown()
		time.Sleep(2 * time.Second)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	slog.Info("controller listening", "addr", *addr)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("listen", "error", err)
		os.Exit(1)
	}
}

// envIntOr parses an integer environment variable, falling back to def when
// unset or malformed. It runs at flag-registration time, before the slog
// default logger is configured, so a malformed value can't be logged
// immediately; if warning is non-nil and the value fails to parse, *warning
// is set to a message the caller should log once the logger is ready.
func envIntOr(name string, def int, warning *string) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		} else if warning != nil {
			*warning = fmt.Sprintf("malformed %s=%q, falling back to default %d: %v", name, v, def, err)
		}
	}
	return def
}
