package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolveRole(t *testing.T) {
	m := RoleMapping{
		RolesClaim: "groups",
		RoleMap: map[string]string{
			"my-org:platform":   "admin",
			"my-org:developers": "developer",
			"my-org:viewers":    "viewer",
		},
		UserMap:     map[string]string{"alice@example.com": "admin"},
		DefaultRole: "viewer",
	}

	role, ok := resolveRole([]string{"my-org:viewers"}, "alice@example.com", "sub-a", m)
	assert.True(t, ok)
	assert.Equal(t, "admin", role)

	role, ok = resolveRole([]string{"my-org:viewers", "my-org:developers"}, "bob@example.com", "sub-b", m)
	assert.True(t, ok)
	assert.Equal(t, "developer", role)

	role, ok = resolveRole([]string{"unknown"}, "carol@example.com", "sub-c", m)
	assert.True(t, ok)
	assert.Equal(t, "viewer", role)

	mDeny := m
	mDeny.DefaultRole = "deny"
	_, ok = resolveRole([]string{"unknown"}, "dan@example.com", "sub-d", mDeny)
	assert.False(t, ok)

	role, ok = resolveRole(nil, "", "sub-a", RoleMapping{UserMap: map[string]string{"sub-a": "developer"}, DefaultRole: "deny"})
	assert.True(t, ok)
	assert.Equal(t, "developer", role)
}

func TestRoleRank(t *testing.T) {
	assert.Equal(t, 1, roleRank("viewer"))
	assert.Equal(t, 2, roleRank("developer"))
	assert.Equal(t, 3, roleRank("admin"))
	assert.Equal(t, 0, roleRank("nonsense"))
	assert.Equal(t, 0, roleRank(""))
}

func TestExtractRoleValues(t *testing.T) {
	assert.Equal(t, []string{"a", "b"},
		extractRoleValues(map[string]any{"groups": []any{"a", "b"}}, "groups"))
	assert.Equal(t, []string{"admin"},
		extractRoleValues(map[string]any{"roles": "admin"}, "roles"))
	assert.Nil(t, extractRoleValues(map[string]any{}, "groups"))
	assert.Equal(t, []string{"x"},
		extractRoleValues(map[string]any{"groups": []any{"x"}}, ""))
	assert.Equal(t, []string{"admin"},
		extractRoleValues(map[string]any{"https://unified-cd/roles": []any{"admin"}}, "https://unified-cd/roles"))
}
