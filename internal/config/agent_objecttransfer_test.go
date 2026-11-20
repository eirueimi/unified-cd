package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadAgent_ObjectTransferValues(t *testing.T) {
	t.Setenv("UNIFIED_OBJECT_TRANSFER", "")

	eff, err := AgentEffective(writeAgentYAML(t, "server: https://c.example.com\n"))
	require.NoError(t, err)
	assert.Equal(t, "direct", eff.ObjectTransfer, "default")
	assert.NoError(t, ValidateObjectTransfer(eff.ObjectTransfer))

	eff, err = AgentEffective(writeAgentYAML(t, "objectTransfer: controller\n"))
	require.NoError(t, err)
	assert.Equal(t, "controller", eff.ObjectTransfer)
	assert.NoError(t, ValidateObjectTransfer(eff.ObjectTransfer))

	t.Setenv("UNIFIED_OBJECT_TRANSFER", "controller")
	eff, err = AgentEffective(writeAgentYAML(t, "server: https://c.example.com\n"))
	require.NoError(t, err)
	assert.Equal(t, "controller", eff.ObjectTransfer, "env")

	eff, err = AgentEffective(writeAgentYAML(t, "objectTransfer: bogus\n"))
	require.NoError(t, err)
	err = ValidateObjectTransfer(eff.ObjectTransfer)
	require.Error(t, err)
	assert.Equal(t, `objectTransfer: must be "direct" or "controller", got "bogus"`, err.Error())
}

func TestLoadAgent_OldCacheSettingsReadForRefusal(t *testing.T) {
	t.Setenv("UNIFIED_CACHE_BUCKET", "b")
	eff, err := AgentEffective(writeAgentYAML(t, "cacheEndpoint: minio:9000\n"))
	require.NoError(t, err)
	assert.Equal(t, "minio:9000", eff.CacheEndpoint)
	assert.Equal(t, "b", eff.CacheBucket)
	assert.ErrorIs(t, RefuseOldCacheStoreSettings(eff.CacheEndpoint, eff.CacheKey, eff.CacheSecret, eff.CacheBucket), ErrOldCacheStoreSettings)
	assert.NoError(t, RefuseOldCacheStoreSettings("", "", "", ""))
}
