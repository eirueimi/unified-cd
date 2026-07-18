package secrets

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// CryptoVersion is the envelope format version written as the first byte of
// both the wrapped DEK and the value ciphertext. Blobs written before this
// format existed carry no version byte and are intentionally unreadable.
const CryptoVersion byte = 0x02

var (
	// ErrUnsupportedVersion means the blob was written by a different format
	// version than this build understands.
	ErrUnsupportedVersion = errors.New("unsupported ciphertext version")
	// ErrBindingMismatch means the ciphertext authenticated correctly as
	// ciphertext but not against the supplied Binding. This is
	// security-relevant: it indicates substitution, tampering, or corruption.
	ErrBindingMismatch = errors.New("ciphertext does not match its binding")
)

// Encrypt performs envelope encryption. It generates a DEK, encrypts the
// plaintext with it under the Binding's canonical encoding as AAD, and wraps
// the DEK with the KeyManager. Both returned blobs are version-prefixed.
func Encrypt(ctx context.Context, km KeyManager, plaintext []byte, b Binding) (encryptedDEK, ciphertext []byte, err error) {
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
	ct, err := aesGCMEncrypt(dek, plaintext, b.canonical())
	if err != nil {
		return nil, nil, fmt.Errorf("encrypt value: %w", err)
	}
	wrapped, err := km.EncryptKey(ctx, dek)
	if err != nil {
		return nil, nil, fmt.Errorf("encrypt dek: %w", err)
	}
	return withVersion(wrapped), withVersion(ct), nil
}

// Decrypt reverses Encrypt. The Binding must match the one used to encrypt or
// the AES-GCM authentication check fails with ErrBindingMismatch.
func Decrypt(ctx context.Context, km KeyManager, encryptedDEK, ciphertext []byte, b Binding) ([]byte, error) {
	wrapped, err := stripVersion(encryptedDEK)
	if err != nil {
		return nil, fmt.Errorf("wrapped dek: %w", err)
	}
	ct, err := stripVersion(ciphertext)
	if err != nil {
		return nil, fmt.Errorf("ciphertext: %w", err)
	}
	dek, err := km.DecryptKey(ctx, wrapped)
	if err != nil {
		return nil, fmt.Errorf("decrypt dek: %w", err)
	}
	defer func() {
		// zero the DEK from memory
		for i := range dek {
			dek[i] = 0
		}
	}()
	plaintext, err := aesGCMDecrypt(dek, ct, b.canonical())
	if err != nil {
		// AES-GCM cannot distinguish a wrong key from wrong AAD; at this point
		// the DEK unwrapped cleanly, so the AAD is the remaining explanation.
		return nil, fmt.Errorf("decrypt %s: %w", b, ErrBindingMismatch)
	}
	return plaintext, nil
}

func withVersion(blob []byte) []byte {
	out := make([]byte, 0, len(blob)+1)
	out = append(out, CryptoVersion)
	return append(out, blob...)
}

func stripVersion(blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, fmt.Errorf("%w: empty blob", ErrUnsupportedVersion)
	}
	if blob[0] != CryptoVersion {
		return nil, fmt.Errorf("%w: got %#x, this build writes %#x", ErrUnsupportedVersion, blob[0], CryptoVersion)
	}
	return blob[1:], nil
}
