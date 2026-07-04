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
