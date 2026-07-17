package agentauth

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateParseAndMatch(t *testing.T) {
	for _, kind := range []TokenKind{AccessToken, RefreshToken, EnrollmentToken} {
		issued, err := Generate(kind)
		require.NoError(t, err)

		parsed, err := Parse(issued.Plaintext, kind)
		require.NoError(t, err)
		assert.Equal(t, issued.ID, parsed.ID)
		require.NoError(t, uuid.Validate(parsed.ID))
		assert.Len(t, parsed.Secret, 32)
		assert.True(t, Matches(issued.Plaintext, issued.Hash))
		assert.False(t, Matches(issued.Plaintext+"x", issued.Hash))
	}
}

func TestParseAcceptsURLSafeBase64Secret(t *testing.T) {
	plaintext := "uca_550e8400-e29b-41d4-a716-446655440000_" +
		base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xff}, 32))

	parsed, err := Parse(plaintext, AccessToken)
	require.NoError(t, err)
	assert.Len(t, parsed.Secret, 32)
}

func TestParseRejectsWrongKind(t *testing.T) {
	issued, err := Generate(AccessToken)
	require.NoError(t, err)

	_, err = Parse(issued.Plaintext, RefreshToken)
	require.ErrorContains(t, err, "unexpected token type")
}

func TestParseRejectsMalformedTokens(t *testing.T) {
	issued, err := Generate(AccessToken)
	require.NoError(t, err)

	parts := strings.Split(issued.Plaintext, "_")
	malformed := []string{
		"uca_not-a-uuid_" + parts[2],
		"uca_" + parts[1] + "_not-valid-base64!",
		"uca_" + parts[1] + "_AQ",
	}
	for _, plaintext := range malformed {
		t.Run(plaintext, func(t *testing.T) {
			_, err := Parse(plaintext, AccessToken)
			require.ErrorContains(t, err, "invalid token format")
		})
	}
}
