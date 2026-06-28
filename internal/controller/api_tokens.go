package controller

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/go-chi/chi/v5"
)

const patPrefix = "exc_"

// generatePAT generates a random Personal Access Token.
// Format: "exc_" + 40-character hex string (160-bit random value).
func generatePAT() (string, error) {
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return patPrefix + hex.EncodeToString(buf), nil
}

// handleCreateToken generates a PAT, saves it to the database, and returns the token once.
func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	var req api.CreatePATRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	token, err := generatePAT()
	if err != nil {
		http.Error(w, "token generation error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// If expiresIn is provided, parse it and calculate the expiry time.
	var expiresAt *time.Time
	if req.ExpiresIn != "" {
		d, err := time.ParseDuration(req.ExpiresIn)
		if err != nil {
			http.Error(w, "invalid expiresIn: "+err.Error(), http.StatusBadRequest)
			return
		}
		t := time.Now().Add(d)
		expiresAt = &t
	}

	// Store the token as a hash in the database (the plaintext is included in the response only once).
	hash := HashToken(token)
	pat, err := s.store.CreatePAT(r.Context(), req.Name, hash, expiresAt)
	if err != nil {
		http.Error(w, "store error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, api.CreatePATResponse{
		ID:    pat.ID,
		Name:  pat.Name,
		Token: token,
	})
}

// handleListTokens returns the list of PATs (token hashes are not included).
func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	pats, err := s.store.ListPATs(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	result := make([]api.PATMeta, 0, len(pats))
	for _, p := range pats {
		result = append(result, api.PATMeta{
			ID:         p.ID,
			Name:       p.Name,
			CreatedAt:  p.CreatedAt,
			ExpiresAt:  p.ExpiresAt,
			LastUsedAt: p.LastUsedAt,
		})
	}
	writeJSON(w, http.StatusOK, result)
}

// handleDeleteToken deletes (revokes) the PAT with the given ID.
func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.store.DeletePAT(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleOIDCConfig is a public endpoint that returns the OIDC provider configuration
// (issuer, clientId, browserSSOEnabled). The server performs OIDC discovery and also
// returns deviceAuthEndpoint and tokenEndpoint so the CLI can skip its own well-known
// discovery. Returns 404 when OIDC is not configured.
func (s *Server) handleOIDCConfig(w http.ResponseWriter, r *http.Request) {
	if s.oidcCfg == nil {
		http.Error(w, "OIDC not configured", http.StatusNotFound)
		return
	}
	// The CLI device flow uses the public client (DeviceClientID).
	// Falls back to the browser ClientID when not set.
	deviceClientID := s.oidcCfg.DeviceClientID
	if deviceClientID == "" {
		deviceClientID = s.oidcCfg.ClientID
	}
	resp := map[string]any{
		"issuer":            s.oidcCfg.Issuer,
		"clientId":          s.oidcCfg.ClientID,
		"deviceClientId":    deviceClientID,
		"browserSSOEnabled": s.oidcCfg.ClientSecret != "",
	}
	// Fetch the actual endpoints via discovery (Dex device authorization is at /device/code) and return them.
	// When IssuerInternal is set, discovery is performed via the internal network.
	if _, _, oauth2cfg, err := s.oidcProvider(r.Context(), r.Host); err == nil {
		resp["deviceAuthEndpoint"] = oauth2cfg.Endpoint.DeviceAuthURL
		resp["tokenEndpoint"] = oauth2cfg.Endpoint.TokenURL
	}
	writeJSON(w, http.StatusOK, resp)
}
