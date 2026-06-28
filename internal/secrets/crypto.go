package secrets

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
)

// Encrypt performs envelope encryption.
// It generates a DEK (data encryption key), encrypts the plaintext with it,
// and then encrypts the DEK itself with the master key.
func Encrypt(ctx context.Context, km KeyManager, plaintext []byte) (encryptedDEK, ciphertext []byte, err error) {
	dek := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return nil, nil, fmt.Errorf("generate dek: %w", err)
	}
	// Always zero the DEK — even on error paths
	defer func() {
		for i := range dek {
			dek[i] = 0
		}
	}()
	ct, err := aesGCMEncrypt(dek, plaintext)
	if err != nil {
		return nil, nil, fmt.Errorf("encrypt value: %w", err)
	}
	encDEK, err := km.EncryptKey(ctx, dek)
	if err != nil {
		return nil, nil, fmt.Errorf("encrypt dek: %w", err)
	}
	return encDEK, ct, nil
}

// Decrypt decrypts envelope-encrypted data.
// It first decrypts the DEK with the master key, then uses that DEK to decrypt the plaintext.
func Decrypt(ctx context.Context, km KeyManager, encryptedDEK, ciphertext []byte) ([]byte, error) {
	dek, err := km.DecryptKey(ctx, encryptedDEK)
	if err != nil {
		return nil, fmt.Errorf("decrypt dek: %w", err)
	}
	defer func() {
		// zero the DEK from memory
		for i := range dek {
			dek[i] = 0
		}
	}()
	plaintext, err := aesGCMDecrypt(dek, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("decrypt value: %w", err)
	}
	return plaintext, nil
}
