package controller

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

// handleApplyJob parses a Job YAML definition and saves it to the database.
func (s *Server) handleApplyJob(w http.ResponseWriter, r *http.Request) {
	var req api.ApplyJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	job, err := dsl.Parse(strings.NewReader(req.YAML))
	if err != nil {
		http.Error(w, "invalid yaml: "+err.Error(), http.StatusBadRequest)
		return
	}
	specJSON, err := json.Marshal(job.Spec)
	if err != nil {
		http.Error(w, "marshal spec: "+err.Error(), http.StatusInternalServerError)
		return
	}
	stored, err := s.store.UpsertJob(r.Context(), job.Metadata.Name, job.APIVersion, specJSON)
	if err != nil {
		http.Error(w, "store: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, stored)
}

// handleListJobs returns all registered Jobs.
func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.store.ListJobs(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if jobs == nil {
		jobs = []api.Job{}
	}
	for i := range jobs {
		jobs[i].Inputs = specInputs(jobs[i].Spec)
	}
	writeJSON(w, http.StatusOK, jobs)
}

// handleGetJob returns the Job with the given name.
func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	job, err := s.store.GetJob(r.Context(), name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	job.Inputs = specInputs(job.Spec)
	writeJSON(w, http.StatusOK, job)
}

// handleGetJobYAML returns the YAML definition of the specified Job.
func (s *Server) handleGetJobYAML(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	job, err := s.store.GetJob(r.Context(), name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	yamlBytes, err := specJSONToYAML(job.Spec)
	if err != nil {
		slog.Warn("job yaml render failed", "job", name, "error", err)
		http.Error(w, "render yaml: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(yamlBytes)
}

// handleDeleteJob deletes the Job with the given name. Associated Run history is also cascade-deleted.
func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := s.store.DeleteJob(r.Context(), name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// specInputs extracts the inputs definition from the stored spec JSON.
// Returns nil when parsing fails or when there are no inputs.
func specInputs(specJSON []byte) []api.InputDef {
	var spec dsl.Spec
	if err := json.Unmarshal(specJSON, &spec); err != nil || len(spec.Params.Inputs) == 0 {
		return nil
	}
	inputs := make([]api.InputDef, len(spec.Params.Inputs))
	for i, in := range spec.Params.Inputs {
		inputs[i] = api.InputDef{
			Name:        in.Name,
			Type:        in.Type,
			Required:    in.Required,
			Default:     in.Default,
			Description: in.Description,
		}
	}
	return inputs
}

// writeJSON is a helper that writes a JSON response.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
