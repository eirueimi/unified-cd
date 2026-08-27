package controller

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/eirueimi/unified-cd/internal/metrics"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/secrets"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Config holds the configuration for the master server.
type Config struct {
	Token         string
	ListenAddr    string
	WebDir        string // Directory for static web files. When empty, /ui/* returns 404.
	UIProxyTarget string // URL of the Vite dev server to proxy /ui/* to when WebDir is not set (e.g. http://vite:5173). When empty, /ui/* returns 404.

	// MatrixMaxCombinations caps matrix step expansion; 0 means the default (64).
	MatrixMaxCombinations int

	// WebhookMaxBodyBytes caps the size of an inbound webhook request body;
	// 0 means the default (1 MiB / 1<<20). A body over the limit is rejected
	// with 413 rather than silently truncated — the ingress handler uses the
	// (possibly truncated) body for HMAC/token verification and DSL payload
	// mapping, so truncation would corrupt auth checks and drop fields
	// instead of failing loudly.
	WebhookMaxBodyBytes int64

	// StderrPlain, when true, tells the web UI (via /api/v1/ui-config) to render
	// step stderr in the run log the same color as stdout instead of red.
	StderrPlain bool

	// InsecureCookies disables the Secure attribute on session cookies.
	// Default (false) sets Secure — Chrome/Firefox treat http://localhost as
	// trustworthy so local dev keeps working; opt out only for plain-HTTP
	// deployments (LAN access, Safari-based local dev).
	InsecureCookies               bool
	KubernetesEnrollmentVerifiers map[string]KubernetesEnrollmentVerifier

	// StoreCredentialClusters lists the Kubernetes clusters (and, per
	// cluster, which namespaces/ServiceAccounts) trusted to broker
	// object-store credentials to job Pod sidecars — see
	// api_store_credentials.go for why this is server configuration rather
	// than a database-backed policy like KubernetesEnrollmentVerifiers'
	// namespace/ServiceAccount constraints are. nil/empty disables the
	// broker entirely: POST /api/v1/store-credentials then always reports
	// "kubernetes identity unavailable", the same failure shape enrollment
	// uses when no verifier is configured for a requested cluster.
	StoreCredentialClusters []StoreCredentialCluster

	// StoreCredentialS3 is the credential the broker hands out — the
	// controller's OWN object-store configuration, passed through
	// unscoped. nil means the controller has no object store configured,
	// which the broker reports as a clear, named error rather than an
	// empty credential the sidecar would only fail to sign with later.
	StoreCredentialS3 *objectstore.S3Config
}

// OIDCConfig holds the OIDC provider configuration.
type OIDCConfig struct {
	Issuer         string
	IssuerInternal string // URL for in-container discovery (defaults to Issuer when omitted).
	ExternalURL    string // Base URL for browser redirect URIs (e.g. http://localhost:8080). Uses r.Host when not set.
	ClientID       string
	ClientSecret   string
	DeviceClientID string // Public client ID for the CLI device flow (defaults to ClientID when omitted).

	// Role resolution (mirrors config.ControllerOIDCConfig).
	RolesClaim  string
	RoleMap     map[string]string
	UserMap     map[string]string
	DefaultRole string
}

// Server represents the master HTTP server.
type Server struct {
	cfg                           Config
	store                         store.Store
	r                             chi.Router
	shuttingDown                  atomic.Bool
	claimDrainCh                  chan struct{}           // Closed on shutdown to immediately drain all claim long-polls.
	objStore                      objectstore.ObjectStore // Archive endpoints return 501 when nil.
	archLogs                      *archivedLogs           // Serves log reads for trimmed runs; nil when objStore is nil.
	cacheStore                    objectstore.ObjectStore // nil = skip TTL cleanup
	km                            secrets.KeyManager      // Secret API returns 501 when nil.
	oidcCfg                       *OIDCConfig             // OIDC endpoints return 404 when nil.
	dexProxy                      *httputil.ReverseProxy  // /dex/* returns 404 when nil.
	uiProxy                       *httputil.ReverseProxy  // /ui/* returns 404 when nil (when WebDir is not set).
	metrics                       *metrics.Metrics        // nil = middleware no-ops and /metrics returns 404.
	claimedBy                     *claimedByCache         // Immutable claimed_by ownership cache (always initialized).
	enrollmentLimiter             *enrollmentLimiter
	credentialTouches             *credentialTouchLimiter
	kubernetesEnrollmentVerifiers map[string]KubernetesEnrollmentVerifier
	storeCredentialClusters       []StoreCredentialCluster
	storeCredentialS3             *objectstore.S3Config

	// Cached provider for OIDC Bearer token verification (lazily initialized).
	// Used to verify id_tokens obtained via the CLI device flow for API authentication.
	oidcVerifyOnce   sync.Once
	oidcProviderV    *oidc.Provider
	oidcProviderVErr error

	// logNotify fans out live "this run's logs changed" wake-ups to SSE
	// viewers in-process (see log_notify.go); logNotifyOnce starts the ONE
	// shared Postgres LISTEN connection that feeds it on the first SSE
	// viewer this Server ever serves, not in NewServer and not at process
	// startup — see subscribeLogNotify for why lazy start specifically.
	// logNotifyCtx/logNotifyCancel bound that goroutine's lifetime; Close
	// cancels it.
	logNotify       *logNotifyHub
	logNotifyOnce   sync.Once
	logNotifyCtx    context.Context
	logNotifyCancel context.CancelFunc
}

