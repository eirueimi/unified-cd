package controller

import (
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// handleArtifactUpload handles PUT /api/v1/runs/{runID}/artifacts/{name}.
// Saves the request body as-is to the object store.
func (s *Server) handleArtifactUpload(w http.ResponseWriter, r *http.Request) {
	if s.objStore == nil {
		http.Error(w, "object store not configured", http.StatusServiceUnavailable)
		return
	}
	runID := chi.URLParam(r, "runID")
	name := chi.URLParam(r, "name")
	key := fmt.Sprintf("artifacts/%s/%s.tar.gz", runID, name)

	size := r.ContentLength
	if size < 0 {
		size = -1
	}
	if err := s.objStore.Put(r.Context(), key, r.Body, size); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleArtifactDownload handles GET /api/v1/runs/{runID}/artifacts/{name}.
// Streams the object from the object store directly into the response.
func (s *Server) handleArtifactDownload(w http.ResponseWriter, r *http.Request) {
	if s.objStore == nil {
		http.Error(w, "object store not configured", http.StatusServiceUnavailable)
		return
	}
	runID := chi.URLParam(r, "runID")
	name := chi.URLParam(r, "name")
	key := fmt.Sprintf("artifacts/%s/%s.tar.gz", runID, name)

	rc, err := s.objStore.Get(r.Context(), key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}
