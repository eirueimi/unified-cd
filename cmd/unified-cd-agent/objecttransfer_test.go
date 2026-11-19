package main

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/config"
)

const oldCacheFlagsMessage = "agent config: the cache store flags (--cache-endpoint, --cache-key, --cache-secret, --cache-bucket) are no longer supported; caches use controller-issued URLs (see docs/operator-manual/migrations/2026-11-20-run-scoped-object-access.md)"

func TestLoadAgent_OldCacheFlagsRefused(t *testing.T) {
	cases := map[string]struct {
		yaml  string
		env   map[string]string
		flags []string
	}{
		"yaml cacheEndpoint": {yaml: "cacheEndpoint: minio:9000\n"},
		"env bucket":         {env: map[string]string{"UNIFIED_CACHE_BUCKET": "b"}},
		"flag cache-key":     {flags: []string{"--cache-key", "x"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			path := writeAgentConfig(t, "server: https://c.example.com\n"+tc.yaml)
			err := loadAndValidateAgentConfig(path, tc.flags)
			require.Error(t, err)
			assert.Equal(t, oldCacheFlagsMessage, err.Error())
		})
	}
}

func TestLoadAgent_NoOldCacheSettingsPasses(t *testing.T) {
	t.Setenv("UNIFIED_OBJECT_TRANSFER", "")
	path := writeAgentConfig(t, "server: https://c.example.com\n")
	assert.NoError(t, loadAndValidateAgentConfig(path, nil))
}

func TestLoadAgent_ObjectTransferValues(t *testing.T) {
	t.Setenv("UNIFIED_OBJECT_TRANSFER", "")
	assert.NoError(t, loadAndValidateAgentConfig(writeAgentConfig(t, "objectTransfer: controller\n"), nil))
	err := loadAndValidateAgentConfig(writeAgentConfig(t, "objectTransfer: bogus\n"), nil)
	require.Error(t, err)
	assert.Equal(t, `objectTransfer: must be "direct" or "controller", got "bogus"`, err.Error())
	err = loadAndValidateAgentConfig(writeAgentConfig(t, "server: https://c.example.com\n"),[]string{"--object-transfer", "bogus"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `got "bogus"`)
}

// writeAgentConfig writes an agent config file and returns its path.
func writeAgentConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "unified-agent.yaml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

// loadAndValidateAgentConfig runs the same steps main does for the object
// store settings: merge env and file, register the flags, parse, validate.
func loadAndValidateAgentConfig(path string, args []string) error {
	eff, err := config.AgentEffective(path)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	o := registerObjectSettingsFlags(fs, eff)
	if err := fs.Parse(args); err != nil {
		return err
	}
	return o.validate()
}
