package main

import (
	"context"
	"encoding/hex"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/eirueimi/unified-cd/internal/config"
	"github.com/eirueimi/unified-cd/internal/gittemplate"
	"github.com/eirueimi/unified-cd/internal/controller"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/secrets"
	"github.com/eirueimi/unified-cd/internal/store"
)

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
	flag.Parse()
	_ = f // registered to prevent "flag provided but not defined" error

	level, err := config.ParseLogLevel(*logLevel)
	if err != nil {
		slog.Error("invalid --log-level", "error", err)
		os.Exit(1)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

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

	srv := controller.NewServer(controller.Config{Token: *token, AgentToken: *token, ListenAddr: *addr, WebDir: *webDir, UIProxyTarget: *uiProxyTarget}, pg)
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
		})
		slog.Info("OIDC configured", "issuer", eff.OIDC.Issuer, "issuerInternal", eff.OIDC.IssuerInternal, "browserSSO", eff.OIDC.ClientSecret != "")
		if eff.OIDC.IssuerInternal == "" {
			slog.Warn("UNIFIED_OIDC_ISSUER_INTERNAL not set: /dex/* proxy is disabled. CLI device flow will fail. Use docker-compose.sso.yml")
		}
	}

	go controller.RunScheduler(ctx, pg, 200*time.Millisecond)
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
		go controller.RunLogArchiver(ctx, pg, obj, 30*time.Second)
		go controller.RunCacheCleanup(ctx, pg, obj)
	}
	go func() {
		var gitCache *gittemplate.Cache
		if obj != nil {
			gitCache = gittemplate.NewCache(obj)
		}
		fetcher := gittemplate.NewFetcher()
		resolver := gittemplate.NewResolver(fetcher, gitCache)
		controller.RunGitResolver(ctx, pg, resolver, km, 200*time.Millisecond)
	}()
	go func() {
		fetcher := gittemplate.NewFetcher()
		controller.RunAppSourceReconciler(ctx, pg, fetcher, km, 30*time.Second)
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
