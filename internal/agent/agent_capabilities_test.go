package agent

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	crt "github.com/eirueimi/unified-cd/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentCapabilities_NativeAlways(t *testing.T) {
	// runtimeAvailable == false -> only native
	assert.Equal(t, []string{"native"}, agentCapabilities(false, false))
	// runtimeAvailable == true -> native + container
	assert.ElementsMatch(t, []string{"native", "container"}, agentCapabilities(true, false))
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

	logRegistration("a1", agentCapabilities(false, false), rtErr, probeResult{})

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

	logRegistration("a1", agentCapabilities(true, true), nil, probeResult{OK: true})

	require.Len(t, h.records, 1)
	assert.Equal(t, slog.LevelInfo, h.records[0].Level)
	caps, ok := attr(h.records[0], "capabilities")
	require.True(t, ok)
	assert.ElementsMatch(t, []string{"native", "container", "securitycontext"}, caps.Any())
}

func TestAgentCapabilities_SecurityContextOnlyWithPassingProbe(t *testing.T) {
	assert.ElementsMatch(t, []string{"native", "container", "securitycontext"}, agentCapabilities(true, true))
	assert.ElementsMatch(t, []string{"native", "container"}, agentCapabilities(true, false))
	// Without a runtime the probe cannot have passed.
	assert.Equal(t, []string{"native"}, agentCapabilities(false, true))
}

func TestLogRegistration_FailedProbeWarnsWithTheReason(t *testing.T) {
	h := captureLogs(t)

	logRegistration("a1", agentCapabilities(true, false), nil, probeResult{Reason: "file group not inherited"})

	require.Len(t, h.records, 2)
	warn := h.records[0]
	assert.Equal(t, slog.LevelWarn, warn.Level)
	assert.Contains(t, warn.Message, "cannot honour securityContext (file group not inherited)")
	assert.Contains(t, warn.Message, "jobs that set securityContext will not be routed to this agent")
	assert.Equal(t, slog.LevelInfo, h.records[1].Level)
}

func TestLogRegistration_NoRuntimeDoesNotAddProbeWarning(t *testing.T) {
	h := captureLogs(t)
	logRegistration("a1", agentCapabilities(false, false), errors.New("none"), probeResult{Reason: "no container runtime"})
	require.Len(t, h.records, 2, "the missing-runtime warning already covers it")
}

func TestAgentCapabilities_UsesProbeOnce(t *testing.T) {
	calls := 0
	prev := probeSecurityContextFn
	probeSecurityContextFn = func(context.Context, crt.ContainerRuntime, string, string, string) (probeResult, error) {
		calls++
		return probeResult{OK: true, Rootless: true, MaxMappedID: 65536}, nil
	}
	t.Cleanup(func() { probeSecurityContextFn = prev })

	a := &Agent{ToolsDir: "/t", PauseImage: "p", WorkspaceDir: t.TempDir()}
	a.resolvedRuntime = &probeFakeRT{}
	a.runtimeOnce.Do(func() {})
	assert.ElementsMatch(t, []string{"native", "container", "securitycontext"}, a.capabilities())
	assert.ElementsMatch(t, []string{"native", "container", "securitycontext"}, a.capabilities())
	assert.Equal(t, 1, calls, "the probe runs once per process")
	assert.Equal(t, int64(65536), a.securityContextProbe().MaxMappedID)
}

func TestAgentCapabilities_ProbeSkippedWithoutToolsDir(t *testing.T) {
	prev := probeSecurityContextFn
	probeSecurityContextFn = func(context.Context, crt.ContainerRuntime, string, string, string) (probeResult, error) {
		t.Fatal("the probe must not run without a tools dir")
		return probeResult{}, nil
	}
	t.Cleanup(func() { probeSecurityContextFn = prev })

	a := &Agent{PauseImage: "p"}
	a.resolvedRuntime = &probeFakeRT{}
	a.runtimeOnce.Do(func() {})
	assert.Equal(t, []string{"native", "container"}, a.capabilities())
	assert.NotEmpty(t, a.securityContextProbe().Reason)
}

func TestRun_CancelDuringProbeReturnsPromptly(t *testing.T) {
	started := make(chan struct{})
	prev := probeSecurityContextFn
	probeSecurityContextFn = func(ctx context.Context, _ crt.ContainerRuntime, _, _, _ string) (probeResult, error) {
		close(started)
		<-ctx.Done()
		return probeResult{Reason: "probe did not run: " + ctx.Err().Error()}, nil
	}
	t.Cleanup(func() { probeSecurityContextFn = prev })

	a := New("a1", NewClient("http://127.0.0.1:1", "t"))
	a.ToolsDir, a.PauseImage, a.WorkspaceDir = "/t", "p", t.TempDir()
	a.resolvedRuntime = &probeFakeRT{}
	a.runtimeOnce.Do(func() {})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	<-started
	cancel()
	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled during the probe")
	}
	assert.False(t, a.securityContextProbe().OK)
}
