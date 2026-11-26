package config_test

import (
	"testing"

	"github.com/eirueimi/unified-cd/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestControllerConfig_ExternalURLFallback(t *testing.T) {
	t.Run("env only", func(t *testing.T) {
		t.Setenv("UNIFIED_EXTERNAL_URL", "https://env.example.com")
		eff, err := config.ControllerEffective("")
		require.NoError(t, err)
		assert.Equal(t, "https://env.example.com", eff.ExternalURL)
	})
	t.Run("file overrides env", func(t *testing.T) {
		t.Setenv("UNIFIED_EXTERNAL_URL", "https://env.example.com")
		eff, err := config.ControllerEffective(writeFile(t, "externalUrl: https://file.example.com\n"))
		require.NoError(t, err)
		assert.Equal(t, "https://file.example.com", eff.ExternalURL)
	})
	t.Run("falls back to oidc externalUrl", func(t *testing.T) {
		eff, err := config.ControllerEffective(writeFile(t, "oidc:\n  issuer: https://idp.example.com\n  clientId: c\n  externalUrl: https://oidc.example.com\n"))
		require.NoError(t, err)
		assert.Equal(t, "https://oidc.example.com", eff.ExternalURL)
	})
	t.Run("top-level wins over oidc", func(t *testing.T) {
		eff, err := config.ControllerEffective(writeFile(t, "externalUrl: https://top.example.com\noidc:\n  issuer: https://idp.example.com\n  clientId: c\n  externalUrl: https://oidc.example.com\n"))
		require.NoError(t, err)
		assert.Equal(t, "https://top.example.com", eff.ExternalURL)
	})
	t.Run("unset", func(t *testing.T) {
		eff, err := config.ControllerEffective("")
		require.NoError(t, err)
		assert.Empty(t, eff.ExternalURL)
	})
}
