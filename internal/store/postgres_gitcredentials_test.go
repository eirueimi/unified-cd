package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgres_GitCredentialKnownHosts(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	const kh = "gitlab.example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample"

	require.NoError(t, pg.UpsertGitCredential(ctx, "gl", "gitlab.example.com", "sshKey", "gl-key", kh))
	gc, err := pg.GetGitCredentialByHost(ctx, "gitlab.example.com")
	require.NoError(t, err)
	require.NotNil(t, gc)
	assert.Equal(t, kh, gc.KnownHosts)

	// Re-applying without knownHosts clears the pin (declarative semantics).
	require.NoError(t, pg.UpsertGitCredential(ctx, "gl", "gitlab.example.com", "sshKey", "gl-key", ""))
	list, err := pg.ListGitCredentials(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "", list[0].KnownHosts)
}
