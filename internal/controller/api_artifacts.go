package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/artifact"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/store"
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
	principal, ok := agentPrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// Apply the same per-run ownership check to every principal, including
	// legacy shared-token callers: this route has no {agentId} path segment,
	// so a legacy principal's AgentID is always empty (there is nothing to
	// bind it to, unlike the agentId-scoped agent routes) — which means
	// agentRunGuard can never find a matching claimed_by for it. That is the
	// correct outcome, not a bug: a legacy caller presents no identity at all
	// here, so it cannot be trusted to write to any run's artifacts, exactly
	// like the (deliberately fail-closed) secrets-fetch path.
	//
	// A nil store means this guard cannot run at all — not "no ownership to
	// check", but "no way to check it". Silently skipping it (the previous
	// behavior) would apply no authz whatsoever to anyone on a
	// store-misconfigured server, which is the opposite of fail-closed. Refuse
	// the request instead: production always has a store, so this only ever
	// fires against a genuine misconfiguration.
	if s.store == nil {
		http.Error(w, "store not configured", http.StatusServiceUnavailable)
		return
	}
	verdict, err := s.agentRunGuard(r.Context(), principal.AgentID, runID, false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if respondRunWriteVerdict(w, verdict, runID) {
		return
	}
	if _, err := s.store.GetRun(r.Context(), runID); err != nil {
		if errors.Is(err, store.ErrRunNotFound) {
			// A late upload for a deleted run would create an orphaned
			// object nothing ever cleans up (deleteRunEverywhere already
			// ran its prefix delete). Terminal-but-existing runs are still
			// accepted: their objects stay referenced and are removed with
			// the run.
			http.Error(w, "run not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	name := chi.URLParam(r, "name")
	key, err := artifact.ArtifactKey(runID, name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

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
		// ObjectStore.Get detects a missing key eagerly (before any bytes are
		// read), so a missing artifact yields a clean 404 here rather than a
		// 200 that breaks mid-stream. Only a genuine miss becomes 404; any
		// other error (e.g. a transient backend failure) is a 500 so it isn't
		// mistaken for "artifact doesn't exist".
		if errors.Is(err, objectstore.ErrNotFound) {
			http.Error(w, "artifact not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

// handleArtifactList handles GET /api/v1/runs/{runID}/artifacts.
// Lists artifact names for the run from the object store.
func (s *Server) handleArtifactList(w http.ResponseWriter, r *http.Request) {
	if s.objStore == nil {
		http.Error(w, "object store not configured", http.StatusServiceUnavailable)
		return
	}
	runID := chi.URLParam(r, "runID")
	prefix := fmt.Sprintf("artifacts/%s/", runID)
	keys, err := s.objStore.List(r.Context(), prefix)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]api.ArtifactInfo, 0, len(keys))
	for _, k := range keys {
		// Only surface keys that match the artifact layout exactly
		// (artifacts/{runID}/{name}.tar.gz); skip any foreign object under the prefix.
		if !strings.HasPrefix(k, prefix) || !strings.HasSuffix(k, ".tar.gz") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(k, prefix), ".tar.gz")
		if name == "" {
			continue
		}
		out = append(out, api.ArtifactInfo{Name: name})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