// NewServer creates a new server from the given config and store and sets up routing.
func NewServer(cfg Config, st store.Store) *Server {
	s := &Server{cfg: cfg, store: st, r: chi.NewRouter(), claimDrainCh: make(chan struct{}), claimedBy: newClaimedByCache(claimedByCacheCap), enrollmentLimiter: newEnrollmentLimiter(nil), credentialTouches: newCredentialTouchLimiter(nil), kubernetesEnrollmentVerifiers: cfg.KubernetesEnrollmentVerifiers, storeCredentialClusters: cfg.StoreCredentialClusters, storeCredentialS3: cfg.StoreCredentialS3}
	// logNotify itself is cheap to construct (an empty map + a mutex); it is
	// the listener goroutine that is expensive (a listenPool connection),
	// and that stays unstarted until subscribeLogNotify's first call — see
	// its doc comment for why eager start here would be wrong.
	s.logNotify = newLogNotifyHub()
	s.logNotifyCtx, s.logNotifyCancel = context.WithCancel(context.Background())
	if cfg.WebDir == "" && cfg.UIProxyTarget != "" {
		if target, err := url.Parse(cfg.UIProxyTarget); err == nil {
			s.uiProxy = &httputil.ReverseProxy{
				Director: func(req *http.Request) {
					req.URL.Scheme = target.Scheme
					req.URL.Host = target.Host
					// Do not rewrite the Host header. Vite's server.allowedHosts check
					// inspects the Host header, so rewriting it to the Docker service name
					// (e.g. "vite") would cause a "Blocked request" rejection. Forwarding
					// the original Host sent by the browser (e.g. localhost:8080) keeps it
					// within the default allowedHosts allowlist (localhost, etc.).
				},
			}
		}
	}
	s.routes()
	return s
}

// SetShuttingDown marks the server as shutting down.
// After this call, /healthz returns 503.
// Closes claimDrainCh to broadcast to all waiting claim handlers.
// CompareAndSwap prevents a panic from a double close.
func (s *Server) SetShuttingDown() {
	if s.shuttingDown.CompareAndSwap(false, true) {
		close(s.claimDrainCh)
	}
}

// subscribeLogNotify registers the caller's interest in runID's live log
// wake-ups, starting the shared listener goroutine (runLogNotifyListener)
// on the very first call across every run and every request this Server
// ever handles — not in NewServer, and not at process startup.
//
// Lazy start matters for two independent reasons: (1) most of this
// package's NewServer(...) call sites are in tests that pass a nil store
// or a fake that does not implement real Postgres NOTIFY — starting the
// listener eagerly would call ListenForNotify on every one of them,
// immediately, for no reason those tests care about. (2) a controller
// replica that never serves an SSE request should not spend a listenPool
// connection it will never use. A Server that DOES serve SSE pays the
// cost exactly once, on its first viewer, no matter how many viewers or
// runs follow.
func (s *Server) subscribeLogNotify(runID string) (<-chan struct{}, func()) {
	s.logNotifyOnce.Do(func() {
		go runLogNotifyListener(s.logNotifyCtx, s.store, s.logNotify)
	})
	return s.logNotify.subscribe(runID)
}

