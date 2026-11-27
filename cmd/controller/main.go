package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/eirueimi/unified-cd/internal/config"
	"github.com/eirueimi/unified-cd/internal/controller"
	"github.com/eirueimi/unified-cd/internal/gittemplate"
	"github.com/eirueimi/unified-cd/internal/gittoken"
	"github.com/eirueimi/unified-cd/internal/metrics"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/store"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
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

// runRetentionDaysDefault resolves the --run-retention-days flag default from
// UNIFIED_RUN_RETENTION_DAYS, falling back to 0 (keep forever) when unset or
// invalid. Unlike audit retention this is opt-in: deleting run history is
// irreversible (it also removes the spec snapshot `run replay` uses).
func runRetentionDaysDefault() int {
	v := os.Getenv("UNIFIED_RUN_RETENTION_DAYS")
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		slog.Warn("invalid UNIFIED_RUN_RETENTION_DAYS, keeping runs forever", "value", v)
		return 0
	}
	return n
}

// logTrimDaysDefault resolves the --log-trim-days flag default from
// UNIFIED_LOG_TRIM_DAYS, falling back to 0 (never trim) when unset or
// invalid. Trimming deletes the DB copy of archived logs; reads then come
// from the object store, so this is opt-in like run retention.
func logTrimDaysDefault() int {
	v := os.Getenv("UNIFIED_LOG_TRIM_DAYS")
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		slog.Warn("invalid UNIFIED_LOG_TRIM_DAYS, never trimming", "value", v)
		return 0
	}
	return n
}

// gitResolveDeadlineDefault resolves UNIFIED_GIT_RESOLVE_DEADLINE (a Go
// duration). Runs whose git-template resolution keeps failing longer than
// this are Failed instead of staying Pending forever. Unset, invalid, or
// non-positive values fall back to 1h — 0 does NOT disable the deadline
// (disabling would reintroduce immortal-Pending runs).
func gitResolveDeadlineDefault() time.Duration {
	const def = time.Hour
	v := os.Getenv("UNIFIED_GIT_RESOLVE_DEADLINE")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		slog.Warn("invalid UNIFIED_GIT_RESOLVE_DEADLINE, using default", "value", v, "default", def)
		return def
	}
	return d
}

// queuedRunGraceDefault resolves the queued-run reaper grace period from
// UNIFIED_QUEUED_RUN_GRACE (a Go duration string such as "5m" or "20m"),
// falling back to 5 minutes when unset, invalid, or non-positive. This is
// how long a run may sit Queued with no eligible live agent before being
// failed - raise it in environments where a full agent outage (e.g. a
// node-pool upgrade) can exceed the default.
func queuedRunGraceDefault() time.Duration {
	const def = 5 * time.Minute
	v := os.Getenv("UNIFIED_QUEUED_RUN_GRACE")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		slog.Warn("invalid UNIFIED_QUEUED_RUN_GRACE, using default", "value", v, "default", def)
		return def
	}
	return d
}

func postgresPoolConfigFromEnv() (store.PostgresPoolConfig, []string) {
	cfg := store.DefaultPostgresPoolConfig()
	var warnings []string
	values := []struct {
		name   string
		target *int32
	}{
		{name: "UNIFIED_DB_API_MAX_CONNS", target: &cfg.APIMaxConns},
		{name: "UNIFIED_DB_BACKGROUND_MAX_CONNS", target: &cfg.BackgroundMaxConns},
		{name: "UNIFIED_DB_LOCK_MAX_CONNS", target: &cfg.LockMaxConns},
		{name: "UNIFIED_DB_LISTEN_MAX_CONNS", target: &cfg.ListenMaxConns},
	}
	for _, value := range values {
		raw := os.Getenv(value.name)
		if raw == "" {
			continue
		}
		n, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || n <= 0 {
			warnings = append(warnings, fmt.Sprintf(
				"invalid %s=%q, using default", value.name, raw))
			continue
		}
		*value.target = int32(n)
	}
	return cfg, warnings
}

func configureKubernetesEnrollmentClient(kubeConfig *rest.Config) {
	// The verifier also sets a per-request context deadline. Set the transport
	// timeout as a production backstop for Kubernetes client operations.
	kubeConfig.Timeout = controller.KubernetesEnrollmentRequestTimeout
}

type enrollmentPolicyBootstrapStore interface {
	UpsertAgentEnrollmentPolicy(context.Context, store.AgentEnrollmentPolicy) (*store.AgentEnrollmentPolicy, error)
}

// bootstrapKubernetesEnrollmentPolicies persists declarative workload identity
// policies before the controller starts accepting agent enrollment requests.
func bootstrapKubernetesEnrollmentPolicies(ctx context.Context, st enrollmentPolicyBootstrapStore, auth *config.ControllerAgentAuthConfig) error {
	if auth == nil {
		return nil
	}
	for _, configured := range auth.KubernetesEnrollmentPolicies {
		policy, err := configured.StorePolicy()
		if err != nil {
			return err
		}
		if _, err := st.UpsertAgentEnrollmentPolicy(ctx, policy); err != nil {
			return err
		}
	}
	return nil
}

