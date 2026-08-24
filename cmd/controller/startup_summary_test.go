package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func lost(t *testing.T, caps []capabilityState, name string) string {
	t.Helper()
	for _, c := range caps {
		if c.Name == name {
			return c.Lost
		}
	}
	t.Fatalf("capability %q not present in summary", name)
	return ""
}

func TestSummarizeStartupReportsNothingLostWhenFullyConfigured(t *testing.T) {
	caps := summarizeStartup(startupInputs{
		ObjectStore: "s3",
		KeyDesc:     "key file /etc/unified-cd/kek",
		OIDC:        true,
		WebUI:       true,
		LogTrimDays: 30,
	})

	for _, c := range caps {
		assert.Empty(t, c.Lost, "capability %q should not be degraded", c.Name)
	}
}

func TestSummarizeStartupNamesWhatEachDegradedCapabilityCosts(t *testing.T) {
	caps := summarizeStartup(startupInputs{
		ObjectStore:  "none",
		KeyDesc:      "ephemeral development key",
		KeyEphemeral: true,
		OIDC:         false,
		WebUI:        false,
		LogTrimDays:  0,
	})

	assert.Contains(t, lost(t, caps, "objectStore"), "log archival and artifacts")
	assert.Contains(t, lost(t, caps, "objectStore"), "UNIFIED_S3_ENDPOINT")
	assert.Contains(t, lost(t, caps, "secretKey"), "unreadable after a restart")
	assert.Contains(t, lost(t, caps, "secretKey"), "UNIFIED_CONTROLLER_KEY_FILE")
	assert.Contains(t, lost(t, caps, "sso"), "device flow")
	assert.Contains(t, lost(t, caps, "webUI"), "404")
}

// The controller logs "log trim enabled" whenever the setting is positive, but
// RunLogTrim returns immediately without an object store
// (internal/controller/log_trim.go:29-31), so the sweeper never runs. The
// summary is where that contradiction has to surface.
func TestSummarizeStartupFlagsLogTrimAsInertWithoutAnObjectStore(t *testing.T) {
	caps := summarizeStartup(startupInputs{
		ObjectStore: "none",
		KeyDesc:     "key file /etc/unified-cd/kek",
		OIDC:        true,
		WebUI:       true,
		LogTrimDays: 30,
	})

	assert.Equal(t, "inert", func() string {
		for _, c := range caps {
			if c.Name == "logTrim" {
				return c.State
			}
		}
		return ""
	}())
	assert.Contains(t, lost(t, caps, "logTrim"), "never runs without an object store")
}

func TestSummarizeStartupOmitsLogTrimWhenItWouldActuallyRun(t *testing.T) {
	caps := summarizeStartup(startupInputs{
		ObjectStore: "s3",
		KeyDesc:     "key file /etc/unified-cd/kek",
		OIDC:        true,
		WebUI:       true,
		LogTrimDays: 30,
	})

	for _, c := range caps {
		assert.NotEqual(t, "logTrim", c.Name, "logTrim should only appear when it is inert")
	}
}

func TestLogStartupSummaryEmitsOneInfoAndOneWarnPerDegradedCapability(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	logStartupSummary(startupInputs{
		ObjectStore:  "none",
		KeyDesc:      "ephemeral development key",
		KeyEphemeral: true,
		OIDC:         false,
		WebUI:        true,
		LogTrimDays:  30,
	})

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, 5, "one summary record plus one per degraded capability")

	assert.Contains(t, lines[0], `"msg":"startup summary"`)
	assert.Contains(t, lines[0], `"objectStore":"none"`)
	assert.Contains(t, lines[0], `"webUI":"served"`)
	assert.Contains(t, lines[0], `"level":"INFO"`)

	warned := strings.Join(lines[1:], "\n")
	assert.Contains(t, warned, `"capability":"objectStore"`)
	assert.Contains(t, warned, `"capability":"secretKey"`)
	assert.Contains(t, warned, `"capability":"sso"`)
	assert.Contains(t, warned, `"capability":"logTrim"`)
	assert.NotContains(t, warned, `"capability":"webUI"`)
	for _, line := range lines[1:] {
		assert.Contains(t, line, `"level":"WARN"`, "every degraded-capability record must be a warning: %s", line)
	}
}