// Close releases resources Server owns outright — currently just the
// shared log-notify listener goroutine started lazily by
// subscribeLogNotify. Safe to call even if no SSE viewer ever connected
// (logNotifyOnce never fired, so there is nothing to stop) and safe to
// call more than once: context.CancelFunc is documented idempotent.
//
// Also safe on a Server built by struct literal rather than NewServer —
// several tests in this package do exactly that (see hardening_test.go),
// and a Close on one of those should be a no-op, not a nil-func panic.
func (s *Server) Close() {
	if s.logNotifyCancel != nil {
		s.logNotifyCancel()
	}
}

// SetObjectStore sets the object store used for log archiving. Archive endpoints return 501 when nil.
func (s *Server) SetObjectStore(obj objectstore.ObjectStore) {
	s.objStore = obj
	if obj != nil {
		s.archLogs = newArchivedLogs(obj)
	} else {
		s.archLogs = nil
	}
}

// SetCacheStore sets the object store used for cache TTL cleanup.
func (s *Server) SetCacheStore(cs objectstore.ObjectStore) {
	s.cacheStore = cs
}

// SetKeyManager sets the encryption key manager. The secret API returns 501 when nil.
func (s *Server) SetKeyManager(km secrets.KeyManager) {
	s.km = km
}

// SetMetrics enables the /metrics endpoint and HTTP request instrumentation.
func (s *Server) SetMetrics(m *metrics.Metrics) { s.metrics = m }

// SetOIDCConfig configures the OIDC provider settings. OIDC endpoints return 404 when nil.
// When IssuerInternal is set, initializes a reverse proxy that forwards /dex/* to IssuerInternal.
func (s *Server) SetOIDCConfig(cfg *OIDCConfig) {
	s.oidcCfg = cfg
	if cfg == nil || cfg.IssuerInternal == "" {
		return
	}
	target, err := url.Parse(cfg.IssuerInternal)
	if err != nil {
		return
	}
	s.dexProxy = &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			// Dex uses the issuer path (http://localhost:8080/dex) as a prefix for all routes
			// (via path.Join(issuerURL.Path, route)), so /.well-known/openid-configuration,
			// /token, and /device/code are all served under /dex/.
			// Therefore only the scheme and host are redirected to the internal Dex; the path is left unchanged.
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
		},
	}
}

// accessLogMiddleware emits a single-line JSON access log after each request completes.
func accessLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		slog.Info("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"remoteAddr", r.RemoteAddr,
		)
	})
}

// metricsMiddleware records request count and duration per chi route
// pattern (never the raw path, to keep label cardinality bounded).
// No-op until SetMetrics is called.
//
// The route pattern is resolved via a standalone s.r.Match lookup rather
// than by reading chi.RouteContext(r.Context()).RoutePattern() after next
// runs: when a route lives inside a mounted subrouter (e.g. /api/v1) whose
// own middleware (auth) short-circuits before reaching that subrouter's
// routeHTTP, chi never appends the leaf pattern segment to the request's
// route context, and RoutePattern() would only report the parent mount's
// wildcard (e.g. "/api/v1/*") instead of "/api/v1/runs/{id}". Mux.Match
// walks the full routing tree structurally, without executing any
// middleware or handlers, so it always yields the true leaf pattern.
func (s *Server) metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.metrics == nil {
			next.ServeHTTP(w, r)
			return
		}
		route := "unmatched"
		if rctx := chi.NewRouteContext(); s.r.Match(rctx, r.Method, r.URL.Path) {
			route = rctx.RoutePattern()
		}
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		code := ww.Status()
		if code == 0 {
			code = http.StatusOK
		}
		s.metrics.HTTPRequest(r.Method, route, code, time.Since(start).Seconds())
	})
}

type agentRouteAuthMode uint8

const (
	agentRouteAuth agentRouteAuthMode = iota
	agentRouteOrServerAuth
)

type agentIdentityRoute struct {
	method       string
	path         string
	auth         agentRouteAuthMode
	bindPath     bool
	requiredRole string
	handler      func(*Server, http.ResponseWriter, *http.Request)
}

