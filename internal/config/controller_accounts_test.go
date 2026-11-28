package config

import (
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServiceAccountsConfig(t *testing.T) {
	t.Run("compiles permissions", func(t *testing.T) {
		cfg, err := ControllerEffective(writeControllerConfig(t, `
serviceAccounts:
  ci-deploy:
    permissions:
      global: [viewer]
      folders:
        team-a: developer
`))
		require.NoError(t, err)
		g, ok := cfg.ServiceAccountGrants["ci-deploy"]
		require.True(t, ok)
		assert.True(t, g.Can(authz.RunTrigger, "team-a/x"))
		assert.False(t, g.Can(authz.RunTrigger, "x"))
		assert.NotContains(t, cfg.ServiceAccountGrants, authz.BuiltinAdminAccount)
		assert.True(t, cfg.AdminAccountEnabled())
	})

	t.Run("admin disabled", func(t *testing.T) {
		cfg, err := ControllerEffective(writeControllerConfig(t, "serviceAccounts:\n  admin:\n    enabled: false\n"))
		require.NoError(t, err)
		assert.False(t, cfg.AdminAccountEnabled())
		assert.NotContains(t, cfg.ServiceAccountGrants, authz.BuiltinAdminAccount)
	})

	t.Run("admin permissions rejected", func(t *testing.T) {
		_, err := ControllerEffective(writeControllerConfig(t, "serviceAccounts:\n  admin:\n    permissions:\n      global: [viewer]\n"))
		require.ErrorContains(t, err, `serviceAccounts["admin"]: only "enabled" may be set for the built-in admin account`)
	})

	for _, name := range []string{"Bad_Name", "env:x", "system:x"} {
		t.Run("invalid name "+name, func(t *testing.T) {
			_, err := ControllerEffective(writeControllerConfig(t, "serviceAccounts:\n  \""+name+"\":\n    enabled: true\n"))
			require.ErrorContains(t, err, `serviceAccounts["`+name+`"]: invalid name: must be a DNS label and must not start with "env:" or "system:"`)
		})
	}

	t.Run("permission error", func(t *testing.T) {
		_, err := ControllerEffective(writeControllerConfig(t, "serviceAccounts:\n  ci:\n    permissions:\n      global: [bogus]\n"))
		require.ErrorContains(t, err, `serviceAccounts["ci"].permissions`)
	})

	t.Run("env-only path", func(t *testing.T) {
		t.Setenv("UNIFIED_TOKENS_MAX_LIFETIME", "0")
		cfg, err := ControllerEffective("")
		require.NoError(t, err)
		assert.Equal(t, time.Duration(0), cfg.TokenMaxLifetime())
		assert.Equal(t, 2160*time.Hour, cfg.TokenDefaultLifetime())
		assert.True(t, cfg.AdminAccountEnabled())
		assert.NotNil(t, cfg.ServiceAccountGrants)
	})
}

func TestTokensAndUIConfig(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		cfg, err := ControllerEffective("")
		require.NoError(t, err)
		assert.Equal(t, 2160*time.Hour, cfg.TokenDefaultLifetime())
		assert.Equal(t, 8760*time.Hour, cfg.TokenMaxLifetime())
		assert.False(t, cfg.UI.AllowTokenEntry)
	})

	t.Run("file", func(t *testing.T) {
		cfg, err := ControllerEffective(writeControllerConfig(t, "tokens:\n  defaultLifetime: 24h\n  maxLifetime: 48h\nui:\n  allowTokenEntry: true\n"))
		require.NoError(t, err)
		assert.Equal(t, 24*time.Hour, cfg.TokenDefaultLifetime())
		assert.Equal(t, 48*time.Hour, cfg.TokenMaxLifetime())
		assert.True(t, cfg.UI.AllowTokenEntry)
	})

	t.Run("env", func(t *testing.T) {
		t.Setenv("UNIFIED_TOKENS_DEFAULT_LIFETIME", "10h")
		t.Setenv("UNIFIED_TOKENS_MAX_LIFETIME", "20h")
		t.Setenv("UNIFIED_UI_ALLOW_TOKEN_ENTRY", "true")
		cfg, err := ControllerEffective("")
		require.NoError(t, err)
		assert.Equal(t, 10*time.Hour, cfg.TokenDefaultLifetime())
		assert.Equal(t, 20*time.Hour, cfg.TokenMaxLifetime())
		assert.True(t, cfg.UI.AllowTokenEntry)
	})

	t.Run("default exceeds max", func(t *testing.T) {
		_, err := ControllerEffective(writeControllerConfig(t, "tokens:\n  defaultLifetime: 100h\n  maxLifetime: 50h\n"))
		require.ErrorContains(t, err, "tokens.defaultLifetime must not exceed tokens.maxLifetime")
	})

	t.Run("max zero allows any default", func(t *testing.T) {
		cfg, err := ControllerEffective(writeControllerConfig(t, "tokens:\n  defaultLifetime: 100h\n  maxLifetime: 0\n"))
		require.NoError(t, err)
		assert.Equal(t, time.Duration(0), cfg.TokenMaxLifetime())
	})

	t.Run("bad env", func(t *testing.T) {
		t.Setenv("UNIFIED_TOKENS_DEFAULT_LIFETIME", "soon")
		_, err := ControllerEffective("")
		require.ErrorContains(t, err, "UNIFIED_TOKENS_DEFAULT_LIFETIME")
	})
}
