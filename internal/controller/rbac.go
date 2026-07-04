package controller

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
	if email != "" {
		if r, ok := m.UserMap[email]; ok {
			return normalizeRole(r)
		}
	}
	if sub != "" {
		if r, ok := m.UserMap[sub]; ok {
			return normalizeRole(r)
		}
	}
	best := ""
	for _, v := range values {
		if r, ok := m.RoleMap[v]; ok && roleRank(r) > roleRank(best) {
			best = r
		}
	}
	if best != "" {
		return best, true
	}
	return normalizeRole(m.DefaultRole)
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
