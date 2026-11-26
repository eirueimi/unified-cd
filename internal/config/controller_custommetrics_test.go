package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestControllerConfig_CustomMetrics(t *testing.T) {
	t.Setenv("UNIFIED_DEV_MODE", "true")
	for _, k := range []string{"UNIFIED_CUSTOM_METRICS_ENABLED", "UNIFIED_CUSTOM_METRICS_MAX_SERIES_PER_METRIC", "UNIFIED_CUSTOM_METRICS_TTL"} {
		t.Setenv(k, "")
	}

	// Defaults.
	eff, err := config.ControllerEffective("")
	require.NoError(t, err)
	assert.True(t, eff.CustomMetricsEnabled())
	assert.Equal(t, 1000, eff.CustomMetricsMaxSeries())
	assert.Equal(t, 24*time.Hour, eff.CustomMetricsTTL())

	// Environment.
	t.Setenv("UNIFIED_CUSTOM_METRICS_ENABLED", "false")
	t.Setenv("UNIFIED_CUSTOM_METRICS_MAX_SERIES_PER_METRIC", "5")
	t.Setenv("UNIFIED_CUSTOM_METRICS_TTL", "2h")
	eff, err = config.ControllerEffective("")
	require.NoError(t, err)
	assert.False(t, eff.CustomMetricsEnabled())
	assert.Equal(t, 5, eff.CustomMetricsMaxSeries())
	assert.Equal(t, 2*time.Hour, eff.CustomMetricsTTL())

	// The config file wins over the environment, field by field.
	path := filepath.Join(t.TempDir(), "c.yaml")
	require.NoError(t, os.WriteFile(path, []byte("customMetrics:\n  enabled: true\n  ttl: 0s\n"), 0o600))
	eff, err = config.ControllerEffective(path)
	require.NoError(t, err)
	assert.True(t, eff.CustomMetricsEnabled())
	assert.Equal(t, 5, eff.CustomMetricsMaxSeries(), "unset file field keeps the env value")
	assert.Equal(t, time.Duration(0), eff.CustomMetricsTTL(), "an explicit 0 disables expiry")

	// Invalid values are rejected.
	t.Setenv("UNIFIED_CUSTOM_METRICS_TTL", "soon")
	_, err = config.ControllerEffective("")
	require.ErrorContains(t, err, "UNIFIED_CUSTOM_METRICS_TTL")
	t.Setenv("UNIFIED_CUSTOM_METRICS_TTL", "")
	t.Setenv("UNIFIED_CUSTOM_METRICS_MAX_SERIES_PER_METRIC", "-1")
	_, err = config.ControllerEffective("")
	require.ErrorContains(t, err, "UNIFIED_CUSTOM_METRICS_MAX_SERIES_PER_METRIC")
	t.Setenv("UNIFIED_CUSTOM_METRICS_MAX_SERIES_PER_METRIC", "")
	t.Setenv("UNIFIED_CUSTOM_METRICS_ENABLED", "maybe")
	_, err = config.ControllerEffective("")
	require.ErrorContains(t, err, "UNIFIED_CUSTOM_METRICS_ENABLED")
	t.Setenv("UNIFIED_CUSTOM_METRICS_ENABLED", "")

	require.NoError(t, os.WriteFile(path, []byte("customMetrics:\n  maxSeriesPerMetric: -3\n"), 0o600))
	_, err = config.ControllerEffective(path)
	require.ErrorContains(t, err, "customMetrics.maxSeriesPerMetric")
}
