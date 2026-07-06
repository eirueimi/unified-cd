package controller

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

// handleApplySchedule accepts a Schedule YAML and creates or updates the cron schedule.
func (s *Server) handleApplySchedule(w http.ResponseWriter, r *http.Request) {
	var req api.ApplyScheduleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sc, err := dsl.ParseSchedule(strings.NewReader(req.YAML))
	if err != nil {
		http.Error(w, "invalid yaml: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.guardManagedResource(r.Context(), "Schedule", sc.Metadata.Name); err != nil {
		writeGuardError(w, err)
		return
	}
	if _, err := s.store.GetJob(r.Context(), sc.Spec.Job); err != nil {
		http.Error(w, "job not found: "+sc.Spec.Job, http.StatusBadRequest)
		return
	}
	stored, err := s.store.UpsertSchedule(r.Context(), sc.Metadata.Name, sc.Spec.Cron, sc.Spec.Job, sc.Spec.Params)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, api.ScheduleMeta{
		Name:        stored.Name,
		Cron:        stored.Cron,
		JobName:     stored.JobName,
		LastFiredAt: stored.LastFiredAt,
		UpdatedAt:   stored.UpdatedAt,
		Params:      stored.Params,
	})
}

// handleListSchedules returns the list of registered Schedules.
func (s *Server) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListSchedules(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	result := make([]api.ScheduleMeta, 0, len(list))
	for _, sc := range list {
		result = append(result, api.ScheduleMeta{
			Name:        sc.Name,
			Cron:        sc.Cron,
			JobName:     sc.JobName,
			LastFiredAt: sc.LastFiredAt,
			UpdatedAt:   sc.UpdatedAt,
			Params:      sc.Params,
		})
	}
	writeJSON(w, http.StatusOK, result)
}

// handleDeleteSchedule deletes the Schedule with the given name. Idempotent — returns 204 even if the schedule does not exist.
func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := s.guardManagedResource(r.Context(), "Schedule", name); err != nil {
		writeGuardError(w, err)
		return
	}
	if err := s.store.DeleteSchedule(r.Context(), name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
