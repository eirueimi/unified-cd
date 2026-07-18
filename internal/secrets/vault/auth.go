// Package vault implements a secrets.KeyManager backed by the Transit secrets
// engine of HashiCorp Vault or OpenBao. The key-encryption key never leaves the
// KMS; this controller holds only a revocable credential.
package vault

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
)

// authResult is what every authentication method produces: a usable token and
// what Vault says about its lifetime.
type authResult struct {
	Token     string
	TTL       time.Duration
	Renewable bool
}

// vaultAuth obtains a fresh token. Implementations differ only in how they
// authenticate; the token lifecycle around them is shared, which is what makes
// adding a method (AppRole, cert) one function rather than a second lifecycle.
type vaultAuth interface {
	login(ctx context.Context) (authResult, error)
}

// staticTokenAuth supplies an operator-provided token.
//
// The file is re-read on every login rather than cached, so replacing a rotated
// token on disk takes effect without restarting the controller. That matters
// because a token that is not periodic dies at its max TTL no matter how often
// it is renewed, and must genuinely be replaced.
type staticTokenAuth struct {
	literal string
	file    string
}

func newStaticTokenAuth(literal, file string) (vaultAuth, error) {
	if literal == "" && file == "" {
		return nil, fmt.Errorf("token auth requires a token: set UNIFIED_VAULT_TOKEN_FILE (preferred) or VAULT_TOKEN")
	}
	return &staticTokenAuth{literal: literal, file: file}, nil
}

// newAuth selects an authentication method. An unknown method is a startup
// error rather than a silent fallback: a typo in a security-relevant setting
// must not fail open.
//
// Only the token case is wrapped in selfLookupAuth. A static token is just an
// opaque string with no TTL of its own, so a self-lookup is what fills TTL
// and Renewable in, and it has the useful side effect of making login a
// genuine round trip to Vault, so an unreachable address or a bad token is
// caught at construction instead of surfacing opaquely on first use.
// Kubernetes auth (and any future method) already returns its own TTL from
// its login response, so wrapping it would add a redundant round trip and the
// auth/token/lookup-self capability requirement to a token that never needed
// either.
func newAuth(cfg Config, client *vaultapi.Client) (vaultAuth, error) {
	switch cfg.Auth {
	case "", "token":
		inner, err := newStaticTokenAuth(cfg.Token, cfg.TokenFile)
		if err != nil {
			return nil, err
		}
		return &selfLookupAuth{inner: inner, client: client}, nil
	default:
		return nil, fmt.Errorf("UNIFIED_VAULT_AUTH %q is not supported; supported methods: token", cfg.Auth)
	}
}

// selfLookupAuth wraps another vaultAuth and enriches its authResult with the
// token's real TTL and renewability via a self-lookup call. See newAuth for
// which methods this wraps and why.
type selfLookupAuth struct {
	inner  vaultAuth
	client *vaultapi.Client

	warnOnce sync.Once
}

func (a *selfLookupAuth) login(ctx context.Context) (authResult, error) {
	res, err := a.inner.login(ctx)
	if err != nil {
		return authResult{}, err
	}

	c, err := a.client.Clone()
	if err != nil {
		return authResult{}, fmt.Errorf("vault client clone: %w", err)
	}
	c.SetToken(res.Token)

	secret, err := c.Auth().Token().LookupSelfWithContext(ctx)
	if err != nil {
		if isPermissionDenied(err) {
			// The token reached Vault but lacks `read` on
			// auth/token/lookup-self. That capability is optional: without
			// it we simply cannot learn the token's real TTL/renewability,
			// so we fall back to the pre-decorator result (TTL: 0,
			// Renewable: false). That is harmless for a static token — the
			// lifecycle re-logins on its normal cadence instead of
			// renewing, which for a file-backed token just re-reads the
			// file. Losing renewal is a degradation, not a reason to take
			// the controller down over a policy that was never required
			// before this decorator existed.
			a.warnOnce.Do(func() {
				slog.Warn("vault: token lookup-self denied, renewal disabled for this token; "+
					"grant `read` on auth/token/lookup-self to enable TTL-aware renewal",
					"error", err)
			})
			return res, nil
		}
		// Anything else -- including an unreachable Vault -- stays a hard
		// failure. TestKeyManager_UnreachableAddressFailsFast depends on an
		// unreachable address failing login, and a transient outage and a
		// misconfiguration are indistinguishable at this point anyway.
		return authResult{}, fmt.Errorf("vault token lookup-self: %w", err)
	}
	ttl, err := secret.TokenTTL()
	if err != nil {
		return authResult{}, fmt.Errorf("vault token lookup-self: parse ttl: %w", err)
	}
	renewable, err := secret.TokenIsRenewable()
	if err != nil {
		return authResult{}, fmt.Errorf("vault token lookup-self: parse renewable: %w", err)
	}
	return authResult{Token: res.Token, TTL: ttl, Renewable: renewable}, nil
}

// isPermissionDenied reports whether err is Vault rejecting a request with
// 403, as opposed to the request never reaching Vault at all (an unreachable
// address, a network error). Matching on the typed StatusCode rather than
// error-string content is deliberate: Vault's error text is not a stable
// contract.
func isPermissionDenied(err error) bool {
	var respErr *vaultapi.ResponseError
	return errors.As(err, &respErr) && respErr.StatusCode == http.StatusForbidden
}

func (a *staticTokenAuth) login(_ context.Context) (authResult, error) {
	// A file is preferred over the literal: environment values leak into
	// docker inspect, process listings, crash dumps, and child processes, and
	// the controller spawns git.
	if a.file != "" {
		raw, err := os.ReadFile(a.file)
		if err != nil {
			return authResult{}, fmt.Errorf("read vault token file %s: %w", a.file, err)
		}
		token := strings.TrimSpace(string(raw))
		if token == "" {
			return authResult{}, fmt.Errorf("vault token file %s is empty", a.file)
		}
		// TTL and renewability are unknown until the token is used; the
		// lifecycle manager looks them up with a self-lookup.
		return authResult{Token: token}, nil
	}
	return authResult{Token: a.literal}, nil
}
