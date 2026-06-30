package controller

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

// handleTriggerRun creates a new Run and returns it in Pending state.
func (s *Server) handleTriggerRun(w http.ResponseWriter, r *http.Request) {
	var req api.TriggerRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.JobName == "" {
		http.Error(w, "jobName is required", http.StatusBadRequest)
		return
	}
	job, err := s.store.GetJob(r.Context(), req.JobName)
	if err != nil {
		http.Error(w, "job not found: "+req.JobName, http.StatusNotFound)
		return
	}
	// Extract the agentSelector from the stored spec JSON.
	var spec dsl.Spec
	agentSelector := []string{}
	if err := json.Unmarshal(job.Spec, &spec); err == nil {
		agentSelector = spec.AgentSelector
	}
	agentSelector, err = dsl.ExpandAgentSelector(agentSelector, req.Params)
	if err != nil {
		http.Error(w, "agentSelector: "+err.Error(), http.StatusBadRequest)
		return
	}
	// podTemplate requires a Kubernetes agent; add "kubernetes" to the selector automatically.
	if spec.PodTemplate != nil {
		agentSelector = appendLabelIfMissing(agentSelector, "kubernetes")
	}
	triggeredBy := "api"
	if p, ok := principalFromContext(r.Context()); ok && p.Name != "" {
		triggeredBy = p.Name
	}
	run, err := s.store.CreateRun(r.Context(), job.Name, req.Params, job.Spec, agentSelector, triggeredBy)
	if err != nil {
		http.Error(w, "create run: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// handleGetRun returns the Run with the given ID.
func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	run, err := s.store.GetRun(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// handleGetRunYAML returns the YAML definition of the specified Run.
func (s *Server) handleGetRunYAML(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	specJSON, err := s.store.GetRunSpec(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	yamlBytes, err := specJSONToYAML(specJSON)
	if err != nil {
		http.Error(w, "render yaml: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(yamlBytes)
}

// handleTailLogs returns logs for the specified Run, starting after the sequence number given by the after query parameter.
func (s *Server) handleTailLogs(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	afterStr := r.URL.Query().Get("after")
	var after int64
	_, _ = fmt.Sscanf(afterStr, "%d", &after)
	lines, err := s.store.TailLogs(r.Context(), id, after, 1000)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if lines == nil {
		lines = []api.LogLine{}
	}
	writeJSON(w, http.StatusOK, lines)
}

// handleGetRunOutputs returns the outputs of the specified Run.
func (s *Server) handleGetRunOutputs(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	outputs, err := s.store.GetRunOutputs(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if outputs == nil {
		outputs = map[string]string{}
	}
	writeJSON(w, http.StatusOK, api.RunOutputs{RunID: id, Outputs: outputs})
}

// handleListRunsByJob returns the most recent Runs for the specified Job.
func (s *Server) handleListRunsByJob(w http.ResponseWriter, r *http.Request) {
	jobName := r.URL.Query().Get("jobName")
	if jobName == "" {
		http.Error(w, "jobName query parameter is required", http.StatusBadRequest)
		return
	}
	runs, err := s.store.ListRunsByJob(r.Context(), jobName, 50)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if runs == nil {
		runs = []api.Run{}
	}
	writeJSON(w, http.StatusOK, runs)
}

// handleListActiveRuns returns all Runs in Pending, Queued, or Running state across all jobs.
func (s *Server) handleListActiveRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.store.ListActiveRuns(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if runs == nil {
		runs = []api.Run{}
	}
	writeJSON(w, http.StatusOK, runs)
}

// handleGetRunSteps returns the list of steps for the specified Run.
func (s *Server) handleGetRunSteps(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	steps, err := s.store.GetRunSteps(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if steps == nil {
		steps = []api.StepReport{}
	}
	writeJSON(w, http.StatusOK, steps)
}

// handleCancelRun transitions the specified Run to the Cancelled state.
func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.store.MarkRunFinished(r.Context(), id, api.RunCancelled); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteRun deletes a Run that has reached a terminal state (Succeeded/Failed/Cancelled).
// Returns 409 if the Run is still in progress.
func (s *Server) handleDeleteRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	run, err := s.store.GetRun(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	switch run.Status {
	case api.RunSucceeded, api.RunFailed, api.RunCancelled:
	default:
		http.Error(w, fmt.Sprintf("run %s is still %s; only terminal runs can be deleted", id, run.Status), http.StatusConflict)
		return
	}
	if err := s.store.DeleteRun(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleLogsArchive retrieves archived logs for a Run from the object store and streams them back.
func (s *Server) handleLogsArchive(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	arch, err := s.store.GetLogArchive(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if arch == nil {
		http.Error(w, "log archive not available yet", http.StatusNotFound)
		return
	}
	if s.objStore == nil {
		http.Error(w, "object store not configured", http.StatusNotImplemented)
		return
	}
	rc, err := s.objStore.Get(r.Context(), arch.ObjectKey)
	if err != nil {
		http.Error(w, "fetch archive: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", `attachment; filename="logs.ndjson"`)
	_, _ = io.Copy(w, rc)
}

func appendLabelIfMissing(labels []string, label string) []string {
	for _, l := range labels {
		if l == label {
			return labels
		}
	}
	return append(labels, label)
}
