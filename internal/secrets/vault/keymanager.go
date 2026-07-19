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

// startupProbe verifies the controller can actually use Transit, in two
// steps that classify failures cleanly instead of guessing from a single
// encrypt's status code:
//
//  1. probeKeyExists reads the key's own metadata at <mount>/keys/<key>. A
//     read cannot create anything, so a missing key comes back as an
//     unambiguous 404 — unlike an encrypt (see below).
//  2. Only once the key is confirmed to exist, probeEncrypt encrypts and
//     discards a throwaway buffer to prove the `update` capability actually
//     works.
//
// Why the encrypt call alone cannot classify "key missing" vs "permission
// denied": Vault ACL-checks an encrypt against a key that does not yet exist
// as a CreateOperation (the policy framework's existence check runs before
// Transit's own "key not found" handler), so a token holding only `update`
// — exactly the least-privilege policy docs/secrets.md documents — is denied
// by the ACL layer with 403 before Transit ever gets a chance to report 400.
// The 400 branch a naive implementation adds for this is therefore
// unreachable under that policy: a typo'd key name is misreported as
// "permission denied; grant `update`", a capability the operator already
// has. Worse, a token that also holds `create` fares worse still: Transit
// auto-vivifies a key on encrypt when the caller may create one, so the
// probe would pass and silently create the wrong key instead of catching
// the typo — precisely the silent misconfiguration this probe exists to
// prevent.
//
// Design §5 lists "Transit key missing" and "Permission denied" as startup
// error causes; without this call neither could fire correctly, and a
// controller with a typo'd key name or an under-scoped policy would either
// get misdiagnosed or start cleanly and fail on the first real write.
func (m *KeyManager) startupProbe(ctx context.Context) error {
	if err := m.probeKeyExists(ctx); err != nil {
		return err
	}
	return m.probeEncrypt(ctx)
}

// probeKeyExists reads <mount>/keys/<key> and classifies the result.
//
// `read` on this path is a required capability, not an optional one: it is
// what lets the classification below distinguish "your key does not exist"
// from "your policy is wrong" at startup instead of at the first secret
// write. Degrading to an encrypt-only probe when `read` is denied would
// silently resurrect the exact ambiguity — and the auto-vivify hazard —
// this fix exists to remove, so a 403 here is reported as a policy gap to
// close rather than tolerated. docs/secrets.md documents `read` on
// <mount>/keys/<key> alongside the two Transit operations for this reason.
func (m *KeyManager) probeKeyExists(ctx context.Context) error {
	token, err := m.tokens.token(ctx)
	if err != nil {
		return err
	}
	c, err := m.client.Clone()
	if err != nil {
		return err
	}
	c.SetToken(token)

	path := fmt.Sprintf("%s/keys/%s", m.mount, m.key)
	secret, err := c.Logical().ReadWithContext(ctx, path)
	if err != nil {
		var respErr *vaultapi.ResponseError
		if errors.As(err, &respErr) && respErr.StatusCode == http.StatusForbidden {
			return fmt.Errorf(
				"vault transit: permission denied reading %s; grant `read` on %[1]s to this "+
					"token's policy — without it the controller cannot verify the key exists "+
					"before using it: %w",
				path, err)
		}
		return fmt.Errorf("vault transit: checking whether key %q exists on mount %q: %w", m.key, m.mount, err)
	}
	// A missing key surfaces here as (nil, nil), not as an error: the Vault
	// API client special-cases 404 responses and swallows them rather than
	// returning a *vaultapi.ResponseError (see (*Logical).ParseRawResponseAndCloseBody).
	if secret == nil || len(secret.Data) == 0 {
		return fmt.Errorf(
			"vault transit: key %q not found on mount %q; create it with "+
				"`vault write -f %s/keys/%s` (OpenBao: `bao write -f %s/keys/%s`)",
			m.key, m.mount, m.mount, m.key, m.mount, m.key)
	}
	return nil
}

// probeEncrypt encrypts and discards a throwaway buffer, proving the
// `update` capability actually works. By the time this runs, probeKeyExists
// has already confirmed the key exists, so a 403 here means exactly one
// thing: the policy lacks `update` on transit/encrypt/<key>.
func (m *KeyManager) probeEncrypt(ctx context.Context) error {
	_, err := m.write(ctx, "encrypt", map[string]any{
		"plaintext": base64.StdEncoding.EncodeToString([]byte("unified-cd startup probe")),
	})
	if err == nil {
		return nil
	}

	var respErr *vaultapi.ResponseError
	if errors.As(err, &respErr) && respErr.StatusCode == http.StatusForbidden {
		return fmt.Errorf(
			"vault transit: permission denied encrypting with %s/%s; grant `update` on "+
				"transit/encrypt/%[2]s and transit/decrypt/%[2]s to this token's policy: %w",
			m.mount, m.key, err)
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
