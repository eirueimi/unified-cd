package controller

import (
	"encoding/json"
	"net/http"
)

// handleUIConfig returns server-set display preferences the web UI reads at
// startup (via /api/v1/ui-config). It is public (no auth) and exposes only
// non-sensitive display flags.
func (s *Server) handleUIConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"stderrPlain":     s.cfg.StderrPlain,
		"allowTokenEntry": s.allowBrowserTokenEntry(),
	})
}

// allowBrowserTokenEntry reports whether the web UI may offer the manual token
// form and the server may accept a PAT from a browser: always without SSO, and
// with SSO only when ui.allowTokenEntry is set.
func (s *Server) allowBrowserTokenEntry() bool {
	return s.oidcCfg == nil || s.cfg.AllowTokenEntry
}
