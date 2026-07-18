package secrets

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// KeyManager is the interface for managing key encryption and decryption.
type KeyManager interface {
	EncryptKey(ctx context.Context, plaintext []byte) ([]byte, error)
	DecryptKey(ctx context.Context, ciphertext []byte) ([]byte, error)
}

// localKeyPrefix tags DEKs wrapped by LocalKeyManager. Vault Transit already
// self-describes its ciphertext as "vault:v1:…"; matching that convention lets
// a provider mismatch be reported precisely instead of surfacing as an opaque
// AES-GCM authentication failure.
const localKeyPrefix = "local:"

// ErrProviderMismatch means the wrapped DEK was produced by a different key
// provider than the one currently configured.
var ErrProviderMismatch = errors.New("wrapped key was produced by a different key provider")

// LocalKeyManager manages a key encryption key (KEK) using AES-256-GCM.
type LocalKeyManager struct {
	kek []byte
}

// NewLocalKeyManager creates a LocalKeyManager from a hex-encoded master key.
func NewLocalKeyManager(masterKeyHex string) (*LocalKeyManager, error) {
	key, err := hex.DecodeString(masterKeyHex)
	if err != nil {
		return nil, fmt.Errorf("decode master key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("master key must be 32 bytes (64 hex chars), got %d bytes", len(key))
	}
	return &LocalKeyManager{kek: key}, nil
}

// GenerateKey generates a random 32-byte key.
func GenerateKey() []byte {
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		panic("failed to generate random key: " + err.Error())
	}
	return key
}

// EncryptKey encrypts a plaintext key. Key wrapping carries no AAD: the
// binding is applied to the value ciphertext, and the wrapped DEK is
// meaningless on its own.
func (m *LocalKeyManager) EncryptKey(_ context.Context, plaintext []byte) ([]byte, error) {
	ct, err := aesGCMEncrypt(m.kek, plaintext, nil)
	if err != nil {
		return nil, err
	}
	return append([]byte(localKeyPrefix), ct...), nil
}

// DecryptKey decrypts an encrypted key.
func (m *LocalKeyManager) DecryptKey(_ context.Context, ciphertext []byte) ([]byte, error) {
	if !bytes.HasPrefix(ciphertext, []byte(localKeyPrefix)) {
		return nil, fmt.Errorf("%w: this controller is configured for the local key provider", ErrProviderMismatch)
	}
	return aesGCMDecrypt(m.kek, ciphertext[len(localKeyPrefix):], nil)
}

// aesGCMEncrypt encrypts plaintext using AES-256-GCM, authenticating aad.
func aesGCMEncrypt(key, plaintext, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, aad), nil
}

// aesGCMDecrypt decrypts ciphertext using AES-256-GCM, authenticating aad.
func aesGCMDecrypt(key, data, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(data) < ns {
		return nil, fmt.Errorf("ciphertext too short")
	}
	return gcm.Open(nil, data[:ns], data[ns:], aad)
}
