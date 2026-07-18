package secrets

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
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

func TestLocalKeyManager_TagsWrappedKeys(t *testing.T) {
	km, err := NewLocalKeyManager("0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	require.NoError(t, err)

	wrapped, err := km.EncryptKey(context.Background(), []byte("dek"))
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(wrapped, []byte("local:")),
		"wrapped DEKs must identify their provider")
}

func TestLocalKeyManager_RejectsForeignProvider(t *testing.T) {
	km, err := NewLocalKeyManager("0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	require.NoError(t, err)

	_, err = km.DecryptKey(context.Background(), []byte("vault:v1:c29tZS1jaXBoZXJ0ZXh0"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrProviderMismatch), "got %v", err)
	assert.Contains(t, err.Error(), "local")
}
