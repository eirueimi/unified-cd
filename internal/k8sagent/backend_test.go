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
// parser. The load-bearing rows are the last two: an ABSENT marker is
// cacheResultUnknown, never cacheResultHit. Defaulting it to a hit is what let
// a cache the sidecar never contacted report "cache hit" indefinitely.
func TestParseCacheResult(t *testing.T) {
	cases := []struct {
		name   string
		stdout string
		want   cacheResult
	}{
		{"hit marker", "UCD_CACHE_RESULT=hit\n", cacheResultHit},
		{"miss marker", "UCD_CACHE_RESULT=miss\n", cacheResultMiss},
		{"empty stdout (error path emits no marker)", "", cacheResultUnknown},
		{"unrelated stdout, no marker", "some other output\n", cacheResultUnknown},
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
		wantErr bool
	}{
		{"hit marker on stdout", "UCD_CACHE_RESULT=hit\n", true, false},
		{"miss marker on stdout", "UCD_CACHE_RESULT=miss\n", false, false},
		// An exit-0 restore with no marker is the sidecar's swallowed
		// "cache restore error (ignored)" path: nothing was restored, so it is
		// reported as unknown-and-not-restored, never as a hit.
		{"no marker is unknown, not a hit", "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ex := &fakeExec{exit: 0, stdout: tc.stdout}
			a := &K8sAgent{exec: ex}
			b := newK8sBackend(a, "run-1", "test-job", "pod-default", "/workspace", nil, metav1.Time{})

			hit, err := b.CacheRestore(context.Background(), agentlib.ScopeHandle{}, "k1", nil, "/workspace/cachedir")
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantHit, hit)
			assert.Equal(t, "pod-default", ex.gotPod, "non-scoped restore must target the default pod")
			assert.Equal(t, artifactSidecarName, ex.gotContainer)
			assertJobFlagInArgv(t, ex.gotScript, "test-job")
		})
	}
}

// TestK8sBackend_CacheRestore_NonZeroExitIsNotAHit is the regression test for
// the worst defect in this family: a cache step reporting a HIT for a cache it
// never contacted.
//
// The shape reproduced here is exactly production's. With no
// sidecarS3SecretName, `unified-sidecar cache restore` exits 1 ("cache requires
// S3 configuration (UNIFIED_S3_*)") before touching S3 and prints no
// UCD_CACHE_RESULT marker. Executor.execArgv maps that non-zero exit to
// (1, nil) — a Go error is NOT produced — so CacheRestore used to discard the
// exit code entirely (`_ = ec`) and derive hit/miss from the absent marker,
// which defaulted to hit. Result: `slog.Info("cache hit")` for a completely
// inert cache, for as long as the misconfiguration lasted.
//
// Cache stays best-effort — the orchestrator only warns on this error and the
// step still succeeds — but it must never be a hit.
func TestK8sBackend_CacheRestore_NonZeroExitIsNotAHit(t *testing.T) {
	// exit 1 with NO Go error and NO marker: precisely what execArgv returns
	// for a sidecar that exited non-zero on its own.
	ex := &fakeExec{exit: 1, err: nil, stdout: ""}
	a := &K8sAgent{exec: ex}
	b := newK8sBackend(a, "run-1", "test-job", "pod-default", "/workspace", nil, metav1.Time{})

	hit, err := b.CacheRestore(context.Background(), agentlib.ScopeHandle{}, "k1", nil, "/workspace/cachedir")
	assert.False(t, hit, "a sidecar that exited non-zero restored nothing; it must never be reported as a cache hit")
	require.Error(t, err, "the non-zero exit must surface so the orchestrator logs 'not restored' instead of 'cache hit'")
	assert.Contains(t, err.Error(), "exited 1")
}

// TestK8sBackend_CacheSave_NonZeroExitIsNotSaved is the save-side twin: a
// sidecar that exited non-zero saved nothing, and CacheSave returning nil is
// what made the deferred cache hook log "cache saved".
func TestK8sBackend_CacheSave_NonZeroExitIsNotSaved(t *testing.T) {
	ex := &fakeExec{exit: 1, err: nil}
	a := &K8sAgent{exec: ex}
	b := newK8sBackend(a, "run-1", "test-job", "pod-default", "/workspace", nil, metav1.Time{})

	err := b.CacheSave(context.Background(), agentlib.ScopeHandle{}, "k1", "/workspace/cachedir", 7)
	require.Error(t, err, "a sidecar that exited non-zero saved nothing; it must not be logged as 'cache saved'")
	assert.Contains(t, err.Error(), "exited 1")
}

// TestK8sBackend_ArtifactTransfers_HonorExitCode locks in that the artifact
// paths — unlike cache before this fix — do check the sidecar's exit code, and
// fail the step rather than silently reporting success. Swept alongside the
// cache fix so a future refactor cannot regress them into the same shape.
func TestK8sBackend_ArtifactTransfers_HonorExitCode(t *testing.T) {
	newBackend := func(ex *fakeExec) *k8sBackend {
		return newK8sBackend(&K8sAgent{exec: ex}, "run-1", "test-job", "pod-default", "/workspace", nil, metav1.Time{})
	}

	t.Run("upload", func(t *testing.T) {
		err := newBackend(&fakeExec{exit: 1}).UploadArtifact(context.Background(), agentlib.ScopeHandle{}, "run-1", "app", "/workspace/bin")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "sidecar exited 1")
		require.NoError(t, newBackend(&fakeExec{exit: 0}).UploadArtifact(context.Background(), agentlib.ScopeHandle{}, "run-1", "app", "/workspace/bin"))
	})
	t.Run("download", func(t *testing.T) {
		err := newBackend(&fakeExec{exit: 1}).DownloadArtifact(context.Background(), agentlib.ScopeHandle{}, "run-1", "app", "/workspace/in")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "sidecar exited 1")
		require.NoError(t, newBackend(&fakeExec{exit: 0}).DownloadArtifact(context.Background(), agentlib.ScopeHandle{}, "run-1", "app", "/workspace/in"))
	})
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
