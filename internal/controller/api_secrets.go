package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/secrets"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
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
	// Validate the secret name (env-var style: letters/digits/_/-, starting
	// with a letter or '_') so it is always resolvable from {{ secrets.NAME }}.
	if err := dsl.ValidateSecretName(req.Name); err != nil {
		http.Error(w, fmt.Sprintf("name %v", err), http.StatusBadRequest)
		return
	}
	if req.Scope == "" {
		req.Scope = "global"
	}
	encDEK, ct, err := secrets.Encrypt(r.Context(), s.km, []byte(req.Value),
		secrets.SecretBinding(req.Name, req.Scope, req.ScopeRef))
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
	principal, ok := agentPrincipalFromContext(r.Context())
	if !ok || principal.AuthMethod == "legacy" {
		http.Error(w, "legacy agent credentials cannot fetch secrets", http.StatusForbidden)
		return
	}
	var req api.AgentFetchSecretsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.RunID == "" {
		http.Error(w, "runId is required", http.StatusBadRequest)
		return
	}
	agentID := chi.URLParam(r, "agentId")
	verdict, err := s.agentRunGuard(r.Context(), agentID, req.RunID, false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if respondRunWriteVerdict(w, verdict, req.RunID) {
		return
	}
	// Constrain the fetch to secrets this run's own spec actually references.
	// Without this, any agent holding a valid credential for *some* run it
	// owns could request the name of any secret in the store, regardless of
	// whether that run declares it. Names outside the allowed set are
	// rejected outright (before any decrypt attempt) with a generic message
	// that neither echoes the requested value nor confirms/denies the
	// secret's existence, so the response can't be used as an oracle to
	// enumerate the secret store.
	allowed, err := s.secretNamesForRun(r.Context(), req.RunID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, name := range req.Names {
		if _, ok := allowed[name]; !ok {
			http.Error(w, "secret not needed by this run", http.StatusForbidden)
			return
		}
	}
	result := map[string]string{}
	for _, name := range req.Names {
		stored, err := s.store.GetSecret(r.Context(), name, "global", "")
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Omitting the secret would hand the agent an empty value and let
				// the step run as if it had been configured. Fail loudly instead.
				http.Error(w, "secret "+name+" not found", http.StatusNotFound)
				return
			}
			// A transient store fault (e.g. DB connectivity) is not "not
			// found" — reporting it as 404 would mislead an operator into
			// thinking the secret needs to be re-registered.
			http.Error(w, "secret "+name+": "+err.Error(), http.StatusInternalServerError)
			return
		}
		plaintext, err := secrets.Decrypt(r.Context(), s.km, stored.EncryptedDEK, stored.Ciphertext,
			secrets.SecretBinding(stored.Name, stored.Scope, stored.ScopeRef))
		if err != nil {
			logSecretDecryptFailure("agent-fetch", name, err)
			http.Error(w, "decrypt "+name+": "+err.Error(), http.StatusInternalServerError)
			return
		}
		result[name] = string(plaintext)
	}
	writeJSON(w, http.StatusOK, api.AgentFetchSecretsResponse{Secrets: result})
}

// logSecretDecryptFailure logs a secrets.Decrypt failure at the controller's
// decrypt call sites. A binding-mismatch (AES-GCM AAD authentication)
// failure is logged distinctly and at a raised level, since it signals
// ciphertext substitution, tampering, or corruption rather than an ordinary
// decrypt error — see docs/superpowers/specs/2026-07-18-secrets-v2-design.md
// §7 (Error handling). site identifies the call path (e.g.
// "agent-fetch", "webhook-hmac", "webhook-token", "oidc-refresh"); id
// identifies the affected secret/session, never its value.
//
// Only identifiers are logged — never the secret value, the plaintext, or
// the AAD contents.
func logSecretDecryptFailure(site, id string, err error) {
	if errors.Is(err, secrets.ErrBindingMismatch) {
		slog.Error("secret decrypt: binding mismatch (possible ciphertext tampering or substitution)",
			"site", site, "id", id)
		return
	}
	if errors.Is(err, secrets.ErrProviderMismatch) {
		slog.Error("secret decrypt: wrapped key came from a different key provider (check UNIFIED_KMS_URI / UNIFIED_CONTROLLER_KEY_FILE)",
			"site", site, "id", id)
		return
	}
	slog.Warn("secret decrypt failed", "site", site, "id", id, "error", err)
}
