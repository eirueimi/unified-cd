package controller

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/unified-cd/unified-cd/internal/api"
	"github.com/unified-cd/unified-cd/internal/dsl"
)

func (s *Server) handleUpsertGitCredential(w http.ResponseWriter, r *http.Request) {
	var req api.UpsertGitCredentialRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := dsl.ValidateName(req.Name); err != nil {
		http.Error(w, "name "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Host == "" || req.CredType == "" || req.SecretRef == "" {
		http.Error(w, "host, credType, secretRef are required", http.StatusBadRequest)
		return
	}
	if req.CredType != "token" && req.CredType != "sshKey" {
		http.Error(w, "credType must be 'token' or 'sshKey'", http.StatusBadRequest)
		return
	}
	if err := s.store.UpsertGitCredential(r.Context(), req.Name, req.Host, req.CredType, req.SecretRef); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListGitCredentials(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListGitCredentials(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	result := make([]api.GitCredentialMeta, 0, len(list))
	for _, gc := range list {
		result = append(result, api.GitCredentialMeta{
			ID: gc.ID, Name: gc.Name, Host: gc.Host,
			CredType: gc.CredType, SecretRef: gc.SecretRef, CreatedAt: gc.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleDeleteGitCredential(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := s.store.DeleteGitCredential(r.Context(), name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
