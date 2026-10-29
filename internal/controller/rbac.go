package controller

import (
	"fmt"
	"net/http"

	"github.com/eirueimi/unified-cd/internal/authz"
)

// roleRanks defines the strict hierarchy viewer < developer < admin.
var roleRanks = map[string]int{"viewer": 1, "developer": 2, "admin": 3}

// roleRank returns the numeric rank of a role, or 0 for unknown/empty.
func roleRank(role string) int { return roleRanks[role] }

// RoleMapping is the OIDC-claim-to-role configuration.
type RoleMapping struct {
	RolesClaim  string
	RoleMap     map[string]string // claim value -> role
	UserMap     map[string]string // email or sub -> role
	DefaultRole string            // "" or "deny" => deny
}

// resolveRole determines a role from claim values and identity.
// Order: userMap(email, then sub) -> roleMap(highest rank) -> defaultRole.
// Returns ("", false) when the outcome is a denial.
func resolveRole(values []string, email, sub string, m RoleMapping) (string, bool) {
	role, _, ok := resolveRoleSource(values, email, sub, m)
	return role, ok
}

// resolveRoleSource is resolveRole plus a description of the config entry that
// decided the outcome: userMap["<email>"], userMap["<sub>"], roleMap["<value>"]
// or defaultRole. The source is empty when the outcome is a denial.
func resolveRoleSource(values []string, email, sub string, m RoleMapping) (role, source string, ok bool) {
	if email != "" {
		if r, found := m.UserMap[email]; found {
			role, ok = normalizeRole(r)
			return role, sourceIf(ok, fmt.Sprintf("userMap[%q]", email)), ok
		}
	}
	if sub != "" {
		if r, found := m.UserMap[sub]; found {
			role, ok = normalizeRole(r)
			return role, sourceIf(ok, fmt.Sprintf("userMap[%q]", sub)), ok
		}
	}
	best, bestValue := "", ""
	for _, v := range values {
		if r, found := m.RoleMap[v]; found && roleRank(r) > roleRank(best) {
			best, bestValue = r, v
		}
	}
	if best != "" {
		return best, fmt.Sprintf("roleMap[%q]", bestValue), true
	}
	role, ok = normalizeRole(m.DefaultRole)
	return role, sourceIf(ok, "defaultRole"), ok
}

func sourceIf(ok bool, source string) string {
	if !ok {
		return ""
	}
	return source
}

// normalizeRole rejects empty, "deny", and unknown roles.
func normalizeRole(r string) (string, bool) {
	if roleRank(r) == 0 { // covers "", "deny", and any unknown string
		return "", false
	}
	return r, true
}

// roleMapping builds a RoleMapping from the server's OIDC config.
func (s *Server) roleMapping() RoleMapping {
	if s.oidcCfg == nil {
		return RoleMapping{}
	}
	return RoleMapping{
		RolesClaim:  s.oidcCfg.RolesClaim,
		RoleMap:     s.oidcCfg.RoleMap,
		UserMap:     s.oidcCfg.UserMap,
		DefaultRole: s.oidcCfg.DefaultRole,
	}
}

// extractRoleValues pulls the configured claim from a decoded claims map as
// a []string. Accepts a string, []string, or []any of strings. Defaults the
// claim name to "groups". Returns nil when the claim is absent.
func extractRoleValues(claims map[string]any, rolesClaim string) []string {
	if rolesClaim == "" {
		rolesClaim = "groups"
	}
	v, ok := claims[rolesClaim]
	if !ok {
		return nil
	}
	switch t := v.(type) {
	case string:
		return []string{t}
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// requireMinRole rejects (403) callers whose role rank is below min.
// Must run inside ServerAuth (which sets the principal + role).
func requireMinRole(min string) func(http.Handler) http.Handler {
	minRank := roleRank(min)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := principalFromContext(r.Context())
			if !ok || roleRank(p.Role) < minRank {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// identityGrants resolves an OIDC identity into its legacy role and effective
// grants: the role bundle from roleMap/userMap/defaultRole united with every
// oidc.permissions entry that matches. admitted is true when the legacy
// resolution succeeded or at least one permissions entry matched; role is ""
// when only permissions matched. A userMap demotion never suppresses matching
// permissions entries.
func (s *Server) identityGrants(values []string, email, sub string) (role string, g authz.Grants, admitted bool) {
	if s == nil {
		return "", authz.Grants{}, false
	}
	role, source, legacyOK := resolveRoleSource(values, email, sub, s.roleMapping())
	if legacyOK {
		g = authz.RoleGrants(role, fmt.Sprintf("role:%s (%s)", role, source))
	}
	var policy *authz.Policy
	if s.oidcCfg != nil {
		policy = s.oidcCfg.Policy
	}
	pg, matched := policy.Match(authz.Identity{Values: values, Email: email, Sub: sub})
	return role, g.Union(pg), legacyOK || matched
}

// ceilingSet returns a PAT's permission ceiling: the explicit permission list
// when there is one, else the role bundle, else nil (no ceiling).
func ceilingSet(role string, perms []string) *authz.Set {
	if perms != nil {
		var set authz.Set
		for _, name := range perms {
			if p, ok := authz.Parse(name); ok {
				set = set.With(p)
			}
		}
		return &set
	}
	if role != "" {
		// An unknown role bundles nothing: fail closed to an empty ceiling.
		set, _ := authz.Bundle(role)
		return &set
	}
	return nil
}
