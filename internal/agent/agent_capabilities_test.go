package agent

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentCapabilities_NativeAlways(t *testing.T) {
	// runtimeAvailable == false -> only native
	assert.Equal(t, []string{"native"}, agentCapabilities(false))
	// runtimeAvailable == true -> native + container
	assert.ElementsMatch(t, []string{"native", "container"}, agentCapabilities(true))
}

// recordHandler keeps every record so a test can assert on what was logged.
type recordHandler struct{ records []slog.Record }

func (h *recordHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r)
	return nil
}
func (h *recordHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordHandler) WithGroup(string) slog.Handler      { return h }

func captureLogs(t *testing.T) *recordHandler {
	t.Helper()
	h := &recordHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h
}

func attr(r slog.Record, key string) (slog.Value, bool) {
	var v slog.Value
	found := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			v, found = a.Value, true
			return false
		}
		return true
	})
	return v, found
}

func TestLogRegistration_MissingRuntimeWarnsWithTheReason(t *testing.T) {
	h := captureLogs(t)
	// The real detector, forced to fail: an unknown preference is an error.
	_, rtErr := (&Agent{RuntimePref: "no-such-runtime"}).containerRuntime()
	require.Error(t, rtErr)

	logRegistration("a1", agentCapabilities(false), rtErr)

	require.Len(t, h.records, 2)
	warn := h.records[0]
	assert.Equal(t, slog.LevelWarn, warn.Level)
	assert.Contains(t, warn.Message, "no container runtime")
	errVal, ok := attr(warn, "error")
	require.True(t, ok, "the warning must carry the detection error")
	assert.Contains(t, errVal.String(), "no-such-runtime")

	info := h.records[1]
	caps, ok := attr(info, "capabilities")
	require.True(t, ok, "the registration line must list the capabilities")
	assert.Equal(t, []string{"native"}, caps.Any())
}

func TestLogRegistration_RuntimePresentDoesNotWarn(t *testing.T) {
	h := captureLogs(t)

	logRegistration("a1", agentCapabilities(true), nil)

	require.Len(t, h.records, 1)
	assert.Equal(t, slog.LevelInfo, h.records[0].Level)
	caps, ok := attr(h.records[0], "capabilities")
	require.True(t, ok)
	assert.ElementsMatch(t, []string{"native", "container"}, caps.Any())
}
