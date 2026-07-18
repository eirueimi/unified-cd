package vault

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	vaultapi "github.com/hashicorp/vault/api"

	"github.com/eirueimi/unified-cd/internal/secrets"
)

// vaultCiphertextPrefix is what Transit ciphertext always begins with. Unlike
// LocalKeyManager, this provider does not add a tag on encrypt — Transit's
// output is already self-describing, and prefixing it would yield
// "vault:vault:v1:".
const vaultCiphertextPrefix = "vault:"

// Config describes a Transit-backed key manager. Now and Sleep are optional and
// exist for tests.
type Config struct {
	Address    string
	Mount      string
	Key        string
	Auth       string
	AuthParams map[string]string
	Token      string
	TokenFile  string

	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

// KeyManager wraps and unwraps DEKs using Vault's Transit engine, so the
// key-encryption key never leaves the KMS.
type KeyManager struct {
	client *vaultapi.Client
	tokens *tokenManager
	mount  string
	key    string
}

// New constructs a Transit key manager, authenticating immediately so a
// misconfiguration surfaces at startup rather than at the first secret read.
func New(ctx context.Context, cfg Config) (*KeyManager, error) {
	apiCfg := vaultapi.DefaultConfig()
	apiCfg.Address = cfg.Address
	client, err := vaultapi.NewClient(apiCfg)
	if err != nil {
		return nil, fmt.Errorf("vault client: %w", err)
	}

	auth, err := newAuth(cfg, client)
	if err != nil {
		return nil, err
	}

	tokens, err := newTokenManager(tokenManagerConfig{
		Auth: auth,
		Renew: func(ctx context.Context, token string) (time.Duration, error) {
			c, err := client.Clone()
			if err != nil {
				return 0, err
			}
			c.SetToken(token)
			secret, err := c.Auth().Token().RenewSelfWithContext(ctx, 0)
			if err != nil {
				return 0, err
			}
			return time.Duration(secret.Auth.LeaseDuration) * time.Second, nil
		},
		Now:   cfg.Now,
		Sleep: cfg.Sleep,
	})
	if err != nil {
		return nil, err
	}

	mount := cfg.Mount
	if mount == "" {
		mount = "transit"
	}
	return &KeyManager{client: client, tokens: tokens, mount: mount, key: cfg.Key}, nil
}

// Close stops the background token renewal loop.
func (m *KeyManager) Close() error {
	m.tokens.stop()
	return nil
}

// EncryptKey wraps a DEK with the Transit key.
func (m *KeyManager) EncryptKey(ctx context.Context, plaintext []byte) ([]byte, error) {
	out, err := m.write(ctx, "encrypt", map[string]any{
		"plaintext": base64.StdEncoding.EncodeToString(plaintext),
	})
	if err != nil {
		return nil, err
	}
	ct, ok := out["ciphertext"].(string)
	if !ok {
		return nil, fmt.Errorf("vault transit: encrypt response had no ciphertext")
	}
	return []byte(ct), nil
}

// DecryptKey unwraps a DEK.
func (m *KeyManager) DecryptKey(ctx context.Context, ciphertext []byte) ([]byte, error) {
	// Mirror of LocalKeyManager: report a provider mismatch precisely instead
	// of letting it surface as an opaque decrypt failure.
	if len(ciphertext) < len(vaultCiphertextPrefix) ||
		string(ciphertext[:len(vaultCiphertextPrefix)]) != vaultCiphertextPrefix {
		return nil, fmt.Errorf("%w: this controller is configured for the hashivault key provider", secrets.ErrProviderMismatch)
	}
	out, err := m.write(ctx, "decrypt", map[string]any{"ciphertext": string(ciphertext)})
	if err != nil {
		return nil, err
	}
	b64, ok := out["plaintext"].(string)
	if !ok {
		return nil, fmt.Errorf("vault transit: decrypt response had no plaintext")
	}
	dek, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("vault transit: decode plaintext: %w", err)
	}
	return dek, nil
}

func (m *KeyManager) write(ctx context.Context, op string, body map[string]any) (map[string]any, error) {
	token, err := m.tokens.token(ctx)
	if err != nil {
		return nil, err
	}
	c, err := m.client.Clone()
	if err != nil {
		return nil, err
	}
	c.SetToken(token)
	path := fmt.Sprintf("%s/%s/%s", m.mount, op, m.key)
	secret, err := c.Logical().WriteWithContext(ctx, path, body)
	if err != nil {
		return nil, fmt.Errorf("vault transit %s %s: %w", op, path, err)
	}
	if secret == nil || secret.Data == nil {
		return nil, fmt.Errorf("vault transit %s %s: empty response", op, path)
	}
	return secret.Data, nil
}
