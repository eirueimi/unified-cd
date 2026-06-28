package controller

import (
	"encoding/json"
	"net/http"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/secrets"
	"github.com/go-chi/chi/v5"
)

// handleSetSecret creates or updates a secret.
func (s *Server) handleSetSecret(w http.ResponseWriter, r *http.Request) {
	if s.km == nil {
		http.Error(w, "key manager not configured", http.StatusNotImplemented)
		return
	}
	var req api.SetSecretRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if req.Scope == "" {
		req.Scope = "global"
	}
	encDEK, ct, err := secrets.Encrypt(r.Context(), s.km, []byte(req.Value))
	if err != nil {
		http.Error(w, "encrypt: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := s.store.UpsertSecret(r.Context(), req.Name, req.Scope, req.ScopeRef, encDEK, ct); err != nil {
		http.Error(w, "store: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListSecrets returns the metadata list of secrets (values are not included).
func (s *Server) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = "global"
	}
	scopeRef := r.URL.Query().Get("scopeRef")
	list, err := s.store.ListSecrets(r.Context(), scope, scopeRef)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	result := make([]api.SecretMeta, 0, len(list))
	for _, m := range list {
		result = append(result, api.SecretMeta{
			ID: m.ID, Name: m.Name, Scope: m.Scope, ScopeRef: m.ScopeRef, CreatedAt: m.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, result)
}

// handleDeleteSecret deletes the secret with the given name.
func (s *Server) handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = "global"
	}
	scopeRef := r.URL.Query().Get("scopeRef")
	if err := s.store.DeleteSecret(r.Context(), name, scope, scopeRef); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAgentSecretsFetch handles a secrets-fetch request from an agent.
// Decrypts the requested secrets and returns them as plaintext.
func (s *Server) handleAgentSecretsFetch(w http.ResponseWriter, r *http.Request) {
	if s.km == nil {
		http.Error(w, "key manager not configured", http.StatusNotImplemented)
		return
	}
	var req api.AgentFetchSecretsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	result := map[string]string{}
	for _, name := range req.Names {
		stored, err := s.store.GetSecret(r.Context(), name, "global", "")
		if err != nil {
			// Skip if not found.
			continue
		}
		plaintext, err := secrets.Decrypt(r.Context(), s.km, stored.EncryptedDEK, stored.Ciphertext)
		if err != nil {
			http.Error(w, "decrypt "+name+": "+err.Error(), http.StatusInternalServerError)
			return
		}
		result[name] = string(plaintext)
	}
	writeJSON(w, http.StatusOK, api.AgentFetchSecretsResponse{Secrets: result})
}
