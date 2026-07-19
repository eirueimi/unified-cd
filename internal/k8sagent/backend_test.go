package k8sagent

import (
	"context"
	"strings"
	"testing"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
			b := newK8sBackend(a, "run-1", "test-job", "pod-default", "/workspace", nil, metav1.Time{})

			hit, err := b.CacheRestore(context.Background(), agentlib.ScopeHandle{}, "k1", nil, "/workspace/cachedir")
			require.NoError(t, err)
			assert.Equal(t, tc.wantHit, hit)
			assert.Equal(t, "pod-default", ex.gotPod, "non-scoped restore must target the default pod")
			assert.Equal(t, artifactSidecarName, ex.gotContainer)
			assertJobFlagInArgv(t, ex.gotScript, "test-job")
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
	b := newK8sBackend(a, "run-1", "test-job", "pod-default", "/workspace", nil, metav1.Time{})

	hit, err := b.CacheRestore(context.Background(), agentlib.ScopeHandle{}, "k1", nil, "/workspace/cachedir")
	require.ErrorIs(t, err, wantErr)
	assert.False(t, hit)
}

// TestK8sResolve_Containment proves F-PATH-1's fix on the k8s backend: a
// non-scoped artifact/cache path resolves against the pod's mount path, and
// an absolute or traversal-escaping path is rejected rather than reaching
// outside the mount (e.g. the artifact sidecar's mounted secrets).
func TestK8sResolve_Containment(t *testing.T) {
	b := &k8sBackend{mountPath: "/workspace"}
	got, err := b.ResolveArtifactPath(agentlib.ScopeHandle{}, "reports")
	require.NoError(t, err)
	assert.Equal(t, "/workspace/reports", got)

	_, err = b.ResolveArtifactPath(agentlib.ScopeHandle{}, "../../proc/self/environ")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes the workspace")
	_, err = b.ResolveCachePath(agentlib.ScopeHandle{}, "/etc/passwd")
	require.Error(t, err)
}

// TestK8sBackend_CacheSave_IncludesJobFlag verifies that CacheSave includes the
// --job flag with the qualified job name in its sidecar argv, ensuring cache
// namespacing is enforced at the sidecar layer (task 7 requirement). The test
// asserts both the presence of the flag and its value, so dropping --job or
// reordering argv would cause test failure.
func TestK8sBackend_CacheSave_IncludesJobFlag(t *testing.T) {
	ex := &fakeExec{exit: 0}
	a := &K8sAgent{exec: ex}
	b := newK8sBackend(a, "run-1", "team-a/build", "pod-default", "/workspace", nil, metav1.Time{})

	err := b.CacheSave(context.Background(), agentlib.ScopeHandle{}, "cache-key-1", "/workspace/cache", 7)
	require.NoError(t, err)
	assert.Equal(t, "pod-default", ex.gotPod, "non-scoped save must target the default pod")
	assert.Equal(t, artifactSidecarName, ex.gotContainer)
	assertJobFlagInArgv(t, ex.gotScript, "team-a/build")
}

// assertJobFlagInArgv verifies that the given space-separated argv string
// contains the --job flag immediately followed by the expected jobName value.
// This ensures the flag is not dropped, reordered, or assigned a different value.
func assertJobFlagInArgv(t *testing.T, argv, expectedJobName string) {
	t.Helper()
	parts := strings.Split(argv, " ")
	found := false
	for i, part := range parts {
		if part == "--job" {
			if i+1 < len(parts) && parts[i+1] == expectedJobName {
				found = true
				break
			}
		}
	}
	assert.True(t, found, "argv must contain '--job' followed by '%s', got: %s", expectedJobName, argv)
}
