package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPAT_RoleRoundTrip(t *testing.T) {
	p := NewTestPostgres(t)
	created, err := p.CreatePAT(t.Context(), "dev-token", "hash-dev", "developer", nil)
	require.NoError(t, err)
	assert.Equal(t, "developer", created.Role)

	got, err := p.GetPATByHash(t.Context(), "hash-dev")
	require.NoError(t, err)
	assert.Equal(t, "developer", got.Role)
}

func TestUpsertBootstrapPAT_IsAdmin(t *testing.T) {
	p := NewTestPostgres(t)
	pat, err := p.UpsertBootstrapPAT(t.Context(), "env:UNIFIED_TOKEN", "hash-boot")
	require.NoError(t, err)
	assert.Equal(t, "admin", pat.Role)
}
