package controller

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/go-chi/chi/v5"
)

func (s *Server) handleDecideApproval(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	stepIndex, err := strconv.Atoi(chi.URLParam(r, "stepIndex"))
	if err != nil {
		http.Error(w, "bad step index", http.StatusBadRequest)
		return
	}
	var req api.ApprovalDecisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	status := ""
	switch req.Decision {
	case "approve":
		status = "Approved"
	case "reject":
		status = "Rejected"
	default:
		http.Error(w, `decision must be "approve" or "reject"`, http.StatusBadRequest)
		return
	}
	decidedBy := "unknown"
	if p, ok := principalFromContext(r.Context()); ok && p.Name != "" {
		decidedBy = p.Name
	}
	changed, err := s.store.DecideApproval(r.Context(), runID, stepIndex, status, decidedBy, req.Comment)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !changed {
		// Either no pending row (404) or already decided (409): disambiguate via GetApproval.
		if _, err := s.store.GetApproval(r.Context(), runID, stepIndex); err != nil {
			http.Error(w, "no pending approval", http.StatusNotFound)
			return
		}
		http.Error(w, "already decided", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListRunApprovals(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	list, err := s.store.ListRunApprovals(r.Context(), runID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []api.RunApproval{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleAgentCreateApproval(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runId")
	var req api.CreateApprovalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	agentID := chi.URLParam(r, "agentId")
	v, gerr := s.agentRunGuard(r.Context(), agentID, runID, true)
	if gerr != nil {
		http.Error(w, gerr.Error(), http.StatusInternalServerError)
		return
	}
	if respondRunWriteVerdict(w, v, runID) {
		return
	}
	var timeoutAt *time.Time
	if req.TimeoutMinutes > 0 {
		t := time.Now().Add(time.Duration(req.TimeoutMinutes * float64(time.Minute)))
		timeoutAt = &t
	}
	if err := s.store.CreatePendingApproval(r.Context(), runID, req.StepIndex, req.StepName, req.Message, timeoutAt); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAgentGetApproval(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runId")
	stepIndex, err := strconv.Atoi(chi.URLParam(r, "stepIndex"))
	if err != nil {
		http.Error(w, "bad step index", http.StatusBadRequest)
		return
	}
	a, err := s.store.GetApproval(r.Context(), runID, stepIndex)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, a)
}
