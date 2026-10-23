//go:build integration

package agent

import (
	"context"
	"os"
	"testing"

	crt "github.com/eirueimi/unified-cd/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProbeSecurityContext_RealRuntime runs the startup probe against the real
// container runtime on this host. It logs the result; on a rootful docker it
// must pass with no mapped-ID limit.
func TestProbeSecurityContext_RealRuntime(t *testing.T) {
	rt, err := crt.Detect(testRuntimePref())
	if err != nil {
		skipOrFailInCI(t, "no container runtime (%s=%q): %v", containerRuntimeEnv, testRuntimePref(), err)
	}
	// UCD_PROBE_WSBASE points the probe at a directory the runtime can bind
	// with real Linux semantics (a Windows or macOS file-sharing mount may
	// not honour setgid, which is exactly what the probe detects).
	ws := os.Getenv(testWorkspaceBaseEnv)
	if ws == "" {
		ws = os.Getenv("UCD_PROBE_WSBASE") // older name for the same thing
	}
	if ws == "" {
		ws = t.TempDir()
	}
	toolsDir, err := InstallShim(ws)
	require.NoError(t, err)

	res, perr := probeSecurityContext(context.Background(), rt, ws, toolsDir, "docker.io/library/busybox:1.36")
	t.Logf("runtime=%s probe=%+v err=%v", rt.Name(), res, perr)
	assert.True(t, res.OK, res.Reason)

	entries, err := os.ReadDir(ws)
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".ucd-probe-", "probe temp dir left behind")
	}
}
