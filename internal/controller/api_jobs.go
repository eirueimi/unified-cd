package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

// handleApplyJob parses a Job YAML definition and saves it to the database.
//
// Status codes are deliberately distinct: a malformed request/YAML is a client
// error (400), while marshal or store failures are server errors (500).
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

	stored, err := s.storeJob(r.Context(), job)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, stored)
}

// storeJob marshals the parsed Job's spec and upserts it under its qualified
// name. Both failures here are infrastructure/server errors, not client errors.
func (s *Server) storeJob(ctx context.Context, job *dsl.Job) (*api.Job, error) {
	specJSON, err := json.Marshal(job.Spec)
	if err != nil {
		return nil, fmt.Errorf("marshal spec: %w", err)
	}
	return s.store.UpsertJob(ctx, job.Metadata.QualifiedName(), job.APIVersion, specJSON)
}

// handleListJobs returns all registered Jobs, decorated with path/leaf.
func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.listJobsDecorated(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (s *Server) listJobsDecorated(ctx context.Context) ([]api.Job, error) {
	jobs, err := s.store.ListJobs(ctx)
	if err != nil {
		return nil, err
	}
	if jobs == nil {
		jobs = []api.Job{}
	}
	for i := range jobs {
		jobs[i].Inputs = specInputs(jobs[i].Spec)
		jobs[i].Path, jobs[i].Leaf = dsl.SplitQualifiedName(jobs[i].Name)
	}
	return jobs, nil
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
	job.Path, job.Leaf = dsl.SplitQualifiedName(job.Name)
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