// serverFlags holds the pointers to every flag the controller server
// registers, plus the env-parse warnings collected while registering.
type serverFlags struct {
	f, dsn, addr, externalURL, token                                         *string
	s3Endpoint, s3Bucket, s3Key, s3Secret, s3Region, artifactDownload        *string
	dataDir, webDir, uiProxyTarget, trustedProxies, logLevel                 *string
	gitKnownHostsFlag                                                        *string
	s3UseSSL, stderrPlain, insecureCookies, storeCredentialRequireRunBinding *bool
	customMetricsEnabled                                                     *bool
	maxRequestBodyBytes, gitMaxRepoBytes                                     *int64
	storeCredBurst, customMetricsMaxSeries                                   *int
	auditRetentionDays, runRetentionDays, logTrimDays                        *int
	matrixMax, webhookMaxBodyBytes                                           *int
	storeCredPerSecond                                                       *float64
	customMetricsTTL, gitTimeout                                             *time.Duration

	// Set while registering, before the logger exists; logged by main.
	matrixMaxEnvWarning, webhookMaxBodyBytesEnvWarning string
}

// registerServerFlags registers the controller server flags on fs with the
// merged (env + config file) defaults from eff.
func registerServerFlags(fs *flag.FlagSet, eff *config.ControllerConfig, configFile string) *serverFlags {
	sf := &serverFlags{}
	sf.f = fs.String("f", configFile, "config file path (YAML)")
	// Handled by the pre-scan above; registered so it appears in -h output
	// and doesn't trip "flag provided but not defined".
	_ = fs.Bool("version", false, "print the controller version and exit")
	sf.dsn = fs.String("dsn", eff.DSN, "postgres DSN (env: UNIFIED_DB_DSN)")
	sf.addr = fs.String("addr", func() string {
		if eff.Addr != "" {
			return eff.Addr
		}
		return ":8080"
	}(), "listen address")
	sf.externalURL = fs.String("external-url", eff.ExternalURL, "controller public base URL, used for run links (UNIFIED_RUN_URL); falls back to oidc.externalUrl (env: UNIFIED_EXTERNAL_URL)")
	// Deprecated and ignored: kept registered so an existing deployment that
	// passes --token / --allow-default-token still starts (spec 6.8).
	sf.token = fs.String("token", "", "deprecated and ignored: the controller no longer authenticates with a static token")
	sf.s3Endpoint = fs.String("s3-endpoint", eff.S3Endpoint, "S3-compatible endpoint (env: UNIFIED_S3_ENDPOINT)")
	sf.s3Bucket = fs.String("s3-bucket", eff.S3Bucket, "S3 bucket name (env: UNIFIED_S3_BUCKET)")
	sf.s3Key = fs.String("s3-key", eff.S3Key, "S3 access key (env: UNIFIED_S3_KEY)")
	sf.s3Secret = fs.String("s3-secret", eff.S3Secret, "S3 secret key (env: UNIFIED_S3_SECRET)")
	sf.s3UseSSL = fs.Bool("s3-use-ssl", eff.S3UseSSL, "reach the S3 endpoint over https; required for presigned URLs on AWS S3 and GCS (env: UNIFIED_S3_USE_SSL)")
	sf.s3Region = fs.String("s3-region", eff.S3Region, "S3 bucket region; \"auto\" for GCS (env: UNIFIED_S3_REGION)")
	sf.artifactDownload = fs.String("artifact-download", eff.ArtifactDownloadMode(), "how human artifact downloads are served: \"proxy\" streams through the controller, \"redirect\" answers with a 15-minute presigned GET URL (S3-compatible store only); agents are always proxied (env: UNIFIED_ARTIFACT_DOWNLOAD)")
	sf.dataDir = fs.String("data-dir", eff.DataDir, "local object store directory (env: UNIFIED_DATA_DIR)")
	sf.webDir = fs.String("web-dir", eff.WebDir, "static web assets directory; if empty /ui/* returns 404 (env: UNIFIED_WEB_DIR)")
	sf.uiProxyTarget = fs.String("ui-proxy-target", eff.UIProxyTarget, "Vite dev server URL to reverse-proxy /ui/* to when --web-dir is empty, e.g. http://localhost:5173 (env: UNIFIED_UI_PROXY_TARGET)")
	sf.stderrPlain = fs.Bool("log-stderr-plain", eff.StderrPlain, "render step stderr in the run log the same color as stdout instead of red (env: UNIFIED_LOG_STDERR_PLAIN)")
	sf.insecureCookies = fs.Bool("insecure-cookies", eff.InsecureCookies, "do not set the Secure attribute on session cookies (env: UNIFIED_INSECURE_COOKIES)")
	sf.storeCredentialRequireRunBinding = fs.Bool("store-credential-require-run-binding", eff.StoreCredentialRequireRunBinding, "deprecated and ignored: run binding is always required for presigned object access (env: UNIFIED_STORE_CREDENTIAL_REQUIRE_RUN_BINDING)")
	_ = fs.Bool("allow-default-token", false, "deprecated and ignored: the controller no longer has a default admin token")
	sf.trustedProxies = fs.String("trusted-proxies", strings.Join(eff.TrustedProxies, ","), "comma-separated CIDRs/IPs of reverse proxies whose X-Forwarded-For / X-Real-IP headers are trusted for the client IP; empty ignores those headers (env: UNIFIED_TRUSTED_PROXIES)")
	sf.maxRequestBodyBytes = fs.Int64("max-request-body-bytes", func() int64 {
		if eff.MaxRequestBodyBytes > 0 {
			return eff.MaxRequestBodyBytes
		}
		return 1 << 20
	}(), "max size in bytes of an API request body; larger bodies are rejected with 413. Webhook ingress (--webhook-max-body-bytes) and artifact upload have their own limits (env: UNIFIED_MAX_REQUEST_BODY_BYTES)")
	sf.storeCredBurst = fs.Int("store-credential-rate-burst", func() int {
		if eff.StoreCredentialRateLimit.Burst > 0 {
			return eff.StoreCredentialRateLimit.Burst
		}
		return 200
	}(), "burst size of the rate limit on POST /api/v1/runs/{runID}/objects/presign (per agent, or per client address for sidecars) (env: UNIFIED_STORE_CREDENTIAL_RATE_BURST)")
	sf.storeCredPerSecond = fs.Float64("store-credential-rate-per-second", func() float64 {
		if eff.StoreCredentialRateLimit.PerSecond > 0 {
			return eff.StoreCredentialRateLimit.PerSecond
		}
		return 20
	}(), "sustained requests per second of the same rate limits (env: UNIFIED_STORE_CREDENTIAL_RATE_PER_SECOND)")
	sf.customMetricsEnabled = fs.Bool("custom-metrics-enabled", eff.CustomMetricsEnabled(), "emit and export job-defined custom metrics (metrics: blocks of a Job); when false they still validate at apply time but nothing is emitted (env: UNIFIED_CUSTOM_METRICS_ENABLED)")
	sf.customMetricsMaxSeries = fs.Int("custom-metrics-max-series-per-metric", eff.CustomMetricsMaxSeries(), "max label combinations per custom metric name per replica; new combinations over the cap are dropped; 0 = unlimited (env: UNIFIED_CUSTOM_METRICS_MAX_SERIES_PER_METRIC)")
	sf.customMetricsTTL = fs.Duration("custom-metrics-ttl", eff.CustomMetricsTTL(), "drop a custom metric series that has not been updated for this long; 0 disables expiry (env: UNIFIED_CUSTOM_METRICS_TTL)")
	sf.logLevel = fs.String("log-level", os.Getenv("UNIFIED_LOG_LEVEL"), "log level: debug, info, warn, error (env: UNIFIED_LOG_LEVEL)")
	sf.auditRetentionDays = fs.Int("audit-retention-days", auditRetentionDaysDefault(), "days to keep audit_logs rows; 0 = keep forever (env: UNIFIED_AUDIT_RETENTION_DAYS)")
	sf.runRetentionDays = fs.Int("run-retention-days", runRetentionDaysDefault(), "days to keep terminal runs incl. their logs, log archives, and artifacts; 0 = keep forever (env: UNIFIED_RUN_RETENTION_DAYS)")
	sf.logTrimDays = fs.Int("log-trim-days", logTrimDaysDefault(), "days after archival to delete a run's DB log rows (reads switch to the archive); 0 = never trim (env: UNIFIED_LOG_TRIM_DAYS)")
	sf.matrixMax = fs.Int("matrix-max-combinations", envIntOr("UNIFIED_MATRIX_MAX_COMBINATIONS", 64, &sf.matrixMaxEnvWarning), "max combinations a matrix step may expand to (env: UNIFIED_MATRIX_MAX_COMBINATIONS)")
	sf.webhookMaxBodyBytes = fs.Int("webhook-max-body-bytes", envIntOr("UNIFIED_WEBHOOK_MAX_BODY_BYTES", 1<<20, &sf.webhookMaxBodyBytesEnvWarning), "max webhook ingress request body size in bytes; larger bodies are rejected with 413 instead of being truncated (env: UNIFIED_WEBHOOK_MAX_BODY_BYTES)")
	sf.gitTimeout = fs.Duration("git-timeout", gitTimeoutDefault(), "maximum time for one controller-side git operation (AppSource fetch, ls-remote, git:// template fetch) before its process tree is killed (env: UNIFIED_GIT_TIMEOUT)")
	sf.gitMaxRepoBytes = fs.Int64("git-max-repo-bytes", gitMaxRepoBytesDefault(), "abort a controller-side git fetch whose repository exceeds this many bytes; 0 = no limit (default 1 GiB, env: UNIFIED_GIT_MAX_REPO_BYTES)")
	sf.gitKnownHostsFlag = fs.String("git-known-hosts-file", os.Getenv("UNIFIED_GIT_KNOWN_HOSTS_FILE"), "persistent SSH known_hosts file for trust-on-first-use of git SSH host keys when a GitCredential has no knownHosts; defaults to <data-dir>/.ssh/known_hosts, else a temp dir (env: UNIFIED_GIT_KNOWN_HOSTS_FILE)")
	return sf
}

