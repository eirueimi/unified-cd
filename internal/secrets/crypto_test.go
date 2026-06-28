package secrets

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnvelopeEncryptDecrypt(t *testing.T) {
	km := mustNewLocalKeyManager(t)
	plaintext := "my-super-secret-value"
	encDEK, ciphertext, err := Encrypt(context.Background(), km, []byte(plaintext))
	require.NoError(t, err)
	assert.NotEmpty(t, encDEK)
	assert.NotEmpty(t, ciphertext)
	result, err := Decrypt(context.Background(), km, encDEK, ciphertext)
	require.NoError(t, err)
	assert.Equal(t, plaintext, string(result))
}

func TestEnvelopeEncrypt_DifferentNonceEachTime(t *testing.T) {
	km := mustNewLocalKeyManager(t)
	_, c1, _ := Encrypt(context.Background(), km, []byte("val"))
	_, c2, _ := Encrypt(context.Background(), km, []byte("val"))
	assert.NotEqual(t, c1, c2)
}

func mustNewLocalKeyManager(t *testing.T) KeyManager {
	t.Helper()
	km, err := NewLocalKeyManager("0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	require.NoError(t, err)
	return km
}
