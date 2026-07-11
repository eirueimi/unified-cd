package controller

import (
	"context"
	"encoding/json"
	"errors"
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
// Status codes are deliberately distinct: a malformed request/YAML, or a job
// whose annotations.path contains an invalid segment, is a client error
// (400); marshal or store failures are server errors (500).
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

	if err := s.guardManagedResource(r.Context(), "Job", job.Metadata.QualifiedName()); err != nil {
		writeGuardError(w, err)
		return
	}

	stored, err := s.storeJob(r.Context(), job)
	if err != nil {
		var badReq errBadRequest
		if errors.As(err, &badReq) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, stored)
}

// storeJob marshals the parsed Job's spec and upserts it under its qualified
// name. Both failures here are infrastructure/server errors, not client errors.
//
// Direct apply is the only path that reaches here (see handleApplyJob); the
// AppSource reconciler upserts jobs itself using directory names straight from
// a trusted git tree and never calls storeJob. So path-segment validation
// belongs here, not in dsl.Job.Validate — that would also reject legitimate
// AppSource subdirectory names that don't happen to be DNS-1123 subdomains.
func (s *Server) storeJob(ctx context.Context, job *dsl.Job) (*api.Job, error) {
	if err := validatePathAnnotation(job.Metadata.Annotations["path"]); err != nil {
		return nil, errBadRequest{err}
	}
	specJSON, err := json.Marshal(job.Spec)
	if err != nil {
		return nil, fmt.Errorf("marshal spec: %w", err)
	}
	return s.store.UpsertJob(ctx, job.Metadata.QualifiedName(), job.APIVersion, specJSON)
}

// validatePathAnnotation validates each non-empty segment of the "path"
// annotation using the same DNS-1123 rule already applied to metadata.name.
// This is only meaningful on direct apply, where an untrusted caller supplies
// the annotation directly; an empty path (no annotation) yields no segments
// and is always accepted unchanged.
func validatePathAnnotation(path string) error {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return nil
	}
	for _, seg := range strings.Split(trimmed, "/") {
		if seg == "" {
			// Collapse repeated slashes (e.g. "a//b") rather than treating the
			// empty segment as invalid; QualifyName/store behavior for such
			// input is unaffected by this validation either way.
			continue
		}
		if err := dsl.ValidateName(seg); err != nil {
			return fmt.Errorf("invalid path segment %q: %w", seg, err)
		}
	}
	return nil
}

// errBadRequest marks an error as a client error (HTTP 400) rather than the
// default 500 used for other storeJob failures.
type errBadRequest struct{ err error }

func (e errBadRequest) Error() string { return e.err.Error() }
func (e errBadRequest) Unwrap() error { return e.err }

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

// extractJobName returns the catch-all wildcard as the job name.
func extractJobName(wild string) string {
	return strings.TrimPrefix(wild, "/")
}

// extractJobNameAndYAML strips an optional trailing "/yaml" segment, reporting
// whether it was present.
func extractJobNameAndYAML(wild string) (name string, yaml bool) {
	wild = strings.TrimPrefix(wild, "/")
	if strings.HasSuffix(wild, "/yaml") {
		return strings.TrimSuffix(wild, "/yaml"), true
	}
	return wild, false
}

// NOTE: a job whose leaf is literally "yaml" or "schedulability" (qualified
// name ".../yaml" or ".../schedulability") is unreachable via GET as a job —
// the suffix is read as the YAML/schedulability discriminator. Such names are
// not expected; direct apply can still create/delete them.

// handleGetJobOrYAML dispatches GET /jobs/* to the job, its YAML, or its
// schedulability evaluation.
func (s *Server) handleGetJobOrYAML(w http.ResponseWriter, r *http.Request) {
	wild := strings.TrimPrefix(chi.URLParam(r, "*"), "/")
	if strings.HasSuffix(wild, "/schedulability") {
		s.serveJobSchedulability(w, r, strings.TrimSuffix(wild, "/schedulability"))
		return
	}
	name, yaml := extractJobNameAndYAML(wild)
	if yaml {
		s.serveJobYAML(w, r, name)
		return
	}
	s.serveJob(w, r, name)
}

// serveJob returns the Job with the given name.
func (s *Server) serveJob(w http.ResponseWriter, r *http.Request, name string) {
	job, err := s.store.GetJob(r.Context(), name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	job.Inputs = specInputs(job.Spec)
	job.Path, job.Leaf = dsl.SplitQualifiedName(job.Name)
	writeJSON(w, http.StatusOK, job)
}

// serveJobYAML returns the YAML definition of the specified Job.
func (s *Server) serveJobYAML(w http.ResponseWriter, r *http.Request, name string) {
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

// serveJobSchedulability evaluates whether any registered agent can currently
// run the named Job and returns the Schedulability report as JSON. Spec
// parsing mirrors handleTriggerRun: an unparseable stored spec is treated as
// requiring nothing rather than failing the request.
func (s *Server) serveJobSchedulability(w http.ResponseWriter, r *http.Request, name string) {
	job, err := s.store.GetJob(r.Context(), name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	var spec dsl.Spec
	if err := json.Unmarshal(job.Spec, &spec); err != nil {
		http.Error(w, "invalid stored spec: "+err.Error(), http.StatusInternalServerError)
		return
	}
	agents, err := s.store.ListAgents(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, EvaluateSchedulability(spec, agents))
}

// handleDeleteJob deletes the Job with the given name. Associated Run history is also cascade-deleted.
func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	name := extractJobName(chi.URLParam(r, "*"))
	if err := s.guardManagedResource(r.Context(), "Job", name); err != nil {
		writeGuardError(w, err)
		return
	}
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
