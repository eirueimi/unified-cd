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
		"stderrPlain": s.cfg.StderrPlain,
	})
}
