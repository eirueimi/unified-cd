package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPAT_RoleRoundTrip(t *testing.T) {
	p := NewTestPostgres(t)
	created := createTestPAT(t, p, "dev-token", "hash-dev", "developer", "")
	assert.Equal(t, "developer", created.Role)

	got, err := p.GetPATByHash(t.Context(), "hash-dev")
	require.NoError(t, err)
	assert.Equal(t, "developer", got.Role)
}
