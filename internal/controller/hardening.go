package controller

import (
	"net/http"
	"net/url"
	"strings"
)

// securityHeadersMiddleware adds defense-in-depth headers to every response.
// CSP and HSTS are deliberately absent: CSP needs a pass over the Vite dev
// setup (HMR, inline styles) first, and HSTS belongs to the TLS-terminating
// proxy.
func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// originCheckMiddleware rejects cross-origin state-changing requests — CSRF
// defense-in-depth on top of the session cookie's SameSite=Lax. Browsers
// always attach an Origin header to non-GET requests, so a request carrying
// neither Origin nor Referer is a non-browser client (CLI, agent, webhook)
// and passes through. The scheme is deliberately not compared: a
// TLS-terminating proxy makes it unreliable; the host (including port) is.
func (s *Server) originCheckMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		ref := r.Header.Get("Origin")
		if ref == "" {
			ref = r.Header.Get("Referer")
		}
		if ref == "" {
			next.ServeHTTP(w, r)
			return
		}
		if u, err := url.Parse(ref); err == nil && u.Host != "" && s.allowedBrowserHost(r, u.Host) {
			next.ServeHTTP(w, r)
			return
		}
		// Covers mismatches and non-URL values like "null" (sandboxed iframes).
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
	})
}

// allowedBrowserHost reports whether host (host[:port]) matches the request's
// own Host or the configured OIDC ExternalURL's host.
func (s *Server) allowedBrowserHost(r *http.Request, host string) bool {
	if strings.EqualFold(host, r.Host) {
		return true
	}
	if s.oidcCfg != nil && s.oidcCfg.ExternalURL != "" {
		if u, err := url.Parse(s.oidcCfg.ExternalURL); err == nil && u.Host != "" && strings.EqualFold(host, u.Host) {
			return true
		}
	}
	return false
}
