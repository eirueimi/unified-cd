package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestEnvIntOr covers the malformed-value warning path added for review
// finding M10: envIntOr runs at flag-registration time, before the slog
// default logger exists, so it can't log directly — it writes a message into
// the caller-supplied *string instead, for the caller to log once the logger
// is ready.
func TestEnvIntOr(t *testing.T) {
	t.Run("unset falls back to default with no warning", func(t *testing.T) {
		t.Setenv("UNIFIED_TEST_ENVINTOR", "")
		var warning string
		got := envIntOr("UNIFIED_TEST_ENVINTOR", 64, &warning)
		assert.Equal(t, 64, got)
		assert.Empty(t, warning)
	})

	t.Run("valid value parses with no warning", func(t *testing.T) {
		t.Setenv("UNIFIED_TEST_ENVINTOR", "128")
		var warning string
		got := envIntOr("UNIFIED_TEST_ENVINTOR", 64, &warning)
		assert.Equal(t, 128, got)
		assert.Empty(t, warning)
	})

	t.Run("malformed value falls back to default and records a warning", func(t *testing.T) {
		t.Setenv("UNIFIED_TEST_ENVINTOR", "not-a-number")
		var warning string
		got := envIntOr("UNIFIED_TEST_ENVINTOR", 64, &warning)
		assert.Equal(t, 64, got, "malformed value should fall back to default")
		assert.NotEmpty(t, warning, "malformed value should record a warning message")
		assert.Contains(t, warning, "UNIFIED_TEST_ENVINTOR")
		assert.Contains(t, warning, "not-a-number")
	})

	t.Run("nil warning pointer is safe", func(t *testing.T) {
		t.Setenv("UNIFIED_TEST_ENVINTOR", "not-a-number")
		got := envIntOr("UNIFIED_TEST_ENVINTOR", 64, nil)
		assert.Equal(t, 64, got)
	})
}
