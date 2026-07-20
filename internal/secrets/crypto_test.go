package secrets

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnvelopeEncryptDecrypt(t *testing.T) {
	km := mustNewLocalKeyManager(t)
	b := SecretBinding("MY_SECRET")
	plaintext := "my-super-secret-value"
	encDEK, ciphertext, err := Encrypt(context.Background(), km, []byte(plaintext), b)
	require.NoError(t, err)
	assert.NotEmpty(t, encDEK)
	assert.NotEmpty(t, ciphertext)
	result, err := Decrypt(context.Background(), km, encDEK, ciphertext, b)
	require.NoError(t, err)
	assert.Equal(t, plaintext, string(result))
}

func TestEnvelopeEncrypt_DifferentNonceEachTime(t *testing.T) {
	km := mustNewLocalKeyManager(t)
	b := SecretBinding("v")
	_, c1, err := Encrypt(context.Background(), km, []byte("val"), b)
	require.NoError(t, err)
	_, c2, err := Encrypt(context.Background(), km, []byte("val"), b)
	require.NoError(t, err)
	assert.NotEqual(t, c1, c2)
}

// The central property of this design: a ciphertext moved to a different
// identity must fail, not decrypt into the wrong secret's value.
func TestEnvelopeDecrypt_WrongBindingFails(t *testing.T) {
	km := mustNewLocalKeyManager(t)
	stored := SecretBinding("staging-token")
	attacker := SecretBinding("prod-token")

	encDEK, ciphertext, err := Encrypt(context.Background(), km, []byte("s3cr3t"), stored)
	require.NoError(t, err)

	_, err = Decrypt(context.Background(), km, encDEK, ciphertext, attacker)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrBindingMismatch), "got %v", err)
}

func TestEnvelopeEncrypt_EmitsVersionByte(t *testing.T) {
	km := mustNewLocalKeyManager(t)
	b := SecretBinding("v")
	encDEK, ciphertext, err := Encrypt(context.Background(), km, []byte("x"), b)
	require.NoError(t, err)
	assert.Equal(t, CryptoVersion, ciphertext[0])
	assert.Equal(t, CryptoVersion, encDEK[0])
}

func TestEnvelopeDecrypt_UnknownVersionRejected(t *testing.T) {
	km := mustNewLocalKeyManager(t)
	b := SecretBinding("v")
	encDEK, ciphertext, err := Encrypt(context.Background(), km, []byte("x"), b)
	require.NoError(t, err)

	tampered := append([]byte{}, ciphertext...)
	tampered[0] = 0x7f
	_, err = Decrypt(context.Background(), km, encDEK, tampered, b)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnsupportedVersion), "got %v", err)
}

func mustNewLocalKeyManager(t *testing.T) KeyManager {
	t.Helper()
	km, err := NewLocalKeyManager("0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	require.NoError(t, err)
	return km
}
