package controller

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/unified-cd/unified-cd/internal/api"
)

func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := s.store.ListAgents(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if agents == nil {
		agents = []api.AgentInfo{}
	}
	writeJSON(w, http.StatusOK, agents)
}

func (s *Server) handleGetAgent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "agentId")
	agent, err := s.store.GetAgent(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if agent == nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

func (s *Server) handleListRunsByAgent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "agentId")
	runs, err := s.store.ListRunsByAgent(r.Context(), id, 50)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if runs == nil {
		runs = []api.Run{}
	}
	writeJSON(w, http.StatusOK, runs)
}

func (s *Server) handleAgentDeregister(w http.ResponseWriter, r *http.Request) {
	agentID := chi.URLParam(r, "agentId")
	if err := s.store.DeleteAgent(r.Context(), agentID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
