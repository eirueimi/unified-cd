package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestControllerEffective_RoleMapping(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
oidc:
  issuer: https://dex.example.com
  clientId: unified-cd
  rolesClaim: groups
  roleMap:
    my-org:platform: admin
    my-org:devs: developer
  userMap:
    alice@example.com: admin
  defaultRole: viewer
`), 0o600))

	cfg, err := ControllerEffective(path)
	require.NoError(t, err)
	require.NotNil(t, cfg.OIDC)
	assert.Equal(t, "groups", cfg.OIDC.RolesClaim)
	assert.Equal(t, "admin", cfg.OIDC.RoleMap["my-org:platform"])
	assert.Equal(t, "admin", cfg.OIDC.UserMap["alice@example.com"])
	assert.Equal(t, "viewer", cfg.OIDC.DefaultRole)
}
