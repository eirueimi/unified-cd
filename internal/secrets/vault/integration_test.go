package vault

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	vaultapi "github.com/hashicorp/vault/api"

	"github.com/eirueimi/unified-cd/internal/secrets"
)

const openBaoRootToken = "dev-root-token"

var (
	sharedBaoOnce sync.Once
	sharedBaoAddr string
	sharedBaoErr  error
	testKeySeq    atomic.Int64
)

func startSharedOpenBao() {
	pool, err := dockertest.NewPool("")
	if err != nil {
		sharedBaoErr = fmt.Errorf("dockertest pool: %w", err)
		return
	}
	resource, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository: "openbao/openbao",
		Tag:        "latest",
		Env: []string{
			"BAO_DEV_ROOT_TOKEN_ID=" + openBaoRootToken,
			"BAO_DEV_LISTEN_ADDRESS=0.0.0.0:8200",
		},
		Cmd: []string{"server", "-dev"},
	}, func(c *docker.HostConfig) {
		c.AutoRemove = true
		c.RestartPolicy = docker.RestartPolicy{Name: "no"}
	})
	if err != nil {
		sharedBaoErr = fmt.Errorf("dockertest run: %w", err)
		return
	}
	// The container must outlive individual tests; Expire is the safety net
	// against a leaked container if the test process dies uncleanly.
	_ = resource.Expire(900)

	addr := "http://localhost:" + resource.GetPort("8200/tcp")
	pool.MaxWait = 60 * time.Second
	if err := pool.Retry(func() error { return pingBao(addr) }); err != nil {
		sharedBaoErr = fmt.Errorf("openbao not ready: %w", err)
		return
	}
	if err := enableTransit(addr); err != nil {
		sharedBaoErr = fmt.Errorf("enable transit: %w", err)
		return
	}
	sharedBaoAddr = addr
}

// newTestTransitKey returns an address and a freshly created key name, so tests
// do not share key state.
func newTestTransitKey(t *testing.T) (addr, key string) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	sharedBaoOnce.Do(startSharedOpenBao)
	if sharedBaoErr != nil {
		t.Fatalf("shared openbao: %v", sharedBaoErr)
	}
	key = fmt.Sprintf("test_%d", testKeySeq.Add(1))
	require.NoError(t, createTransitKey(sharedBaoAddr, key))
	return sharedBaoAddr, key
}

func TestIntegration_TransitRoundTrip(t *testing.T) {
	addr, key := newTestTransitKey(t)
	m, err := New(context.Background(), Config{
		Address: addr, Mount: "transit", Key: key,
		Auth: "token", Token: openBaoRootToken,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = m.Close() })

	dek := []byte("0123456789abcdef0123456789abcdef")
	wrapped, err := m.EncryptKey(context.Background(), dek)
	require.NoError(t, err)
	got, err := m.DecryptKey(context.Background(), wrapped)
	require.NoError(t, err)
	assert.Equal(t, dek, got)
}

// The whole point of the change: a DEK wrapped by Transit round-trips through
// the real envelope layer that stores secrets.
func TestIntegration_EnvelopeThroughTransit(t *testing.T) {
	addr, key := newTestTransitKey(t)
	m, err := New(context.Background(), Config{
		Address: addr, Mount: "transit", Key: key,
		Auth: "token", Token: openBaoRootToken,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = m.Close() })

	b := secrets.SecretBinding("MY_SECRET")
	encDEK, ct, err := secrets.Encrypt(context.Background(), m, []byte("hunter2"), b)
	require.NoError(t, err)
	plain, err := secrets.Decrypt(context.Background(), m, encDEK, ct, b)
	require.NoError(t, err)
	assert.Equal(t, "hunter2", string(plain))
}

func baoClient(addr string) (*vaultapi.Client, error) {
	cfg := vaultapi.DefaultConfig()
	cfg.Address = addr
	c, err := vaultapi.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	c.SetToken(openBaoRootToken)
	return c, nil
}

func pingBao(addr string) error {
	c, err := baoClient(addr)
	if err != nil {
		return err
	}
	health, err := c.Sys().Health()
	if err != nil {
		return err
	}
	if !health.Initialized || health.Sealed {
		return fmt.Errorf("openbao not ready: initialized=%v sealed=%v", health.Initialized, health.Sealed)
	}
	return nil
}

func enableTransit(addr string) error {
	c, err := baoClient(addr)
	if err != nil {
		return err
	}
	err = c.Sys().Mount("transit", &vaultapi.MountInput{Type: "transit"})
	if err != nil && strings.Contains(err.Error(), "path is already in use") {
		return nil // idempotent: the shared container may already have it
	}
	return err
}

func createTransitKey(addr, name string) error {
	c, err := baoClient(addr)
	if err != nil {
		return err
	}
	_, err = c.Logical().Write("transit/keys/"+name, map[string]any{})
	return err
}

