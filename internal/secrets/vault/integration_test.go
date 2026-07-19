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

	b := secrets.SecretBinding("MY_SECRET", "global", "")
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