// agentRouteIdentityMatrix is both the registration source and the
// impersonation-test matrix.
var agentRouteIdentityMatrix = []agentIdentityRoute{
	{method: http.MethodGet, path: "/api/v1/agents/{agentId}", auth: agentRouteOrServerAuth, bindPath: true, requiredRole: "viewer", handler: (*Server).handleGetAgent},
	{method: http.MethodGet, path: "/api/v1/agents/{agentId}/runs", auth: agentRouteOrServerAuth, bindPath: true, requiredRole: "viewer", handler: (*Server).handleListRunsByAgent},
	{method: http.MethodPost, path: "/api/v1/agents/{agentId}/heartbeat", bindPath: true, handler: (*Server).handleAgentHeartbeat},
	{method: http.MethodDelete, path: "/api/v1/agents/{agentId}", bindPath: true, handler: (*Server).handleAgentDeregister},
	{method: http.MethodPost, path: "/api/v1/agents/{agentId}/claim", bindPath: true, handler: (*Server).handleAgentClaim},
	{method: http.MethodPost, path: "/api/v1/agents/{agentId}/steps", bindPath: true, handler: (*Server).handleAgentStepReport},
	{method: http.MethodPost, path: "/api/v1/agents/{agentId}/logs", bindPath: true, handler: (*Server).handleAgentLogAppend},
	{method: http.MethodPost, path: "/api/v1/agents/{agentId}/runs/reconcile", bindPath: true, handler: (*Server).handleAgentReconcileRuns},
	{method: http.MethodPost, path: "/api/v1/agents/{agentId}/runs/{runId}/finish", bindPath: true, handler: (*Server).handleAgentFinishRun},
	{method: http.MethodPost, path: "/api/v1/agents/{agentId}/runs/{runId}/children", bindPath: true, handler: (*Server).handleAgentCreateChildRun},
	{method: http.MethodPost, path: "/api/v1/agents/{agentId}/runs/{runId}/steps/{stepIndex}/outputs", bindPath: true, handler: (*Server).handleAgentSetStepOutputs},
	{method: http.MethodPost, path: "/api/v1/agents/{agentId}/runs/{runId}/outputs", bindPath: true, handler: (*Server).handleAgentSetRunOutputs},
	{method: http.MethodPost, path: "/api/v1/agents/{agentId}/runs/{runId}/steps/{stepIndex}/logs/bulk", bindPath: true, handler: (*Server).handleAgentLogBulk},
	{method: http.MethodPost, path: "/api/v1/agents/{agentId}/runs/{runId}/sidecars", bindPath: true, handler: (*Server).handleAgentSidecarStatus},
	{method: http.MethodPost, path: "/api/v1/agents/{agentId}/secrets/fetch", bindPath: true, handler: (*Server).handleAgentSecretsFetch},
	{method: http.MethodPost, path: "/api/v1/agents/{agentId}/runs/{runId}/approvals", bindPath: true, handler: (*Server).handleAgentCreateApproval},
	{method: http.MethodGet, path: "/api/v1/agents/{agentId}/runs/{runId}/approvals/{stepIndex}", bindPath: true, handler: (*Server).handleAgentGetApproval},
	{method: http.MethodPut, path: "/api/v1/runs/{runID}/artifacts/{name}", handler: (*Server).handleArtifactUpload},
}

func (s *Server) registerAgentIdentityRoutes() {
	for _, route := range agentRouteIdentityMatrix {
		route := route
		r := s.r
		if route.auth == agentRouteOrServerAuth {
			r = r.With(s.agentOrServerAuth)
		} else {
			r = r.With(s.agentAuth)
		}
		if route.bindPath {
			r = r.With(s.requireAgentPathIdentity)
		}
		if route.requiredRole != "" {
			r = r.With(requireMinRole(route.requiredRole))
		}
		r.Method(route.method, route.path, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			route.handler(s, w, req)
		}))
	}
}

