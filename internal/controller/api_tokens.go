package controller

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
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

// NewPATToken generates a random PAT value in the same format the API issues.
// The recovery command uses it so it does not duplicate the format.
func NewPATToken() (string, error) { return generatePAT() }

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
		if perm == authz.AgentEnroll {
			// agent.manage may create enrollment tokens too, so it may mint
			// enrollment-only PATs.
			if !creator.Grants.HeldAnywhere(authz.AgentEnroll) && !creator.Grants.HeldAnywhere(authz.AgentManage) {
				return http.StatusBadRequest, fmt.Sprintf("cannot grant permission %q: you do not hold it", name)
			}
			continue
		}
		if !creator.Grants.HeldAnywhere(perm) {
			return http.StatusBadRequest, fmt.Sprintf("cannot grant permission %q: you do not hold it", name)
		}
	}
	return 0, ""
}

// validateEnrollLabels checks the enrollLabels field of a create request
// against its permissions and, for a bounded creator, the creator's own bound.
func validateEnrollLabels(creator Principal, req api.CreatePATRequest) (int, string) {
	if len(req.EnrollLabels) == 0 {
		return 0, ""
	}
	if !slices.Contains(req.Permissions, string(authz.AgentEnroll)) {
		return http.StatusBadRequest, "enrollLabels requires the agent.enroll permission"
	}
	seen := make(map[string]bool, len(req.EnrollLabels))
	for _, l := range req.EnrollLabels {
		if strings.TrimSpace(l) == "" {
			return http.StatusBadRequest, "enrollLabels must not contain empty entries"
		}
		if seen[l] {
			return http.StatusBadRequest, "enrollLabels must not contain duplicates"
		}
		seen[l] = true
	}
	if !creator.Grants.Can(authz.AgentManage, "") {
		// Without agent.manage the creator can only hand out labels inside its own
		// bound; no bound (nil) means no labels, matching handleCreateAgentEnrollment.
		for _, l := range req.EnrollLabels {
			if !slices.Contains(creator.EnrollLabels, l) {
				return http.StatusForbidden, "enrollLabels must be a subset of the creating token's enrollLabels"
			}
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
	if strings.HasPrefix(req.Name, "env:") || strings.HasPrefix(req.Name, "system:") {
		http.Error(w, fmt.Sprintf("token name %q is reserved", req.Name), http.StatusBadRequest)
		return
	}

	creator, _ := principalFromContext(r.Context())
	if !creator.ClaimsKnown {
		// Sessions always set ClaimsKnown, so only a bearer id_token without
		// the configured claim reaches this.
		http.Error(w, fmt.Sprintf("cannot create a token: your OIDC token carries no %q claim; log in again so the token includes it, then retry", s.rolesClaimName()), http.StatusForbidden)
		return
	}
	if creator.AccountID == "" {
		// Every PAT belongs to an account.
		http.Error(w, "cannot create a token: your identity has no account; log in again, then retry", http.StatusForbidden)
		return
	}

	// Resolve the target account: the caller's own unless a service account is
	// named. Naming the caller's own service account is not "another account".
	targetID, targetName := creator.AccountID, creator.AccountName
	targetIsService := creator.AccountKind == store.AccountKindService
	if req.Account != "" && !(targetIsService && req.Account == creator.AccountName) {
		if !creator.Grants.Can(authz.TokenManage, "") && !creator.Grants.Can(authz.ServiceAccountToken, "") {
			http.Error(w, "creating a token for another account requires the token.manage or serviceaccount.token permission", http.StatusForbidden)
			return
		}
		if req.Account == authz.BuiltinAdminAccount {
			http.Error(w, "PATs for the built-in admin account can only be obtained through the initial token or the recovery command", http.StatusBadRequest)
			return
		}
		accts, err := s.store.ListAccounts(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		found := false
		for _, a := range accts {
			if a.Kind == store.AccountKindService && a.Name == req.Account && a.DisabledAt == nil {
				targetID, targetName, targetIsService, found = a.ID, a.Name, true, true
				break
			}
		}
		if !found {
			http.Error(w, fmt.Sprintf("unknown service account %q", req.Account), http.StatusBadRequest)
			return
		}
	}

	expiresAt, status, msg := s.patExpiry(req.ExpiresIn, targetIsService && targetName == authz.BuiltinAdminAccount)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}

	if status, msg := validateEnrollLabels(creator, req); status != 0 {
		http.Error(w, msg, status)
		return
	}

	token, err := generatePAT()
	if err != nil {
		http.Error(w, "token generation error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	hash := HashToken(token)

	var pat *store.PAT
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
		// PAT-from-PAT (any target account): the child is bounded by the parent's ceiling.
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
			parent := *creator.Ceiling
			if parent.Has(authz.AgentManage) {
				// agent.manage implies agent.enroll, as for the owner's own grants.
				parent = parent.With(authz.AgentEnroll)
			}
			role, perms = "", want.Intersect(parent).Names()
		}
	}
	var labels []string
	if slices.Contains(perms, string(authz.AgentEnroll)) {
		labels = req.EnrollLabels
		if len(labels) == 0 {
			// A bounded parent never yields an unbounded child.
			labels = creator.EnrollLabels
		}
	}
	pat, err = s.store.CreatePAT(r.Context(), store.NewPAT{
		Name: req.Name, TokenHash: hash, AccountID: targetID, Role: role, Permissions: perms, EnrollLabels: labels, ExpiresAt: expiresAt, CreatedBy: creator.AuditActorID(),
	})
	if err != nil {
		http.Error(w, "store error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// The audit row's resource is the (non-unique) name; the id ties the
	// "pat:<id>" actor of later rows back to this creation. Never the token.
	setAuditDetails(r, map[string]any{"patId": pat.ID, "name": pat.Name})
	writeJSON(w, http.StatusOK, api.CreatePATResponse{
		ID:           pat.ID,
		Name:         pat.Name,
		Role:         pat.Role,
		Token:        token,
		Permissions:  pat.Permissions,
		EnrollLabels: pat.EnrollLabels,
	})
}

// adminTokenMaxLifetime caps the lifetime of PATs of the built-in admin account.
const adminTokenMaxLifetime = 720 * time.Hour

// patExpiry resolves a create request's expiresIn against the lifetime policy.
// An empty value takes tokens.defaultLifetime (0 = no expiry). The built-in
// admin account is always capped at adminTokenMaxLifetime. A non-zero status
// means the request is refused with msg.
func (s *Server) patExpiry(expiresIn string, admin bool) (*time.Time, int, string) {
	var d time.Duration
	if expiresIn != "" {
		var err error
		d, err = time.ParseDuration(expiresIn)
		if err != nil {
			return nil, http.StatusBadRequest, "invalid expiresIn: " + err.Error()
		}
		if d <= 0 {
			return nil, http.StatusBadRequest, "invalid expiresIn: must be positive"
		}
		if admin && d > adminTokenMaxLifetime {
			return nil, http.StatusBadRequest, "expiresIn exceeds the 720h limit for the built-in admin account"
		}
		if max := s.cfg.TokenMaxLifetime; max != 0 && d > max {
			return nil, http.StatusBadRequest, fmt.Sprintf("expiresIn exceeds tokens.maxLifetime (%s)", max)
		}
	} else {
		d = s.cfg.TokenDefaultLifetime
		if admin && (d == 0 || d > adminTokenMaxLifetime) {
			d = adminTokenMaxLifetime
		}
	}
	if d == 0 {
		return nil, 0, ""
	}
	t := time.Now().Add(d)
	return &t, 0, ""
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
	case p.AccountID == "":
		// No account: it owns nothing.
	default:
		pats, err = s.store.ListPATsByAccount(r.Context(), p.AccountID)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Label each PAT with its account: a human's email (else sub), "sa:<name>" for
	// a service account. PATMeta.Owner carries the same label for the web UI.
	accountLabels := map[string]string{}
	if len(pats) > 0 {
		accts, err := s.store.ListAccounts(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, a := range accts {
			accountLabel := "sa:" + a.Name
			if a.Kind == store.AccountKindHuman {
				accountLabel = a.Email
				if accountLabel == "" {
					accountLabel = a.OIDCSub
				}
			}
			accountLabels[a.ID] = accountLabel
		}
	}
	result := make([]api.PATMeta, 0, len(pats))
	for _, pat := range pats {
		m := api.PATMeta{
			ID:           pat.ID,
			Name:         pat.Name,
			Role:         pat.Role,
			CreatedAt:    pat.CreatedAt,
			ExpiresAt:    pat.ExpiresAt,
			LastUsedAt:   pat.LastUsedAt,
			Permissions:  pat.Permissions,
			EnrollLabels: pat.EnrollLabels,
			CreatedBy:    pat.CreatedBy,
		}
		m.Account = accountLabels[pat.AccountID]
		m.Owner = m.Account
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
		if err := s.store.DeletePAT(r.Context(), id); errors.Is(err, store.ErrPATNotFound) {
			http.Error(w, "token not found", http.StatusNotFound)
			return
		} else if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	deleted := false
	if p.AccountID != "" {
		var err error
		deleted, err = s.store.DeleteAccountPAT(r.Context(), id, p.AccountID)
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
		// True when the controller relays the CLI logins; the secret itself is never returned.
		"deviceRelay": s.oidcCfg.DeviceClientSecret != "",
		// The scopes the CLI device flow must request (shared with the web login).
		"scopes": oidcScopes(s.oidcCfg.RolesClaim, s.oidcCfg.Scopes),
	}
	// Fetch the actual endpoints via discovery (Dex device authorization is at /device/code) and return them.
	// When IssuerInternal is set, discovery is performed via the internal network.
	if _, provider, oauth2cfg, err := s.oidcProvider(r.Context(), r.Host); err == nil {
		resp["deviceAuthEndpoint"] = oauth2cfg.Endpoint.DeviceAuthURL
		resp["tokenEndpoint"] = oauth2cfg.Endpoint.TokenURL
		var disc struct {
			RevocationEndpoint string `json:"revocation_endpoint"`
		}
		if provider.Claims(&disc) == nil && disc.RevocationEndpoint != "" {
			resp["revocationEndpoint"] = disc.RevocationEndpoint
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
