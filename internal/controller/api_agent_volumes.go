package controller

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/go-chi/chi/v5"
)

// handleApplyAgentVolume accepts an AgentVolume manifest YAML and creates or
// updates it.
func (s *Server) handleApplyAgentVolume(w http.ResponseWriter, r *http.Request) {
	var req api.ApplyAgentVolumeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	v, err := dsl.ParseAgentVolume(strings.NewReader(req.YAML))
	if err != nil {
		http.Error(w, "invalid yaml: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.guardManagedResource(r.Context(), "AgentVolume", v.Metadata.Name); err != nil {
		writeGuardError(w, err)
		return
	}
	specJSON, err := json.Marshal(v.Spec)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.store.UpsertAgentVolume(r.Context(), v.Metadata.Name, specJSON); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rec, err := s.store.GetAgentVolume(r.Context(), v.Metadata.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	meta := api.AgentVolumeMeta{Name: v.Metadata.Name, Spec: v.Spec}
	if rec != nil {
		meta.UpdatedAt = rec.UpdatedAt
	}
	writeJSON(w, http.StatusOK, meta)
}

// handleListAgentVolumes returns every registered AgentVolume manifest.
func (s *Server) handleListAgentVolumes(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListAgentVolumes(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	result := make([]api.AgentVolumeMeta, 0, len(list))
	for _, rec := range list {
		var spec dsl.AgentVolumeSpec
		if err := json.Unmarshal(rec.Spec, &spec); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		result = append(result, api.AgentVolumeMeta{Name: rec.Name, Spec: spec, UpdatedAt: rec.UpdatedAt})
	}
	writeJSON(w, http.StatusOK, result)
}

// handleDeleteAgentVolume deletes the AgentVolume manifest with the given name.
func (s *Server) handleDeleteAgentVolume(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := s.guardManagedResource(r.Context(), "AgentVolume", name); err != nil {
		writeGuardError(w, err)
		return
	}
	if err := s.store.DeleteAgentVolume(r.Context(), name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
