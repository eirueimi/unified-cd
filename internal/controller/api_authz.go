package controller

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/authz"
)

const (
	msgExplainNeedsIdentity = "user or groups is required"
	msgExplainJobAndFolder  = "job and folder are mutually exclusive"

	groupsSourceRequest = "request"
	groupsSourceLogin   = "login"
	groupsSourceNone    = "none"
)

// handleAuthzPolicy returns the authorization policy as resolved at startup
// (spec 5.18). Without OIDC only the built-in bundles are listed.
func (s *Server) handleAuthzPolicy(w http.ResponseWriter, r *http.Request) {
	if s.oidcCfg == nil {
		writeJSON(w, http.StatusOK, authz.NoOIDCPolicy())
		return
	}
	writeJSON(w, http.StatusOK, s.oidcCfg.Policy.Describe(authz.Legacy{
		RolesClaim:  s.oidcCfg.RolesClaim,
		RoleMap:     s.oidcCfg.RoleMap,
		UserMap:     s.oidcCfg.UserMap,
		DefaultRole: s.oidcCfg.DefaultRole,
	}))
}

// handleAuthzExplain answers what an identity may do at a scope, and which
// grants give it each permission. It explains an identity, not a token: a
// PAT's ceiling is not applied. The grants come from identityGrants, the same
// resolution login and audit use, so the grantedBy strings match the audit log.
func (s *Server) handleAuthzExplain(w http.ResponseWriter, r *http.Request) {
	var req api.AuthzExplainRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.User == "" && req.Groups == nil {
		http.Error(w, msgExplainNeedsIdentity, http.StatusBadRequest)
		return
	}
	if req.Job != "" && req.Folder != "" {
		http.Error(w, msgExplainJobAndFolder, http.StatusBadRequest)
		return
	}

	scope := api.AuthzExplainScope{Kind: "global"}
	target := "" // the job the permissions are evaluated on
	switch {
	case req.Job != "":
		scope = api.AuthzExplainScope{Kind: "job", Name: req.Job}
		target = req.Job
	case req.Folder != "":
		folder, err := authz.NormalizeFolder(req.Folder)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		scope = api.AuthzExplainScope{Kind: "folder", Name: folder}
		// Explained as a job directly inside the folder would be.
		target = folder + "/"
	}

	groups, source := []string{}, groupsSourceNone
	switch {
	case req.Groups != nil:
		groups, source = append(groups, *req.Groups...), groupsSourceRequest
	case req.User != "":
		claims, found, err := s.store.LatestSessionClaims(r.Context(), req.User)
		if err != nil {
			slog.Error("authz explain: session claims lookup failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if found {
			groups, source = append(groups, claims...), groupsSourceLogin
		}
	}

	// user is matched as both email and sub, as userMap and users keys are at login.
	role, grants, admitted := s.identityGrants(groups, req.User, req.User)

	resp := api.AuthzExplainResponse{
		Identity:     api.AuthzExplainIdentity{User: req.User, Groups: groups, GroupsSource: source},
		Scope:        scope,
		GlobalRole:   role,
		LoginAllowed: admitted,
		Permissions:  []api.AuthzExplainedPermission{},
	}
	for _, p := range authz.All() {
		if by := grants.Explain(p, target); len(by) > 0 {
			resp.Permissions = append(resp.Permissions, api.AuthzExplainedPermission{Permission: string(p), GrantedBy: by})
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
