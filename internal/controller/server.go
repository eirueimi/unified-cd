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
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/secrets"
	"github.com/eirueimi/unified-cd/internal/store"
)

// Config holds the configuration for the master server.
type Config struct {
	Token         string
	AgentToken    string
	ListenAddr    string
	WebDir        string // Directory for static web files. When empty, /ui/* returns 404.
	UIProxyTarget string // URL of the Vite dev server to proxy /ui/* to when WebDir is not set (e.g. http://vite:5173). When empty, /ui/* returns 404.
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
	cfg          Config
	store        store.Store
	r            chi.Router
	shuttingDown atomic.Bool
	claimDrainCh chan struct{}           // Closed on shutdown to immediately drain all claim long-polls.
	objStore     objectstore.ObjectStore // Archive endpoints return 501 when nil.
	cacheStore   objectstore.ObjectStore // nil = skip TTL cleanup
	km           secrets.KeyManager      // Secret API returns 501 when nil.
	oidcCfg      *OIDCConfig             // OIDC endpoints return 404 when nil.
	dexProxy     *httputil.ReverseProxy  // /dex/* returns 404 when nil.
	uiProxy      *httputil.ReverseProxy  // /ui/* returns 404 when nil (when WebDir is not set).

	// Cached provider for OIDC Bearer token verification (lazily initialized).
	// Used to verify id_tokens obtained via the CLI device flow for API authentication.
	oidcVerifyOnce   sync.Once
	oidcProviderV    *oidc.Provider
	oidcProviderVErr error
}

