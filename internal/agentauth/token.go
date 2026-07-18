package agentauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

type TokenKind string

const (
	AccessToken     TokenKind = "uca"
	RefreshToken    TokenKind = "ucr"
	EnrollmentToken TokenKind = "uce"
)

type IssuedToken struct{ ID, Plaintext, Hash string }
type ParsedToken struct{ ID, Secret string }

// Generate creates an opaque token with a random 32-byte secret.
func Generate(kind TokenKind) (IssuedToken, error) {
	if !validKind(kind) {
		return IssuedToken{}, fmt.Errorf("invalid token type %q", kind)
	}

	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return IssuedToken{}, fmt.Errorf("generate token secret: %w", err)
	}

	id := uuid.NewString()
	plaintext := string(kind) + "_" + id + "_" + base64.RawURLEncoding.EncodeToString(secret)
	return IssuedToken{ID: id, Plaintext: plaintext, Hash: Hash(plaintext)}, nil
}

// Parse validates an opaque token and returns its lookup ID and decoded secret.
func Parse(plaintext string, kind TokenKind) (ParsedToken, error) {
	parts := strings.SplitN(plaintext, "_", 3)
	if len(parts) != 3 {
		return ParsedToken{}, errors.New("invalid token format")
	}
	if parts[0] != string(kind) {
		return ParsedToken{}, errors.New("unexpected token type")
	}
	if _, err := uuid.Parse(parts[1]); err != nil {
		return ParsedToken{}, errors.New("invalid token format")
	}
	secret, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(secret) != 32 {
		return ParsedToken{}, errors.New("invalid token format")
	}
	return ParsedToken{ID: parts[1], Secret: string(secret)}, nil
}

// Hash returns the SHA-256 hex digest of a token.
func Hash(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// Matches reports whether plaintext has the expected SHA-256 hash.
func Matches(plaintext, hash string) bool {
	actual, err := hex.DecodeString(Hash(plaintext))
	if err != nil {
		return false
	}
	expected, err := hex.DecodeString(hash)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func validKind(kind TokenKind) bool {
	return kind == AccessToken || kind == RefreshToken || kind == EnrollmentToken
}
