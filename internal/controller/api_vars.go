package controller

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/go-chi/chi/v5"
)

// handleApplyVars accepts a Vars manifest YAML and creates or updates it.
//
// Unlike the other apply handlers, this one also rejects the write when it
// would collide with a DIFFERENT Vars manifest that already defines one of
// the same keys (see dsl.CheckVarsCollision). This is the interactive apply
// path — an author is present to read the error and fix it — which is why
// the check lives here and not on the AppSource sync path (see the comment
// at the "Vars" case in appsource_apply.go's applyResource).
func (s *Server) handleApplyVars(w http.ResponseWriter, r *http.Request) {
	var req api.ApplyVarsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	v, err := dsl.ParseVars(strings.NewReader(req.YAML))
	if err != nil {
		http.Error(w, "invalid yaml: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.guardManagedResource(r.Context(), "Vars", v.Metadata.Name); err != nil {
		writeGuardError(w, err)
		return
	}

	// Collision check across manifests. ListVars is sorted by name, so the
	// error names the same two manifests every time rather than whichever the
	// database happened to return first. CheckVarsCollision returns nil when
	// the existing record's own manifest name matches the incoming one, so
	// re-applying the same manifest (an update) is never flagged as a
	// collision with itself.
	existing, err := s.store.ListVars(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, rec := range existing {
		var spec dsl.VarsSpec
		if err := json.Unmarshal(rec.Spec, &spec); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if cerr := dsl.CheckVarsCollision(spec.Vars, rec.Name, v.Spec.Vars, v.Metadata.Name); cerr != nil {
			http.Error(w, cerr.Error(), http.StatusBadRequest)
			return
		}
	}

	specJSON, err := json.Marshal(v.Spec)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	name, err := s.store.UpsertVars(r.Context(), v.Metadata.Name, specJSON)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, api.VarsMeta{Name: name, Vars: v.Spec.Vars})
}

// handleListVars returns every registered Vars manifest.
func (s *Server) handleListVars(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListVars(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	result := make([]api.VarsMeta, 0, len(list))
	for _, rec := range list {
		var spec dsl.VarsSpec
		if err := json.Unmarshal(rec.Spec, &spec); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		result = append(result, api.VarsMeta{Name: rec.Name, Vars: spec.Vars})
	}
	writeJSON(w, http.StatusOK, result)
}

// handleDeleteVars deletes the Vars manifest with the given name.
func (s *Server) handleDeleteVars(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := s.guardManagedResource(r.Context(), "Vars", name); err != nil {
		writeGuardError(w, err)
		return
	}
	if err := s.store.DeleteVars(r.Context(), name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
