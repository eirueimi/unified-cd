package controller

import (
	"context"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/secrets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The defect this fixes: refresh_token was stored in the clear. The test reads
// the raw columns back and asserts the plaintext is absent.
func TestSession_RefreshTokenIsNotStoredInPlaintext(t *testing.T) {
	s, pg := newTestServerWithKM(t)
	ctx := context.Background()
	const plaintextRT = "refresh-token-plaintext-marker"

	sess, err := s.createSessionWithRefreshToken(ctx, "hash-1", "sub-1", "u@example.com", "admin", plaintextRT, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.NotEmpty(t, sess.ID)

	got, err := pg.GetSessionByHash(ctx, "hash-1")
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.NotContains(t, string(got.RefreshTokenCT), plaintextRT)
	assert.NotContains(t, string(got.RefreshTokenDEK), plaintextRT)

	// And it must still be recoverable — refresh tokens are replayed to the
	// IdP, so they are encrypted, not hashed.
	decrypted, err := secrets.Decrypt(ctx, s.km, got.RefreshTokenDEK, got.RefreshTokenCT,
		secrets.SessionRefreshBinding(got.ID))
	require.NoError(t, err)
	assert.Equal(t, plaintextRT, string(decrypted))
}

// A refresh-token blob lifted from one session must not decrypt under another.
func TestSession_RefreshTokenIsBoundToItsSession(t *testing.T) {
	s, pg := newTestServerWithKM(t)
	ctx := context.Background()

	_, err := s.createSessionWithRefreshToken(ctx, "hash-a", "sub-a", "a@example.com", "admin", "token-a", time.Now().Add(time.Hour))
	require.NoError(t, err)
	b, err := s.createSessionWithRefreshToken(ctx, "hash-b", "sub-b", "b@example.com", "admin", "token-b", time.Now().Add(time.Hour))
	require.NoError(t, err)

	stolen, err := pg.GetSessionByHash(ctx, "hash-a")
	require.NoError(t, err)

	_, err = secrets.Decrypt(ctx, s.km, stolen.RefreshTokenDEK, stolen.RefreshTokenCT,
		secrets.SessionRefreshBinding(b.ID))
	require.Error(t, err, "session A's refresh token must not decrypt as session B's")
}
