package secrets

import (
	"context"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLocalKeyManager_EncryptDecrypt(t *testing.T) {
	key := "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"
	km, err := NewLocalKeyManager(key)
	require.NoError(t, err)

	plaintext := []byte("super-secret-dek")
	ciphertext, err := km.EncryptKey(context.Background(), plaintext)
	require.NoError(t, err)
	assert.NotEqual(t, plaintext, ciphertext)

	decrypted, err := km.DecryptKey(context.Background(), ciphertext)
	require.NoError(t, err)
	assert.Equal(t, plaintext, decrypted)
}

func TestLocalKeyManager_InvalidKey(t *testing.T) {
	// valid hex but not 32 bytes (hex representation of 16 bytes)
	_, err := NewLocalKeyManager("0102030405060708090a0b0c0d0e0f10")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "32 bytes")
}

func TestLocalKeyManager_GenerateKey(t *testing.T) {
	key := hex.EncodeToString(GenerateKey())
	assert.Len(t, key, 64)
}