func (s *Server) routes() {
	s.r.Use(middleware.Recoverer)
	s.r.Use(middleware.RealIP)
	s.r.Use(accessLogMiddleware)
	s.r.Use(s.metricsMiddleware)
	s.r.Use(securityHeadersMiddleware)
	// Router-wide (not just /api/v1): the auth POST routes (e.g.
	// /api/v1/auth/logout) are registered directly on s.r outside the
	// /api/v1 group, and non-browser clients pass through anyway.
	s.r.Use(s.originCheckMiddleware)

	// Health-check endpoint (no auth required).
	// Returns 503 while shutting down so the load balancer can drain traffic.
	s.r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if s.shuttingDown.Load() {
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Prometheus metrics (no auth by design — block at the LB / firewall
	// when the controller is internet-facing). 404 until SetMetrics.
	s.r.Get("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if s.metrics == nil {
			http.NotFound(w, r)
			return
		}
		s.metrics.Handler().ServeHTTP(w, r)
	})

	// Readiness-check endpoint (no auth required).
	// Returns 503 while shutting down and also checks DB connectivity.
	s.r.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if s.shuttingDown.Load() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		if s.store != nil {
			pingCtx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			if err := s.store.Ping(pingCtx); err != nil {
				http.Error(w, "db unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})

	// The SSE endpoint is registered individually outside the /api/v1 route block.
	s.r.With(ServerAuth(s.store, s), requireMinRole("viewer")).
		Get("/api/v1/runs/{id}/events", s.handleRunEvents)

	// Run + outputs reads are also reachable by an enrolled agent (the call:
	// step polls the child run it created), so they use agentOrServerAuth like
	// the artifact routes rather than the human-only /api/v1 group. viewerOrAgent
	// keeps the human viewer floor while letting agent principals through.
	//
	// /runs/active is registered here too (auth unchanged: ServerAuth + viewer,
	// no agent access) purely so it stays a sibling of /runs/{id} in the SAME
	// chi tree. chi prefers a static match ("active") over a param match
	// ({id}) only when both live in the same routing tree; the /api/v1 group
	// below is mounted as its own sub-router, so once /runs/{id} is registered
	// directly on the top-level tree it would otherwise swallow "active" as an
	// id before the request ever reaches the mounted group.
	s.r.With(ServerAuth(s.store, s), requireMinRole("viewer")).Get("/api/v1/runs/active", s.handleListActiveRuns)
	s.r.With(s.agentOrServerAuth, s.viewerOrAgent).Get("/api/v1/runs/{id}", s.handleGetRun)
	s.r.With(s.agentOrServerAuth, s.viewerOrAgent).Get("/api/v1/runs/{id}/outputs", s.handleGetRunOutputs)

	s.r.Route("/api/v1", func(r chi.Router) {
		r.Use(ServerAuth(s.store, s))
		r.Use(auditLogMiddleware(s.store))

		dev := requireMinRole("developer")
		view := requireMinRole("viewer")
		admin := requireMinRole("admin")

		r.With(admin).Get("/audit", s.handleListAuditLogs)

		r.With(dev).Post("/jobs", s.handleApplyJob)
		r.With(view).Get("/jobs", s.handleListJobs)
		r.With(view).Get("/jobs/*", s.handleGetJobOrYAML)
		r.With(dev).Delete("/jobs/*", s.handleDeleteJob)

		r.With(dev).Post("/runs", s.handleTriggerRun)
		r.With(dev).Post("/runs/{id}/replay", s.handleReplayRun)
		r.With(view).Get("/runs", s.handleListRunsByJob)
		r.With(view).Get("/runs/{id}/yaml", s.handleGetRunYAML)
		r.With(dev).Post("/runs/{id}/cancel", s.handleCancelRun)
		r.With(dev).Delete("/runs/{id}", s.handleDeleteRun)
		r.With(view).Get("/runs/{id}/logs", s.handleTailLogs)
		r.With(view).Get("/runs/{id}/steps", s.handleGetRunSteps)
		r.With(view).Get("/runs/{id}/logs/archive", s.handleLogsArchive)
		r.With(view).Get("/runs/{id}/logs/stats", s.handleLogStats)
		r.With(view).Get("/runs/{id}/logs/range", s.handleLogRange)
		r.With(view).Get("/runs/{id}/logs/search", s.handleLogSearch)
		r.With(view).Get("/runs/{runID}/approvals", s.handleListRunApprovals)
		r.With(dev).Post("/runs/{runID}/approvals/{stepIndex}", s.handleDecideApproval)

		r.Route("/secrets", func(r chi.Router) {
			r.With(admin).Post("/", s.handleSetSecret)
			r.With(dev).Get("/", s.handleListSecrets) // names only
			r.With(admin).Delete("/{name}", s.handleDeleteSecret)
		})
		r.Route("/gitcredentials", func(r chi.Router) {
			r.Use(admin)
			r.Post("/", s.handleUpsertGitCredential)
			r.Get("/", s.handleListGitCredentials)
			r.Delete("/{name}", s.handleDeleteGitCredential)
		})
		r.With(dev).Post("/tokens", s.handleCreateToken)
		r.With(dev).Get("/tokens", s.handleListTokens)
		r.With(dev).Delete("/tokens/{id}", s.handleDeleteToken)

		r.With(admin).Post("/agent-enrollments", s.handleCreateAgentEnrollment)
		r.With(view).Get("/agent-enrollments", s.handleListAgentEnrollments)
		r.With(admin).Delete("/agent-enrollments/{id}", s.handleRevokeAgentEnrollment)
		r.With(admin).Post("/agent-enrollment-policies", s.handleCreateAgentEnrollmentPolicy)
		r.With(view).Get("/agent-enrollment-policies", s.handleListAgentEnrollmentPolicies)
		r.With(view).Get("/agent-enrollment-policies/{name}", s.handleGetAgentEnrollmentPolicy)
		r.With(admin).Put("/agent-enrollment-policies/{name}", s.handleUpdateAgentEnrollmentPolicy)
		r.With(admin).Delete("/agent-enrollment-policies/{name}", s.handleDeleteAgentEnrollmentPolicy)
		r.With(view).Get("/agent-identities/{agentId}", s.handleGetAgentIdentity)
		r.With(admin).Post("/agent-identities/{agentId}/enable", s.handleEnableAgentIdentity)
		r.With(admin).Post("/agent-identities/{agentId}/disable", s.handleDisableAgentIdentity)
		r.With(admin).Post("/agent-identities/{agentId}/credentials/revoke", s.handleRevokeAgentCredentials)
	})

	// WebhookReceiver management (auth required)
	s.r.Route("/api/v1/webhooks", func(r chi.Router) {
		r.Use(ServerAuth(s.store, s))
		r.Use(auditLogMiddleware(s.store))
		r.With(requireMinRole("admin")).Post("/", s.handleApplyWebhook)
		r.With(requireMinRole("viewer")).Get("/", s.handleListWebhooks)
		r.With(requireMinRole("admin")).Delete("/{name}", s.handleDeleteWebhook)
	})

	// Vars management (auth required)
	s.r.Route("/api/v1/vars", func(r chi.Router) {
		r.Use(ServerAuth(s.store, s))
		r.Use(auditLogMiddleware(s.store))
		r.With(requireMinRole("admin")).Post("/", s.handleApplyVars)
		r.With(requireMinRole("viewer")).Get("/", s.handleListVars)
		r.With(requireMinRole("admin")).Delete("/{name}", s.handleDeleteVars)
	})

	// Schedule management (auth required)
	s.r.Route("/api/v1/schedules", func(r chi.Router) {
		r.Use(ServerAuth(s.store, s))
		r.Use(auditLogMiddleware(s.store))
		r.With(requireMinRole("developer")).Post("/", s.handleApplySchedule)
		r.With(requireMinRole("viewer")).Get("/", s.handleListSchedules)
		r.With(requireMinRole("developer")).Delete("/{name}", s.handleDeleteSchedule)
	})

	// AppSource management (auth required)
	s.r.Route("/api/v1/appsources", func(r chi.Router) {
		r.Use(ServerAuth(s.store, s))
		r.Use(auditLogMiddleware(s.store))
		r.With(requireMinRole("admin")).Post("/", s.handleApplyAppSource)
		r.With(requireMinRole("viewer")).Get("/", s.handleListAppSources)
		r.With(requireMinRole("viewer")).Get("/{name}", s.handleGetAppSource)
		r.With(requireMinRole("admin")).Delete("/{name}", s.handleDeleteAppSource)
		r.With(requireMinRole("admin")).Post("/{name}/sync", s.handleSyncAppSource)
	})

	// Webhook payload ingress (no per-route auth; authenticated via signature verification)
	s.r.Post("/webhook/{name}", s.handleWebhookIngress)

	// Dex reverse proxy (active only when IssuerInternal is set).
	// Dex uses the issuer path (/dex) as a prefix for all routes, so /dex/* is forwarded to Dex as-is.
	s.r.HandleFunc("/dex", func(w http.ResponseWriter, r *http.Request) {
		if s.dexProxy == nil {
			http.NotFound(w, r)
			return
		}
		s.dexProxy.ServeHTTP(w, r)
	})
	s.r.HandleFunc("/dex/*", func(w http.ResponseWriter, r *http.Request) {
		if s.dexProxy == nil {
			http.NotFound(w, r)
			return
		}
		s.dexProxy.ServeHTTP(w, r)
	})
	// After device-flow approval, Dex redirects the browser to the bare path "/device/callback"
	// (without the issuer path — hardcoded in deviceflowhandlers.go).
	// Proxy this path to Dex's actual route /dex/device/callback.
	s.r.HandleFunc("/device/callback", func(w http.ResponseWriter, r *http.Request) {
		if s.dexProxy == nil {
			http.NotFound(w, r)
			return
		}
		r.URL.Path = "/dex" + r.URL.Path
		s.dexProxy.ServeHTTP(w, r)
	})

	// OIDC configuration endpoint (no auth required — public)
	s.r.Get("/api/v1/auth/oidc-config", s.handleOIDCConfig)

	// UI configuration endpoint (no auth required — public): server-set display
	// preferences the web UI reads at startup.
	s.r.Get("/api/v1/ui-config", s.handleUIConfig)

	// OIDC browser SSO endpoints (no auth required)
	s.r.Get("/api/v1/auth/oidc-login", s.handleOIDCLogin)
	s.r.Get("/api/v1/auth/oidc-callback", s.handleOIDCCallback)
	s.r.Post("/api/v1/auth/logout", s.handleLogout)
	s.r.Get("/api/v1/auth/me", s.handleMe)

	s.r.Route("/api/v1/agents", func(r chi.Router) {
		// Bootstrap and refresh are intentionally outside ServerAuth: their opaque
		// agent credentials are authenticated by the handlers themselves.
		r.Post("/enroll", s.handleAgentEnroll)
		r.Post("/token/refresh", s.handleAgentRefresh)
		r.With(ServerAuth(s.store, s), requireMinRole("viewer")).Get("/", s.handleListAgents)
		r.With(s.agentAuth).Post("/register", s.handleAgentRegister)
	})

	s.r.Route("/api/v1/runs/{runID}/artifacts", func(r chi.Router) {
		r.With(s.agentOrServerAuth).Get("/{name}", s.handleArtifactDownload)
		r.With(s.agentOrServerAuth).Get("/", s.handleArtifactList)
	})

	// store-credentials is deliberately OUTSIDE ServerAuth/agentAuth/the
	// /api/v1/agents group above, for the same reason /enroll is: the
	// projected ServiceAccount token IN THE REQUEST BODY is the credential,
	// checked by handleStoreCredentials itself via a TokenReview. Neither an
	// agent bearer token nor a human session cookie is an appropriate gate
	// here — a job Pod's sidecar has neither. Applying agentOrServerAuth (or
	// any of this file's other middleware) would require the sidecar to
	// already hold one of THOSE credentials before it could ask for this
	// one, which defeats the point: this endpoint exists so a Pod that holds
	// nothing but its own projected token can still get store credentials.
	s.r.Post("/api/v1/store-credentials", s.handleStoreCredentials)

	s.registerAgentIdentityRoutes()

	// When WebDir is set, serve the Web UI as static files (no auth required).
	// When WebDir is not set but UIProxyTarget is set, reverse-proxy any request that
	// did not match an API route above (e.g. /ui/* or assets requested by Vite itself)
	// to the Vite dev server via chi's NotFound fallback.
	// ("/ui/" is a relative path on the controller's own origin after OIDC SSO login
	// completes; without this, a different-origin Vite in development would return 404.
	// /api, /dex, /webhook, and /healthz are explicitly registered above and will not
	// fall through to NotFound, so they are never forwarded to Vite.)
	// When neither is set, /ui/* returns 404.
	switch {
	case s.cfg.WebDir != "":
		uiFS := http.StripPrefix("/ui", http.FileServer(http.Dir(s.cfg.WebDir)))
		s.r.Handle("/ui", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/ui/", http.StatusMovedPermanently)
		}))
		s.r.Handle("/ui/*", uiFS)
		s.r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/ui/", http.StatusFound)
		})
	case s.uiProxy != nil:
		s.r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/ui/", http.StatusFound)
		})
		s.r.NotFound(func(w http.ResponseWriter, r *http.Request) {
			s.uiProxy.ServeHTTP(w, r)
		})
	}
}

// Router returns the HTTP handler.
func (s *Server) Router() http.Handler { return s.r }