// loadServerFlags loads the effective controller configuration and parses the
// server flags from args. The config file is found with a pre-scan of args
// for -f so it can supply the flag defaults, which gives the priority env
// vars -> config file -> CLI flags. The values of the flags that mirror
// configuration fields (-f, --dsn, --addr) are written back into the returned
// config.
func loadServerFlags(args []string, fs *flag.FlagSet) (*config.ControllerConfig, *serverFlags, error) {
	configFile := config.FindFlag(args, "f")
	eff, err := config.ControllerEffective(configFile)
	if err != nil {
		return nil, nil, err
	}
	sf := registerServerFlags(fs, eff, configFile)
	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}
	eff.DSN = *sf.dsn
	eff.Addr = *sf.addr
	return eff, sf, nil
}

// loadRuntimeConfig loads the controller configuration the way the server
// does: environment variables, then the config file named by -f, then flags
// from args registered on fs.
func loadRuntimeConfig(args []string, fs *flag.FlagSet) (*config.ControllerConfig, error) {
	eff, _, err := loadServerFlags(args, fs)
	return eff, err
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "admin" {
		os.Exit(runAdmin(os.Args[2:], os.Stdout, os.Stderr))
	}

	// --version answers before anything else so it works in an image with no
	// config file and no DSN: `docker run <image> --version`.
	if config.VersionRequested(os.Args[1:]) {
		fmt.Println(controller.BuildVersion())
		return
	}

	eff, sf, err := loadServerFlags(os.Args[1:], flag.CommandLine)
	if err != nil {
		slog.Error("config file", "error", err)
		os.Exit(1)
	}
	f, dsn, addr, externalURL, token := sf.f, sf.dsn, sf.addr, sf.externalURL, sf.token
	s3Endpoint, s3Bucket, s3Key, s3Secret, s3Region, artifactDownload := sf.s3Endpoint, sf.s3Bucket, sf.s3Key, sf.s3Secret, sf.s3Region, sf.artifactDownload
	dataDir, webDir, uiProxyTarget, trustedProxies, logLevel := sf.dataDir, sf.webDir, sf.uiProxyTarget, sf.trustedProxies, sf.logLevel
	gitKnownHostsFlag := sf.gitKnownHostsFlag
	s3UseSSL, stderrPlain, insecureCookies, storeCredentialRequireRunBinding := sf.s3UseSSL, sf.stderrPlain, sf.insecureCookies, sf.storeCredentialRequireRunBinding
	customMetricsEnabled := sf.customMetricsEnabled
	maxRequestBodyBytes, gitMaxRepoBytes := sf.maxRequestBodyBytes, sf.gitMaxRepoBytes
	storeCredBurst, customMetricsMaxSeries := sf.storeCredBurst, sf.customMetricsMaxSeries
	auditRetentionDays, runRetentionDays, logTrimDays := sf.auditRetentionDays, sf.runRetentionDays, sf.logTrimDays
	matrixMax, webhookMaxBodyBytes := sf.matrixMax, sf.webhookMaxBodyBytes
	storeCredPerSecond := sf.storeCredPerSecond
	customMetricsTTL, gitTimeout := sf.customMetricsTTL, sf.gitTimeout
	matrixMaxEnvWarning, webhookMaxBodyBytesEnvWarning := sf.matrixMaxEnvWarning, sf.webhookMaxBodyBytesEnvWarning
	_ = f // registered to prevent "flag provided but not defined" error

	level, err := config.ParseLogLevel(*logLevel)
	if err != nil {
		slog.Error("invalid --log-level", "error", err)
		os.Exit(1)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	// First line of every controller log: which build this is. Logged before
	// the DB connection so a controller that dies during migration still
	// tells the operator what version it was.
	slog.Info("unified-cd controller starting", "version", controller.BuildVersion())

	// envIntOr runs during flag registration, before the logger above exists,
	// so a malformed value's warning is collected then and only logged now.
	if matrixMaxEnvWarning != "" {
		slog.Warn(matrixMaxEnvWarning)
	}
	if webhookMaxBodyBytesEnvWarning != "" {
		slog.Warn(webhookMaxBodyBytesEnvWarning)
	}
	trustedProxyPrefixes, err := config.ParseTrustedProxies(config.SplitCSV(*trustedProxies))
	if err != nil {
		slog.Error("invalid --trusted-proxies", "error", err)
		os.Exit(1)
	}
	if *dsn == "" {
		slog.Error("dsn is required (--dsn, UNIFIED_DB_DSN, or config file)")
		os.Exit(1)
	}
	ssoConfigured := config.OIDCConfigured(eff.OIDC)
	if *token != "" || eff.Token != "" {
		slog.Error("UNIFIED_TOKEN is no longer used by the controller and is ignored; see docs/operator-manual/migrations/2026-11-27-internal-accounts.md")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// eff.Warnings already covers the env var and the config file; the flag
	// is the remaining way to set the deprecated knob.
	for _, w := range eff.Warnings {
		slog.Warn(w)
	}
	if *storeCredentialRequireRunBinding && !eff.StoreCredentialRequireRunBinding {
		slog.Warn(config.RequireRunBindingDeprecatedWarning)
	}

	poolCfg, poolWarnings := postgresPoolConfigFromEnv()
	for _, warning := range poolWarnings {
		slog.Warn(warning)
	}
	slog.Info("postgres pool configuration",
		"apiMaxConns", poolCfg.APIMaxConns,
		"backgroundMaxConns", poolCfg.BackgroundMaxConns,
		"lockMaxConns", poolCfg.LockMaxConns,
		"listenMaxConns", poolCfg.ListenMaxConns)
	pg, err := store.NewPostgresWithPoolConfig(ctx, *dsn, poolCfg)
	if err != nil {
		slog.Error("store init", "error", err)
		os.Exit(1)
	}
	defer pg.Close()
	if err := pg.Migrate(*dsn); err != nil {
		slog.Error("migrate", "error", err)
		os.Exit(1)
	}
	// Scrub credentials that older versions let users embed in an AppSource
	// repoURL. The URL is rewritten without userinfo, so those AppSources need
	// a GitCredential for their host to keep authenticating.
	if names, err := pg.SanitizeAppSourceCredentials(ctx); err != nil {
		slog.Error("sanitize AppSource repoURL credentials", "error", err)
		os.Exit(1)
	} else if len(names) > 0 {
		slog.Warn("removed credentials embedded in AppSource repoURL; create a GitCredential for each repository host so these AppSources can authenticate again",
			"appSources", names)
	}
	// Make the service-account rows match the config: new names are created,
	// removed names are disabled (re-adding one re-enables it).
	serviceAccountNames := make([]string, 0, len(eff.ServiceAccountGrants))
	for name := range eff.ServiceAccountGrants {
		serviceAccountNames = append(serviceAccountNames, name)
	}
	sort.Strings(serviceAccountNames)
	if err := pg.SyncServiceAccounts(ctx, serviceAccountNames); err != nil {
		slog.Error("sync service accounts", "error", err)
		os.Exit(1)
	}
	if err := bootstrapKubernetesEnrollmentPolicies(ctx, pg, eff.AgentAuth); err != nil {
		slog.Error("bootstrap kubernetes enrollment policies", "error", err)
		os.Exit(1)
	}

	// Metrics: DB-backed gauges + store decorator counting run/step
	// transitions. staleAfter=90s matches the stuck-run reaper's window.
	m := metrics.NewForController(controller.BuildVersion(), pg, 90*time.Second)
	st := metrics.NewInstrumentedStore(pg, m)
	backgroundPG := pg.BackgroundStore()
	backgroundSt := metrics.NewInstrumentedStore(backgroundPG, m)

	// The one-time initial admin token (spec 6.4). Startup rule (spec 6.8): the
	// controller starts when SSO is configured, the bootstrap is done, or the
	// initial token could be generated or was provided.
	initialDirs := initialTokenDirs(*f, *dataDir)
	if err := validateProvidedInitialToken(os.Getenv("UNIFIED_INITIAL_ADMIN_TOKEN"), eff.KeySource.DevMode); err != nil {
		slog.Error("initial admin token", "error", err)
		os.Exit(1)
	}
	if _, err := prepareInitialToken(ctx, pg, ssoConfigured, os.Getenv("UNIFIED_INITIAL_ADMIN_TOKEN"), initialDirs); err != nil {
		state, stateErr := pg.GetBootstrapState(ctx)
		if !ssoConfigured && (stateErr != nil || !state.Done) {
			if errors.Is(err, errNoWritableDir) {
				slog.Error("cannot write the initial admin token: no writable config or data directory; set UNIFIED_INITIAL_ADMIN_TOKEN")
			} else {
				slog.Error("initial admin token", "error", err)
			}
			os.Exit(1)
		}
		slog.Warn("initial admin token", "error", err)
	}
	if ssoConfigured && eff.AdminAccountEnabled() {
		slog.Warn("the built-in admin service account is enabled while SSO is configured; set serviceAccounts.admin.enabled: false once SSO works")
	}

	resolved, err := eff.KeySource.Resolve(ctx)
	if err != nil {
		slog.Error("encryption key", "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := resolved.Close(); err != nil {
			slog.Warn("encryption key: close failed", "error", err)
		}
	}()
	for _, w := range resolved.Warnings {
		slog.Warn(w)
	}
	slog.Info("encryption key loaded", "source", resolved.Description)
	km := resolved.KeyManager

	var obj objectstore.ObjectStore
	objectStoreState := "none"
	if *s3Endpoint != "" && *s3Bucket != "" {
		cfg := objectstore.S3Config{
			Endpoint:        *s3Endpoint,
			Bucket:          *s3Bucket,
			AccessKeyID:     *s3Key,
			SecretAccessKey: *s3Secret,
			UseSSL:          *s3UseSSL,
			Region:          *s3Region,

			MultipartPresign: objectstore.MultipartPresignSupported(*s3Endpoint),
		}
		s3, err := objectstore.NewS3ObjectStore(ctx, cfg)
		if err != nil {
			slog.Error("s3 object store init", "error", err)
			os.Exit(1)
		}
		obj = s3
		objectStoreState = "s3"
		slog.Info("using S3-compatible object store", "endpoint", *s3Endpoint, "bucket", *s3Bucket)
	} else if *dataDir != "" {
		obj = objectstore.NewLocalObjectStore(*dataDir)
		objectStoreState = "local"
		slog.Info("using local object store", "dir", *dataDir)
	} else {
		slog.Warn("no object store configured — log archival disabled")
	}

	_, canPresign := obj.(objectstore.Presigner)
	if err := config.CheckArtifactDownload(*artifactDownload, canPresign); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}

	verifiers := make(map[string]controller.KubernetesEnrollmentVerifier, len(eff.AgentAuth.KubernetesClusters))
	// clusterClients is keyed the same as verifiers (by cluster name) and
	// reuses the SAME kubernetes.Interface built below for each cluster —
	// one TokenReview-capable client per cluster serves both the enrollment
	// verifier above and the sidecar authentication below, which differ
	// only in which audience they ask the API server to confirm. Building a
	// second client per cluster would be a duplicate, unnecessary
	// kubeconfig/rest.Config resolution for a client that behaves
	// identically to the one already made.
	clusterClients := make(map[string]kubernetes.Interface, len(eff.AgentAuth.KubernetesClusters))
	for _, cluster := range eff.AgentAuth.KubernetesClusters {
		kubeConfig, err := buildKubernetesEnrollmentConfig(cluster.Kubeconfig)
		if err != nil {
			slog.Error("kubernetes enrollment cluster configuration", "cluster", cluster.Name, "error", err)
			os.Exit(1)
		}
		configureKubernetesEnrollmentClient(kubeConfig)
		client, err := kubernetes.NewForConfig(kubeConfig)
		if err != nil {
			slog.Error("kubernetes enrollment client", "cluster", cluster.Name, "error", err)
			os.Exit(1)
		}
		verifiers[cluster.Name] = controller.NewKubernetesEnrollmentVerifier(cluster.Name, client)
		clusterClients[cluster.Name] = client
	}

	// storeCredentialClusters is the sidecar authentication's
	// namespace/ServiceAccount allowlist, one entry per configured
	// agentAuth.kubernetesStoreCredentialPolicies policy — see
	// ControllerAgentAuthConfig.KubernetesStoreCredentialPolicies and
	// controller.StoreCredentialCluster for why this is a separate list from
	// the enrollment policies above rather than a reuse of them.
	// validateControllerAgentAuth already rejected any policy referencing an
	// unknown cluster name, so the clusterClients lookup below cannot miss.
	var storeCredentialClusters []controller.StoreCredentialCluster
	if eff.AgentAuth != nil {
		for _, policy := range eff.AgentAuth.KubernetesStoreCredentialPolicies {
			storeCredentialClusters = append(storeCredentialClusters, controller.StoreCredentialCluster{
				Cluster: policy.Cluster, Client: clusterClients[policy.Cluster],
				Namespaces: policy.Namespaces, ServiceAccounts: policy.ServiceAccounts,
			})
		}
	}

	srv := controller.NewServer(controller.Config{ServiceAccountGrants: eff.ServiceAccountGrants, AdminEnabled: eff.AdminAccountEnabled(), TokenDefaultLifetime: eff.TokenDefaultLifetime(), TokenMaxLifetime: eff.TokenMaxLifetime(), AllowTokenEntry: eff.UI.AllowTokenEntry, KubernetesEnrollmentVerifiers: verifiers, StoreCredentialClusters: storeCredentialClusters, ListenAddr: *addr, WebDir: *webDir, UIProxyTarget: *uiProxyTarget, MatrixMaxCombinations: *matrixMax, WebhookMaxBodyBytes: int64(*webhookMaxBodyBytes), TrustedProxies: trustedProxyPrefixes, MaxRequestBodyBytes: *maxRequestBodyBytes, StoreCredentialRateBurst: *storeCredBurst, StoreCredentialRatePerSecond: *storeCredPerSecond, StderrPlain: *stderrPlain, InsecureCookies: *insecureCookies, ArtifactDownload: *artifactDownload, ExternalURL: *externalURL, InitialTokenDirs: initialDirs}, st)
	// Stops the shared log-notify listener goroutine (started lazily on the
	// first SSE viewer — see (*controller.Server).subscribeLogNotify) if it
	// was ever started. A no-op otherwise. Deferred here rather than left to
	// process exit so the listenPool connection it may hold is released
	// deterministically during graceful shutdown, not just implicitly by the
	// OS reclaiming the process.
	defer srv.Close()
	srv.SetMetrics(m)
	if *customMetricsMaxSeries < 0 || *customMetricsTTL < 0 {
		slog.Error("invalid custom metrics settings: --custom-metrics-max-series-per-metric and --custom-metrics-ttl must not be negative")
		os.Exit(1)
	}
	customCfg := metrics.CustomConfig{Enabled: *customMetricsEnabled, MaxSeriesPerMetric: *customMetricsMaxSeries, TTL: *customMetricsTTL}
	if err := srv.ConfigureCustomMetrics(customCfg, pg); err != nil {
		slog.Error("custom metrics init", "error", err)
		os.Exit(1)
	}
	// Runs are terminalized in several places, some inside the store itself;
	// hooking the store covers all of them exactly once.
	pg.SetRunFinishedHook(srv.OnRunFinished)
	srv.SetKeyManager(km)
	// One GitHub App token issuer per process: the AppSource reconciler, the
	// uses: resolver and job token hand-outs share its token cache. The
	// resolver reads credentials through the background pool; job hand-outs
	// touch the store only on a token cache miss.
	gitTokenIssuer := gittoken.NewIssuer(nil, nil)
	gitTokenIssuer.OnRequest = func(credential string, err error) {
		result := "ok"
		if err != nil {
			result = "error"
		}
		m.GitHubAppTokenRequest(credential, result)
	}
	gitCreds := controller.NewGitCredentialResolver(backgroundSt, km, gitTokenIssuer, m)
	srv.SetGitCredentialResolver(gitCreds)
	if obj != nil {
		srv.SetObjectStore(obj)
		srv.SetCacheStore(obj)
		if _, ok := obj.(objectstore.Presigner); !ok {
			slog.Info("object store has no presigning (local filesystem): all agent and sidecar transfers stream through the controller")
		}
	}

	// OIDC configuration (config file > env vars)
	oidcConfigured := false
	if eff.OIDC != nil && eff.OIDC.Issuer != "" && eff.OIDC.ClientID != "" {
		srv.SetOIDCConfig(&controller.OIDCConfig{
			Issuer:             eff.OIDC.Issuer,
			IssuerInternal:     eff.OIDC.IssuerInternal,
			ExternalURL:        eff.OIDC.ExternalURL,
			ClientID:           eff.OIDC.ClientID,
			ClientSecret:       eff.OIDC.ClientSecret,
			DeviceClientID:     eff.OIDC.DeviceClientID,
			DeviceClientSecret: eff.OIDC.DeviceClientSecret,
			AuthParams:         eff.OIDC.AuthParams,
			Scopes:             eff.OIDC.Scopes,
			RolesClaim:         eff.OIDC.RolesClaim,
			RoleMap:            eff.OIDC.RoleMap,
			UserMap:            eff.OIDC.UserMap,
			DefaultRole:        eff.OIDC.DefaultRole,
			Policy:             eff.OIDC.Policy,
		})
		oidcConfigured = true
		slog.Info("OIDC configured", "issuer", eff.OIDC.Issuer, "issuerInternal", eff.OIDC.IssuerInternal, "browserSSO", eff.OIDC.ClientSecret != "")
		if eff.OIDC.IssuerInternal == "" {
			slog.Warn("UNIFIED_OIDC_ISSUER_INTERNAL not set: /dex/* proxy is disabled. CLI device flow will fail. Use docker-compose.sso.yml")
		}
	}
	if eff.OIDC != nil {
		for _, w := range eff.OIDC.PolicyWarnings {
			slog.Warn(w)
		}
		if eff.OIDC.Policy.Configured() && eff.OIDC.ClientID == "" {
			slog.Warn("oidc.permissions is set but oidc.clientId is not; no OIDC caller can authenticate")
		}
	}

	go controller.RunScheduler(ctx, backgroundSt, 200*time.Millisecond)
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := backgroundPG.DeleteExpiredOIDCStates(context.Background()); err != nil {
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
				n, err := backgroundPG.DeleteStaleAgents(ctx, 5*time.Minute)
				if err != nil {
					slog.Warn("deleteStaleAgents", "error", err)
				} else if n > 0 {
					slog.Info("deleteStaleAgents", "deleted", n)
				}
			}
		}
	}()
	if obj != nil {
		go controller.RunLogArchiver(ctx, backgroundSt, obj, 30*time.Second)
		go controller.RunCacheCleanup(ctx, backgroundSt, obj)
		if p, ok := obj.(objectstore.Presigner); ok {
			// Abort multipart uploads abandoned for more than 24h.
			go controller.RunMultipartSweep(ctx, backgroundSt, p, time.Hour)
		}
	}
	go controller.RunApprovalReaper(ctx, backgroundSt, time.Minute)
	// Every replica reports PATs that expire within 7 days (once at startup,
	// then daily); it only reads the database, so no leader is needed.
	go controller.RunPATExpiryCheck(ctx, backgroundSt, m)
	go controller.RunCustomMetricsSweeper(ctx, backgroundSt, m.Custom(), customCfg.TTL)
	go controller.RunStuckRunReaper(ctx, backgroundSt, 30*time.Second, 90*time.Second, 60*time.Second)
	// Fail runs that have sat Queued for longer than the grace period (default
	// 5m, configurable via UNIFIED_QUEUED_RUN_GRACE) with no live agent able to
	// claim them (the agent they need disconnected), so they don't stay
	// "in progress" forever. staleAfter=90s matches the stuck-run reaper's
	// agent-liveness window.
	go controller.RunQueuedRunReaper(ctx, backgroundSt, 30*time.Second, queuedRunGraceDefault(), 90*time.Second)
	// Reap AppSources stuck in "Syncing" (bug #33): the manual sync-trigger API sets
	// sync_status="Syncing" synchronously, so a reconciler crash / restart / leadership
	// change mid-sync can strand the row forever. Reset any Syncing row older than 5m.
	go controller.RunAppSourceSyncReaper(ctx, backgroundSt, 30*time.Second, 5*time.Minute)
	if *auditRetentionDays > 0 {
		slog.Info("audit log retention enabled", "retentionDays", *auditRetentionDays)
	} else {
		slog.Info("audit log retention disabled (keep forever)")
	}
	go controller.RunAuditRetention(ctx, backgroundSt, time.Hour, *auditRetentionDays)
	if *runRetentionDays > 0 {
		slog.Info("run retention enabled", "retentionDays", *runRetentionDays)
	} else {
		slog.Info("run retention disabled (keep forever)")
	}
	go controller.RunRunRetention(ctx, backgroundSt, obj, time.Hour, *runRetentionDays)
	if *logTrimDays > 0 {
		slog.Info("log trim enabled", "trimDays", *logTrimDays)
		if *runRetentionDays > 0 && *logTrimDays >= *runRetentionDays {
			slog.Warn("log trim will never fire: --log-trim-days >= --run-retention-days deletes runs before their logs qualify for trimming",
				"logTrimDays", *logTrimDays, "runRetentionDays", *runRetentionDays)
		}
	} else {
		slog.Info("log trim disabled (DB log rows kept forever)")
	}
	go controller.RunLogTrim(ctx, backgroundSt, obj, time.Hour, *logTrimDays)
	knownHostsFile := gitKnownHostsPath(*gitKnownHostsFlag, *dataDir)
	if err := gittemplate.ValidateKnownHostsFile(knownHostsFile); err != nil {
		slog.Error("invalid SSH known_hosts location", "path", knownHostsFile, "error", err)
		os.Exit(1)
	}
	go func() {
		var gitCache *gittemplate.Cache
		if obj != nil {
			gitCache = gittemplate.NewCache(obj)
		}
		fetcher := newGitFetcher(backgroundSt, knownHostsFile, *gitTimeout, *gitMaxRepoBytes)
		resolver := gittemplate.NewResolver(fetcher, gitCache)
		controller.RunGitResolver(ctx, backgroundSt, resolver, gitCreds, 200*time.Millisecond, gitResolveDeadlineDefault())
	}()
	go func() {
		fetcher := newGitFetcher(backgroundSt, knownHostsFile, *gitTimeout, *gitMaxRepoBytes)
		controller.RunAppSourceReconciler(ctx, backgroundSt, fetcher, gitCreds, 30*time.Second)
	}()

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Router(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	keySource := "file"
	switch {
	case resolved.Ephemeral:
		keySource = "ephemeral"
	case eff.KeySource.KMSURI != "":
		keySource = "kms"
	}

	logStartupSummary(startupInputs{
		ObjectStore:  objectStoreState,
		KeySource:    keySource,
		KeyEphemeral: resolved.Ephemeral,
		OIDC:         oidcConfigured,
		WebUI:        *webDir != "" || *uiProxyTarget != "",
		LogTrimDays:  *logTrimDays,
	})

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		slog.Error("listen", "error", err)
		os.Exit(1)
	}
	slog.Info("controller listening", "addr", *addr)
	// serveUntilShutdown returns only after in-flight requests have drained
	// (or the shutdown timeout elapsed), so the deferred pg.Close(),
	// srv.Close() and key-source Close() below never run under live handlers.
	if err := serveUntilShutdown(ctx, httpSrv, ln, srv.SetShuttingDown, shutdownDrainDelay, shutdownTimeout); err != nil {
		slog.Error("serve", "error", err)
		os.Exit(1)
	}
}

