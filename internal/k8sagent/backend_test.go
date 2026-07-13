package k8sagent

import (
	"context"
	"testing"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseCacheResult is a table test for the UCD_CACHE_RESULT stdout-marker
// parser: a "miss" marker must yield false, a "hit" marker (or any stdout
// without a "miss" marker, including an empty/older-sidecar stdout) must
// yield true — preserving the historical lenient best-effort default.
func TestParseCacheResult(t *testing.T) {
	cases := []struct {
		name   string
		stdout string
		want   bool
	}{
		{"hit marker", "UCD_CACHE_RESULT=hit\n", true},
		{"miss marker", "UCD_CACHE_RESULT=miss\n", false},
		{"empty stdout (older sidecar / error path)", "", true},
		{"unrelated stdout, no marker", "some other output\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, parseCacheResult(tc.stdout))
		})
	}
}

// TestK8sBackend_CacheRestore_HonorsStdoutMarker drives the real
// k8sBackend.CacheRestore against a fakeExec that simulates the sidecar's
// exit-0-always contract while varying its stdout marker, proving
// CacheRestore's returned bool now tracks the sidecar's true hit/miss
// (via UCD_CACHE_RESULT) rather than unconditionally reporting exit==0 as a
// hit (the parity #4 bug).
func TestK8sBackend_CacheRestore_HonorsStdoutMarker(t *testing.T) {
	cases := []struct {
		name    string
		stdout  string
		wantHit bool
	}{
		{"hit marker on stdout", "UCD_CACHE_RESULT=hit\n", true},
		{"miss marker on stdout", "UCD_CACHE_RESULT=miss\n", false},
		{"no marker (older sidecar) defaults to hit", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ex := &fakeExec{exit: 0, stdout: tc.stdout}
			a := &K8sAgent{exec: ex}
			b := newK8sBackend(a, "run-1", "pod-default", "/workspace")

			hit, err := b.CacheRestore(context.Background(), agentlib.ScopeHandle{}, "k1", nil, "/workspace/cachedir")
			require.NoError(t, err)
			assert.Equal(t, tc.wantHit, hit)
			assert.Equal(t, "pod-default", ex.gotPod, "non-scoped restore must target the default pod")
			assert.Equal(t, artifactSidecarName, ex.gotContainer)
		})
	}
}

// TestK8sBackend_CacheRestore_PropagatesExecError proves a genuine exec
// failure (distinct from a cache miss) still surfaces as an error, not a
// silently-lenient (true, nil) — unchanged by this fix.
func TestK8sBackend_CacheRestore_PropagatesExecError(t *testing.T) {
	wantErr := assert.AnError
	ex := &fakeExec{exit: 1, err: wantErr}
	a := &K8sAgent{exec: ex}
	b := newK8sBackend(a, "run-1", "pod-default", "/workspace")

	hit, err := b.CacheRestore(context.Background(), agentlib.ScopeHandle{}, "k1", nil, "/workspace/cachedir")
	require.ErrorIs(t, err, wantErr)
	assert.False(t, hit)
}
