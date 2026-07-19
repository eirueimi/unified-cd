package vault

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
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

// dekCacheCapacity and dekCacheTTL bound the unwrapped-DEK cache: capacity
// caps memory, and the TTL is what makes a Transit key rotation or revocation
// take effect within a bounded window rather than never.
const (
	dekCacheCapacity = 1024
	dekCacheTTL      = 5 * time.Minute
)

// KeyManager wraps and unwraps DEKs using Vault's Transit engine, so the
// key-encryption key never leaves the KMS.
type KeyManager struct {
	client *vaultapi.Client
	tokens *tokenManager
	mount  string
	key    string
	cache  *dekCache
}

// New constructs a Transit key manager, authenticating immediately and
// performing a Transit round trip so a misconfiguration — a bad credential, a
// missing key, or a policy too narrow to actually use Transit — surfaces at
// startup rather than at the first secret read.
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
		Auth:     auth,
		LoginCtx: ctx,
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
	cache := newDEKCache(dekCacheCapacity, dekCacheTTL, cfg.Now)
	km := &KeyManager{client: client, tokens: tokens, mount: mount, key: cfg.Key, cache: cache}

	if err := km.startupProbe(ctx); err != nil {
		_ = km.Close()
		return nil, err
	}

	return km, nil
}

// startupProbe encrypts and discards a throwaway buffer, proving the
// controller can actually use Transit rather than merely that its credential
// is valid. It needs only the `update` capability the documented policy
// already grants on transit/encrypt/<key>.
//
// Design §5 lists "Transit key missing" and "Permission denied" as startup
// error causes; without this call neither could ever fire, because nothing
// touched transit/encrypt/<key> before the first real secret operation, and a
// controller with a typo'd key name or an under-scoped policy would start
// cleanly and then fail every write.
func (m *KeyManager) startupProbe(ctx context.Context) error {
	_, err := m.write(ctx, "encrypt", map[string]any{
		"plaintext": base64.StdEncoding.EncodeToString([]byte("unified-cd startup probe")),
	})
	if err == nil {
		return nil
	}

	var respErr *vaultapi.ResponseError
	if errors.As(err, &respErr) {
		switch respErr.StatusCode {
		case http.StatusForbidden:
			return fmt.Errorf(
				"vault transit: permission denied encrypting with %s/%s; grant `update` on "+
					"transit/encrypt/%[2]s and transit/decrypt/%[2]s to this token's policy: %w",
				m.mount, m.key, err)
		case http.StatusBadRequest:
			return fmt.Errorf(
				"vault transit: key %q not found on mount %q; create it with "+
					"`vault write -f %s/keys/%s` (OpenBao: `bao write -f %s/keys/%s`): %w",
				m.key, m.mount, m.mount, m.key, m.mount, m.key, err)
		}
	}
	return fmt.Errorf("vault transit: startup probe against %s/%s failed: %w", m.mount, m.key, err)
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

	cacheKey := string(ciphertext)
	if dek, ok := m.cache.get(cacheKey); ok {
		return dek, nil
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
	m.cache.put(cacheKey, dek)
	return dek, nil
}

// write performs one Transit call, and — per design §5 ("Token expired or
// revoked — re-login, then retry the operation once") — retries exactly once
// if the call fails with a 403. That covers an operator revoking this
// controller's token and dropping a replacement into UNIFIED_VAULT_TOKEN_FILE
// (see docs/secrets.md): without the retry, every operation would keep using
// the stale token from tokenManager's cache until the next renewal tick, up
// to half a lease away, even though the new token is already on disk.
//
// Only a 403 triggers a retry, and only once: a persistent denial (a policy
// that genuinely lacks the capability) must still surface as an error rather
// than loop.
func (m *KeyManager) write(ctx context.Context, op string, body map[string]any) (map[string]any, error) {
	token, err := m.tokens.token(ctx)
	if err != nil {
		return nil, err
	}

	out, err := m.doWrite(ctx, op, token, body)
	if err == nil || !isPermissionDenied(err) {
		return out, err
	}

	newToken, reauthErr := m.tokens.reauthenticate(ctx, token)
	if reauthErr != nil {
		return nil, fmt.Errorf("%w (re-login after permission denied also failed: %v)", err, reauthErr)
	}
	return m.doWrite(ctx, op, newToken, body)
}

// doWrite is the single, non-retrying Transit round trip that write wraps.
func (m *KeyManager) doWrite(ctx context.Context, op, token string, body map[string]any) (map[string]any, error) {
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
