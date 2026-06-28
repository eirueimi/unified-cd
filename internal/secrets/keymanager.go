package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
)

// KeyManager is the interface for managing key encryption and decryption.
type KeyManager interface {
	EncryptKey(ctx context.Context, plaintext []byte) ([]byte, error)
	DecryptKey(ctx context.Context, ciphertext []byte) ([]byte, error)
}

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

// EncryptKey encrypts a plaintext key.
func (m *LocalKeyManager) EncryptKey(_ context.Context, plaintext []byte) ([]byte, error) {
	return aesGCMEncrypt(m.kek, plaintext)
}

// DecryptKey decrypts an encrypted key.
func (m *LocalKeyManager) DecryptKey(_ context.Context, ciphertext []byte) ([]byte, error) {
	return aesGCMDecrypt(m.kek, ciphertext)
}

// aesGCMEncrypt encrypts plaintext using AES-256-GCM.
func aesGCMEncrypt(key, plaintext []byte) ([]byte, error) {
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
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// aesGCMDecrypt decrypts ciphertext using AES-256-GCM.
func aesGCMDecrypt(key, data []byte) ([]byte, error) {
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
	return gcm.Open(nil, data[:ns], data[ns:], nil)
}
