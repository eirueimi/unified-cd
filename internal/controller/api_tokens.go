package controller

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/eirueimi/unified-cd/internal/store"
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

// validateCeiling checks a requested PAT ceiling against what the creator
// holds (spec 5.15). Nothing is ever clamped: a ceiling beyond the creator's is
// a 400. For a PAT creator, Role and Grants are already restricted by its own
// ceiling, so the bound is the PAT's own.
func validateCeiling(creator Principal, role string, perms []string) (int, string) {
	if role != "" {
		if roleRank(role) == 0 {
			return http.StatusBadRequest, fmt.Sprintf("unknown role %q", role)
		}
		if roleRank(role) > roleRank(creator.Role) {
			have := "(none)"
			if creator.Role != "" {
				have = fmt.Sprintf("%q", creator.Role)
			}
			return http.StatusBadRequest, fmt.Sprintf("cannot grant role %q: it exceeds your global role %s; use --permission to grant folder permissions you hold", role, have)
		}
	}
	for _, name := range perms {
		perm, ok := authz.Parse(name)
		if !ok {
			return http.StatusBadRequest, fmt.Sprintf("unknown permission %q", name)
		}
		if !creator.Grants.HeldAnywhere(perm) {
			return http.StatusBadRequest, fmt.Sprintf("cannot grant permission %q: you do not hold it", name)
		}
	}
	return 0, ""
}

// rolesClaimName is the configured oidc.rolesClaim, defaulting to "groups".
func (s *Server) rolesClaimName() string {
	if s.oidcCfg != nil && s.oidcCfg.RolesClaim != "" {
		return s.oidcCfg.RolesClaim
	}
	return "groups"
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
	if req.Name == BootstrapPATName || strings.HasPrefix(req.Name, "env:") {
		http.Error(w, fmt.Sprintf("token name %q is reserved", req.Name), http.StatusBadRequest)
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

	creator, _ := principalFromContext(r.Context())
	roleOnlyCreator := creator.BreakGlass || (creator.Kind == "pat" && creator.OwnerPATID == "")

	token, err := generatePAT()
	if err != nil {
		http.Error(w, "token generation error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	hash := HashToken(token)

	var pat *store.PAT
	if roleOnlyCreator {
		// Break-glass and role-only PATs have no owner to record: the new PAT is
		// role-only too, bounded by the creator's role.
		if req.Permissions != nil {
			http.Error(w, "cannot grant permissions: this token has no owner; use --role", http.StatusBadRequest)
			return
		}
		role := req.Role
		if role == "" {
			role = creator.Role
		}
		if status, msg := validateCeiling(creator, role, nil); status != 0 {
			http.Error(w, msg, status)
			return
		}
		pat, err = s.store.CreatePAT(r.Context(), req.Name, hash, role, expiresAt)
	} else {
		if !creator.ClaimsKnown {
			if creator.Kind == "session" {
				http.Error(w, "cannot create a token: this login predates group-aware tokens; log out and log in again, then retry", http.StatusForbidden)
			} else {
				http.Error(w, fmt.Sprintf("cannot create a token: your OIDC token carries no %q claim; log in again so the token includes it, then retry", s.rolesClaimName()), http.StatusForbidden)
			}
			return
		}
		if creator.Sub == "" {
			// No owner PAT may be stored with an empty owner_sub.
			http.Error(w, "cannot create a token: your identity has no subject (sub); log in again, then retry", http.StatusForbidden)
			return
		}
		if req.Role != "" && req.Permissions != nil {
			http.Error(w, "role and permissions are mutually exclusive", http.StatusBadRequest)
			return
		}
		if req.Permissions != nil && len(req.Permissions) == 0 {
			http.Error(w, "permissions must not be empty", http.StatusBadRequest)
			return
		}
		if status, msg := validateCeiling(creator, req.Role, req.Permissions); status != 0 {
			http.Error(w, msg, status)
			return
		}
		role, perms := req.Role, req.Permissions
		if creator.OwnerPATID != "" {
			// PAT-from-PAT: the child is bounded by the parent's ceiling.
			switch {
			case req.Role == "" && req.Permissions == nil:
				role, perms = creator.CeilingRole, creator.CeilingPerms
			case creator.Ceiling != nil:
				var want authz.Set
				if req.Role != "" {
					want, _ = authz.Bundle(req.Role)
				} else {
					want = *ceilingSet("", req.Permissions)
				}
				role, perms = "", want.Intersect(*creator.Ceiling).Names()
			}
		}
		pat, err = s.store.CreateOwnedPAT(r.Context(), store.NewOwnedPAT{
			Name: req.Name, TokenHash: hash, Role: role, Permissions: perms, ExpiresAt: expiresAt,
			Owner: store.PATOwner{Sub: creator.Sub, Email: creator.Email, Claims: creator.Claims},
		})
	}
	if err != nil {
		http.Error(w, "store error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, api.CreatePATResponse{
		ID:          pat.ID,
		Name:        pat.Name,
		Role:        pat.Role,
		Token:       token,
		Permissions: pat.Permissions,
	})
}

// canManageTokens reports whether the caller may list and delete every PAT.
func canManageTokens(p Principal) bool {
	return p.Grants.Can(authz.TokenManage, "")
}

// handleListTokens returns the PATs the caller may see (token hashes are not
// included): all of them with token.manage, otherwise only the caller's own.
func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFromContext(r.Context())
	var pats []store.PAT
	var err error
	switch {
	case canManageTokens(p):
		pats, err = s.store.ListPATs(r.Context())
	case p.Sub == "":
		// No owner identity (role-only PAT): it owns nothing.
	default:
		pats, err = s.store.ListPATsByOwner(r.Context(), p.Sub)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	result := make([]api.PATMeta, 0, len(pats))
	for _, pat := range pats {
		m := api.PATMeta{
			ID:          pat.ID,
			Name:        pat.Name,
			Role:        pat.Role,
			CreatedAt:   pat.CreatedAt,
			ExpiresAt:   pat.ExpiresAt,
			LastUsedAt:  pat.LastUsedAt,
			Permissions: pat.Permissions,
		}
		if pat.Owner != nil {
			m.Owner = pat.Owner.Email
			if m.Owner == "" {
				m.Owner = pat.Owner.Sub
			}
		}
		result = append(result, m)
	}
	writeJSON(w, http.StatusOK, result)
}

// handleDeleteToken revokes the PAT with the given ID: any PAT with
// token.manage, otherwise only one the caller owns (404 for the rest).
func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	p, _ := principalFromContext(r.Context())
	if canManageTokens(p) {
		if err := s.store.DeletePAT(r.Context(), id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	deleted := false
	if p.Sub != "" {
		var err error
		deleted, err = s.store.DeleteOwnedPAT(r.Context(), id, p.Sub)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if !deleted {
		http.Error(w, "token not found", http.StatusNotFound)
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
