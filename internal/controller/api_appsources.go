package controller

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/store"
)

// handleApplyAppSource parses an AppSource YAML and saves it to the database.
func (s *Server) handleApplyAppSource(w http.ResponseWriter, r *http.Request) {
	var req api.ApplyAppSourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	as, err := dsl.ParseAppSource(strings.NewReader(req.YAML))
	if err != nil {
		http.Error(w, "invalid yaml: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.guardManagedResource(r.Context(), "AppSource", as.Metadata.Name); err != nil {
		writeGuardError(w, err)
		return
	}
	specJSON, err := json.Marshal(as.Spec)
	if err != nil {
		http.Error(w, "marshal spec: "+err.Error(), http.StatusInternalServerError)
		return
	}
	stored, err := s.store.UpsertAppSource(r.Context(), as.Metadata.Name, specJSON)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, appSourceToMeta(stored, as.Spec))
}

// handleListAppSources returns all registered AppSources.
func (s *Server) handleListAppSources(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListAppSources(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	result := make([]api.AppSourceMeta, 0, len(list))
	for _, a := range list {
		var spec dsl.AppSourceSpec
		_ = json.Unmarshal(a.Spec, &spec)
		result = append(result, appSourceToMeta(&a, spec))
	}
	writeJSON(w, http.StatusOK, result)
}

// handleGetAppSource returns the AppSource with the given name.
func (s *Server) handleGetAppSource(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	a, err := s.store.GetAppSource(r.Context(), name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	var spec dsl.AppSourceSpec
	_ = json.Unmarshal(a.Spec, &spec)
	writeJSON(w, http.StatusOK, appSourceToMeta(a, spec))
}

// handleDeleteAppSource deletes the AppSource with the given name.
func (s *Server) handleDeleteAppSource(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := s.guardManagedResource(r.Context(), "AppSource", name); err != nil {
		writeGuardError(w, err)
		return
	}
	if err := s.store.DeleteAppSource(r.Context(), name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSyncAppSource resets the lastCommit of the given AppSource to force a re-sync on the next poll,
// and marks the AppSource as Syncing so the API/WebUI can reflect the in-progress state.
func (s *Server) handleSyncAppSource(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := s.store.ResetAppSourceCommit(r.Context(), name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.store.SetAppSourceSyncStatus(r.Context(), name, "Syncing", ""); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// appSourceToMeta converts a store.AppSource and dsl.AppSourceSpec into an api.AppSourceMeta.
func appSourceToMeta(a *store.AppSource, spec dsl.AppSourceSpec) api.AppSourceMeta {
	m := api.AppSourceMeta{
		Name:           a.Name,
		RepoURL:        spec.RepoURL,
		TargetRevision: spec.TargetRevision,
		Path:           spec.Path,
		LastSyncedAt:   a.LastSyncedAt,
		LastCommit:     a.LastCommit,
		SyncStatus:     a.SyncStatus,
		LastError:      a.LastError,
		UpdatedAt:      a.UpdatedAt,
	}
	if spec.SyncPolicy != (dsl.AppSyncPolicy{}) {
		m.SyncPolicy = &api.AppSourceSyncPolicy{
			Interval:            spec.SyncPolicy.Interval,
			Prune:               spec.SyncPolicy.Prune,
			AllowManualOverride: spec.SyncPolicy.AllowManualOverride,
		}
	}
	for _, ref := range a.ManagedResources {
		m.ManagedResources = append(m.ManagedResources, api.ResourceRef{Kind: ref.Kind, Name: ref.Name})
	}
	return m
}
