package controller

import (
	"net/http"
	"strconv"
)

const (
	defaultAuditListLimit = 100
	maxAuditListLimit     = 1000
)

// handleListAuditLogs returns audit log entries newest-first, paginated via
// ?limit=N&offset=M. admin role only (see server.go routing).
func (s *Server) handleListAuditLogs(w http.ResponseWriter, r *http.Request) {
	limit := defaultAuditListLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			http.Error(w, "invalid limit", http.StatusBadRequest)
			return
		}
		if n > maxAuditListLimit {
			n = maxAuditListLimit
		}
		limit = n
	}
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			http.Error(w, "invalid offset", http.StatusBadRequest)
			return
		}
		offset = n
	}

	list, err := s.store.ListAuditLogs(r.Context(), limit, offset)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, list)
}