// NewServer creates a new server from the given config and store and sets up routing.
func NewServer(cfg Config, st store.Store) *Server {
	if cfg.AgentToken == "" {
		cfg.AgentToken = cfg.Token
	}
	s := &Server{cfg: cfg, store: st, r: chi.NewRouter(), claimDrainCh: make(chan struct{})}
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

// SetObjectStore sets the object store used for log archiving. Archive endpoints return 501 when nil.
func (s *Server) SetObjectStore(obj objectstore.ObjectStore) {
	s.objStore = obj
}

// SetCacheStore sets the object store used for cache TTL cleanup.
func (s *Server) SetCacheStore(cs objectstore.ObjectStore) {
	s.cacheStore = cs
}

// SetKeyManager sets the encryption key manager. The secret API returns 501 when nil.
func (s *Server) SetKeyManager(km secrets.KeyManager) {
	s.km = km
}

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

func (s *Server) routes() {
	s.r.Use(middleware.Recoverer)
	s.r.Use(middleware.RealIP)
	s.r.Use(accessLogMiddleware)

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

	s.r.Route("/api/v1", func(r chi.Router) {
		r.Use(ServerAuth(s.store, s))
		r.Use(auditLogMiddleware(s.store))

		dev := requireMinRole("developer")
		view := requireMinRole("viewer")
		admin := requireMinRole("admin")

		r.With(admin).Get("/audit", s.handleListAuditLogs)

		r.With(dev).Post("/jobs", s.handleApplyJob)
		r.With(view).Get("/jobs", s.handleListJobs)
		r.With(view).Get("/jobs/{name}", s.handleGetJob)
		r.With(view).Get("/jobs/{name}/yaml", s.handleGetJobYAML)
		r.With(dev).Delete("/jobs/{name}", s.handleDeleteJob)

		r.With(dev).Post("/runs", s.handleTriggerRun)
		r.With(view).Get("/runs/active", s.handleListActiveRuns)
		r.With(view).Get("/runs", s.handleListRunsByJob)
		r.With(view).Get("/runs/{id}", s.handleGetRun)
		r.With(view).Get("/runs/{id}/yaml", s.handleGetRunYAML)
		r.With(dev).Post("/runs/{id}/cancel", s.handleCancelRun)
		r.With(dev).Delete("/runs/{id}", s.handleDeleteRun)
		r.With(view).Get("/runs/{id}/logs", s.handleTailLogs)
		r.With(view).Get("/runs/{id}/steps", s.handleGetRunSteps)
		r.With(view).Get("/runs/{id}/outputs", s.handleGetRunOutputs)
		r.With(view).Get("/runs/{id}/logs/archive", s.handleLogsArchive)
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
	})

	// WebhookReceiver management (auth required)
	s.r.Route("/api/v1/webhooks", func(r chi.Router) {
		r.Use(ServerAuth(s.store, s))
		r.Use(auditLogMiddleware(s.store))
		r.With(requireMinRole("admin")).Post("/", s.handleApplyWebhook)
		r.With(requireMinRole("viewer")).Get("/", s.handleListWebhooks)
		r.With(requireMinRole("admin")).Delete("/{name}", s.handleDeleteWebhook)
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

	// OIDC browser SSO endpoints (no auth required)
	s.r.Get("/api/v1/auth/oidc-login", s.handleOIDCLogin)
	s.r.Get("/api/v1/auth/oidc-callback", s.handleOIDCCallback)
	s.r.Post("/api/v1/auth/logout", s.handleLogout)
	s.r.Get("/api/v1/auth/me", s.handleMe)

	s.r.Route("/api/v1/agents", func(r chi.Router) {
		// GET uses ServerAuth + requireMinRole("viewer"); all other methods use BearerAuth (agent token).
		r.With(ServerAuth(s.store, s), requireMinRole("viewer")).Get("/", s.handleListAgents)
		r.With(ServerAuth(s.store, s), requireMinRole("viewer")).Get("/{agentId}", s.handleGetAgent)
		r.With(ServerAuth(s.store, s), requireMinRole("viewer")).Get("/{agentId}/runs", s.handleListRunsByAgent)
		r.With(BearerAuth(s.cfg.AgentToken)).Post("/register", s.handleAgentRegister)
		r.With(BearerAuth(s.cfg.AgentToken)).Post("/{agentId}/heartbeat", s.handleAgentHeartbeat)
		r.With(BearerAuth(s.cfg.AgentToken)).Delete("/{agentId}", s.handleAgentDeregister)
		r.With(BearerAuth(s.cfg.AgentToken)).Post("/{agentId}/claim", s.handleAgentClaim)
		r.With(BearerAuth(s.cfg.AgentToken)).Post("/{agentId}/steps", s.handleAgentStepReport)
		r.With(BearerAuth(s.cfg.AgentToken)).Post("/{agentId}/logs", s.handleAgentLogAppend)
		r.With(BearerAuth(s.cfg.AgentToken)).Post("/{agentId}/runs/{runId}/finish", s.handleAgentFinishRun)
		r.With(BearerAuth(s.cfg.AgentToken)).Post("/{agentId}/runs/{runId}/steps/{stepIndex}/outputs", s.handleAgentSetStepOutputs)
		r.With(BearerAuth(s.cfg.AgentToken)).Post("/{agentId}/runs/{runId}/outputs", s.handleAgentSetRunOutputs)
		r.With(BearerAuth(s.cfg.AgentToken)).Post("/{agentId}/runs/{runId}/steps/{stepIndex}/logs/bulk", s.handleAgentLogBulk)
		r.With(BearerAuth(s.cfg.AgentToken)).Post("/{agentId}/secrets/fetch", s.handleAgentSecretsFetch)
		r.With(BearerAuth(s.cfg.AgentToken)).Post("/{agentId}/runs/{runId}/approvals", s.handleAgentCreateApproval)
		r.With(BearerAuth(s.cfg.AgentToken)).Get("/{agentId}/runs/{runId}/approvals/{stepIndex}", s.handleAgentGetApproval)
	})

	s.r.Route("/api/v1/runs/{runID}/artifacts", func(r chi.Router) {
		r.With(BearerAuth(s.cfg.AgentToken)).Put("/{name}", s.handleArtifactUpload)
		r.With(AgentOrServerAuth(s.cfg.AgentToken, s.store, s)).Get("/{name}", s.handleArtifactDownload)
		r.With(AgentOrServerAuth(s.cfg.AgentToken, s.store, s)).Get("/", s.handleArtifactList)
	})

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