// leastPrivilegePolicyHCL is exactly the capabilities docs/secrets.md
// documents as required for a Transit-backed controller token: `read` on the
// key's own metadata (the capability this fix adds), and `update` on
// encrypt/decrypt. `auth/token/renew-self` is included too since the docs
// list it as required, not optional, even though a short construction-only
// test never reaches renewal. Notably absent: `create` on anything — a
// least-privilege token must never be able to auto-vivify a key.
func leastPrivilegePolicyHCL(mount, key string) string {
	return fmt.Sprintf(`
path "%[1]s/keys/%[2]s" {
  capabilities = ["read"]
}
path "%[1]s/encrypt/%[2]s" {
  capabilities = ["update"]
}
path "%[1]s/decrypt/%[2]s" {
  capabilities = ["update"]
}
path "auth/token/renew-self" {
  capabilities = ["update"]
}
`, mount, key)
}

// createLeastPrivilegeToken writes a policy scoped to exactly the documented
// capabilities for mount/key and mints a token bound to it with
// NoDefaultPolicy so nothing beyond that policy leaks in (Vault's built-in
// `default` policy grants renew-self and a few other things a real
// least-privilege deployment would not rely on).
func createLeastPrivilegeToken(addr, policyName, mount, key string) (string, error) {
	c, err := baoClient(addr)
	if err != nil {
		return "", err
	}
	if err := c.Sys().PutPolicy(policyName, leastPrivilegePolicyHCL(mount, key)); err != nil {
		return "", fmt.Errorf("put policy %s: %w", policyName, err)
	}
	secret, err := c.Auth().Token().Create(&vaultapi.TokenCreateRequest{
		Policies:        []string{policyName},
		NoDefaultPolicy: true,
		TTL:             "10m",
	})
	if err != nil {
		return "", fmt.Errorf("create token for policy %s: %w", policyName, err)
	}
	if secret == nil || secret.Auth == nil || secret.Auth.ClientToken == "" {
		return "", fmt.Errorf("create token for policy %s: empty response", policyName)
	}
	return secret.Auth.ClientToken, nil
}

// TestIntegration_LeastPrivilegeTokenCatchesMissingKey is the test the
// original startup probe lacked: every other integration test in this file
// authenticates with the OpenBao dev-mode root token, which can read, write,
// and create anything, so none of them could ever exercise what a real
// least-privilege deployment sees. A root token sails through the old
// encrypt-only probe on a missing key too, by auto-creating it — the exact
// silent misconfiguration this fix exists to prevent.
//
// This test mints a token scoped to precisely the policy docs/secrets.md
// documents (see leastPrivilegePolicyHCL — update on encrypt/decrypt, read
// on the key's own metadata, no create) and points a KeyManager at a key
// name that was never created. Under the old status-code-based
// classification this token's encrypt attempt would fail its ACL check as a
// CreateOperation and come back 403 — "permission denied" — even though the
// token already has every capability the documented policy grants. With the
// fix, the key-existence read reports an unambiguous 404 before encrypt is
// ever attempted.
func TestIntegration_LeastPrivilegeTokenCatchesMissingKey(t *testing.T) {
	addr, _ := newTestTransitKey(t) // brings up shared OpenBao; the returned key is unused here
	missingKey := fmt.Sprintf("does-not-exist-%d", testKeySeq.Add(1))
	policyName := fmt.Sprintf("unified-cd-least-priv-%d", testKeySeq.Add(1))

	token, err := createLeastPrivilegeToken(addr, policyName, "transit", missingKey)
	require.NoError(t, err)

	_, err = New(context.Background(), Config{
		Address: addr, Mount: "transit", Key: missingKey,
		Auth: "token", Token: token,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), missingKey, "the error must name the missing key")
	assert.Contains(t, err.Error(), fmt.Sprintf("vault write -f transit/keys/%s", missingKey),
		"the error must give the exact command to create the missing key, not report permission denied")

	// Guard against the auto-vivify hazard directly: prove the key still
	// does not exist. If the old encrypt-first probe (or a regression of
	// this fix) had run against a token that could create, this would fail.
	root, err := baoClient(addr)
	require.NoError(t, err)
	secret, err := root.Logical().Read("transit/keys/" + missingKey)
	require.NoError(t, err)
	assert.Nil(t, secret, "the probe must never create the key it is checking for")
}

// TestIntegration_LeastPrivilegeTokenRoundTrips is the positive-path
// complement to TestIntegration_LeastPrivilegeTokenCatchesMissingKey: with a
// real key and the same least-privilege policy (read on the key, update on
// encrypt/decrypt, nothing more), construction and a full encrypt/decrypt
// round trip must succeed. This is what proves the added `read` requirement
// does not overreach — the documented policy is still sufficient for normal
// operation, not just for startup.
func TestIntegration_LeastPrivilegeTokenRoundTrips(t *testing.T) {
	addr, key := newTestTransitKey(t)
	policyName := fmt.Sprintf("unified-cd-least-priv-%d", testKeySeq.Add(1))

	token, err := createLeastPrivilegeToken(addr, policyName, "transit", key)
	require.NoError(t, err)

	m, err := New(context.Background(), Config{
		Address: addr, Mount: "transit", Key: key,
		Auth: "token", Token: token,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = m.Close() })

	dek := []byte("0123456789abcdef0123456789abcdef")
	wrapped, err := m.EncryptKey(context.Background(), dek)
	require.NoError(t, err)
	got, err := m.DecryptKey(context.Background(), wrapped)
	require.NoError(t, err)
	assert.Equal(t, dek, got)
}