const (
	// shutdownDrainDelay is how long the controller keeps serving after the
	// shutdown signal (health endpoints already 503, long-lived streams
	// ended) so load balancers and clients can move to another replica.
	shutdownDrainDelay = 2 * time.Second
	// shutdownTimeout bounds how long http.Server.Shutdown waits for
	// in-flight requests. drainDelay + timeout must stay well under the
	// pod's terminationGracePeriodSeconds (30 in the Helm chart).
	shutdownTimeout = 10 * time.Second
)

// serveUntilShutdown serves httpSrv on ln until ctx is cancelled, then runs
// the graceful stop sequence: onDrain (flip health checks to 503 and end
// long-lived streams), wait drainDelay, then http.Server.Shutdown bounded by
// shutdownTimeout. It returns only after Shutdown has finished, which is the
// point of the function: Serve returns http.ErrServerClosed as soon as
// Shutdown is called, so a caller that returned on that alone would exit
// while handlers are still running. A Shutdown timeout is logged and the
// remaining connections are force-closed; it is not an error. Only a Serve
// failure other than http.ErrServerClosed is returned.
func serveUntilShutdown(ctx context.Context, httpSrv *http.Server, ln net.Listener, onDrain func(), drainDelay, timeout time.Duration) error {
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(ln) }()

	select {
	case err := <-serveErr:
		// Serve stopped on its own before any signal.
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
	}

	slog.Info("received shutdown signal, draining...")
	if onDrain != nil {
		onDrain()
	}
	time.Sleep(drainDelay)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("graceful shutdown did not finish, forcing close", "error", err)
		_ = httpSrv.Close()
	}
	<-serveErr
	return nil
}

var inClusterKubernetesConfig = rest.InClusterConfig

func buildKubernetesEnrollmentConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig == "" {
		return inClusterKubernetesConfig()
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
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
