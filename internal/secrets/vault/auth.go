// Package vault implements a secrets.KeyManager backed by the Transit secrets
// engine of HashiCorp Vault or OpenBao. The key-encryption key never leaves the
// KMS; this controller holds only a revocable credential.
package vault

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
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
