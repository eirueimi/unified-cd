package k8sagent

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/config"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

var gateNow = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

func mustTh(t *testing.T, s string) config.DiskThreshold {
	t.Helper()
	th, err := config.ParseDiskThreshold(s)
	require.NoError(t, err)
	return th
}

func vstat(free, total uint64, at time.Time) api.VolumeStats {
	return api.VolumeStats{FilesystemStats: api.FilesystemStats{FreeBytes: free, TotalBytes: total}, MeasuredAt: at}
}

func TestVolumeGated(t *testing.T) {
	fresh := gateNow.Add(-time.Minute)
	cases := []struct {
		name  string
		stats api.VolumeStats
		th    string
		want  bool
	}{
		{"below percent and fresh", vstat(5, 1000, fresh), "1%", true},
		{"below absolute and fresh", vstat(50, 1000, fresh), "100", true},
		{"exactly at threshold", vstat(10, 1000, fresh), "1%", false},
		{"above threshold", vstat(500, 1000, fresh), "1%", false},
		{"just inside the max age", vstat(5, 1000, gateNow.Add(-volumeGateMaxAge)), "1%", true},
		{"older than 10 minutes is ignored (lockout escape)", vstat(5, 1000, gateNow.Add(-volumeGateMaxAge-time.Second)), "1%", false},
		{"never measured", api.VolumeStats{}, "1%", false},
		{"threshold equal to total", vstat(5, 1000, fresh), "1000", false},
		{"threshold above total", vstat(5, 1000, fresh), "5000", false},
		{"off", vstat(0, 1000, fresh), "off", false},
		{"unset", vstat(0, 1000, fresh), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var th config.DiskThreshold
			if tc.th != "" {
				th = mustTh(t, tc.th)
			}
			assert.Equal(t, tc.want, volumeGated(tc.stats, th, gateNow))
		})
	}
}

// logCapture collects what the default slog logger writes.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logCapture) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logCapture) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *logCapture) count(sub string) int { return strings.Count(l.String(), sub) }

func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	lc := &logCapture{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lc, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return lc
}

func seed(a *K8sAgent, name string, free, total uint64, at time.Time) {
	a.volumeStats.set(name, api.FilesystemStats{FreeBytes: free, TotalBytes: total}, at)
}

func TestClaimRequest_GatesLowVolumes(t *testing.T) {
	now := time.Now().UTC()
	for _, mode := range []string{"exclusive", "shared"} {
		t.Run(mode, func(t *testing.T) {
			f := newVolFixture(t, []config.VolumeConfig{vcfg("full", mode, "1Gi"), vcfg("roomy", mode, "1Gi")})
			seed(f.a, "full", 5, 1000, now)
			seed(f.a, "roomy", 900, 1000, now)
			req := f.a.claimRequest(context.Background())
			assert.Equal(t, []string{"full", "roomy"}, req.Volumes)
			assert.Equal(t, []string{"full"}, req.BusyVolumes)
		})
	}
}

func TestClaimRequest_HeldAndLowIsListedOnce(t *testing.T) {
	f := newVolFixture(t, []config.VolumeConfig{vcfg("unity", "exclusive", "1Gi"), vcfg("aaa", "shared", "1Gi")})
	f.holder("r1", "unity")
	seed(f.a, "unity", 1, 1000, time.Now().UTC())
	seed(f.a, "aaa", 1, 1000, time.Now().UTC())
	req := f.a.claimRequest(context.Background())
	assert.Equal(t, []string{"unity", "aaa"}, req.BusyVolumes, "held first, then gated in name order, no duplicate")
}

func TestClaimRequest_IgnoresUndeclaredVolumes(t *testing.T) {
	f := newVolFixture(t, []config.VolumeConfig{vcfg("mine", "shared", "1Gi")})
	seed(f.a, "stranger", 1, 1000, time.Now().UTC())
	assert.Empty(t, f.a.claimRequest(context.Background()).BusyVolumes)
}

func TestClaimRequest_StaleAndMissingReadingsFailOpen(t *testing.T) {
	f := newVolFixture(t, []config.VolumeConfig{vcfg("old", "shared", "1Gi"), vcfg("never", "shared", "1Gi")})
	seed(f.a, "old", 1, 1000, time.Now().UTC().Add(-volumeGateMaxAge-time.Minute))
	assert.Empty(t, f.a.claimRequest(context.Background()).BusyVolumes)
}

// failingListPM is a podManager whose ListPods fails.
type failingListPM struct{ volPM }

func (failingListPM) ListPods(context.Context, string) (*corev1.PodList, error) {
	return nil, assert.AnError
}

func TestClaimRequest_GatedNamesSurviveAPodListError(t *testing.T) {
	f := newVolFixture(t, []config.VolumeConfig{vcfg("full", "exclusive", "1Gi")})
	f.a.pm = failingListPM{f.a.pm.(volPM)}
	seed(f.a, "full", 1, 1000, time.Now().UTC())
	req := f.a.claimRequest(context.Background())
	assert.Equal(t, []string{"full"}, req.Volumes)
	assert.Equal(t, []string{"full"}, req.BusyVolumes, "gating is local; only the pod-derived busy list degrades")
}

func TestClaimRequest_UsesTheConfiguredThreshold(t *testing.T) {
	f := newVolFixture(t, []config.VolumeConfig{vcfg("v", "shared", "1Gi")})
	seed(f.a, "v", 500, 1000, time.Now().UTC())
	assert.Empty(t, f.a.claimRequest(context.Background()).BusyVolumes, "50% free is fine under the 1% default")
	f.a.cfg.MinFreeDisk = mustTh(t, "600")
	assert.Equal(t, []string{"v"}, f.a.claimRequest(context.Background()).BusyVolumes)
	f.a.cfg.MinFreeDisk = mustTh(t, "off")
	assert.Empty(t, f.a.claimRequest(context.Background()).BusyVolumes)
}

func TestClaimRequest_LogsGateTransitionsOnce(t *testing.T) {
	lc := captureLogs(t)
	f := newVolFixture(t, []config.VolumeConfig{vcfg("v", "shared", "1Gi")})
	seed(f.a, "v", 1, 1000, time.Now().UTC())
	for i := 0; i < 3; i++ {
		f.a.claimRequest(context.Background())
	}
	assert.Equal(t, 1, lc.count("volume below minFreeDisk"), "entering is logged once: %s", lc.String())
	assert.Equal(t, 0, lc.count("volume no longer below"))

	seed(f.a, "v", 900, 1000, time.Now().UTC())
	for i := 0; i < 3; i++ {
		f.a.claimRequest(context.Background())
	}
	assert.Equal(t, 1, lc.count("volume below minFreeDisk"))
	assert.Equal(t, 1, lc.count("volume no longer below"), "leaving is logged once: %s", lc.String())
}

func TestClaimRequest_UnsatisfiableThresholdWarnsOncePerVolume(t *testing.T) {
	lc := captureLogs(t)
	f := newVolFixture(t, []config.VolumeConfig{vcfg("small", "shared", "1Gi")})
	f.a.cfg.MinFreeDisk = mustTh(t, "5000")
	seed(f.a, "small", 1, 1000, time.Now().UTC())
	for i := 0; i < 3; i++ {
		assert.Empty(t, f.a.claimRequest(context.Background()).BusyVolumes)
	}
	assert.Equal(t, 1, lc.count("not smaller than the volume"), lc.String())
	assert.Equal(t, 0, lc.count("volume below minFreeDisk"))
}

func TestStorageReading_MarksClaimBlocked(t *testing.T) {
	f := newVolFixture(t, []config.VolumeConfig{vcfg("full", "shared", "1Gi"), vcfg("roomy", "shared", "1Gi"), vcfg("old", "shared", "1Gi")})
	now := time.Now().UTC()
	seed(f.a, "full", 5, 1000, now)
	seed(f.a, "roomy", 900, 1000, now)
	seed(f.a, "old", 5, 1000, now.Add(-time.Hour))
	got := f.a.storageReading().Volumes
	assert.True(t, got["full"].ClaimBlocked)
	assert.False(t, got["roomy"].ClaimBlocked)
	assert.False(t, got["old"].ClaimBlocked, "a stale reading no longer blocks, so the heartbeat must not say it does")
	assert.Equal(t, uint64(5), got["full"].FreeBytes)
}

// Replicas share PVCs but not caches: each agent gates on its own readings.
func TestClaimRequest_ReplicasGateIndependently(t *testing.T) {
	vols := []config.VolumeConfig{vcfg("shared-pvc", "shared", "1Gi")}
	a := newVolFixture(t, vols)
	b := newVolFixture(t, vols)
	seed(a.a, "shared-pvc", 1, 1000, time.Now().UTC())
	seed(b.a, "shared-pvc", 900, 1000, time.Now().UTC())
	assert.Equal(t, []string{"shared-pvc"}, a.a.claimRequest(context.Background()).BusyVolumes)
	assert.Empty(t, b.a.claimRequest(context.Background()).BusyVolumes)
}

func TestWarnLowVolumes(t *testing.T) {
	f := newVolFixture(t, []config.VolumeConfig{vcfg("full", "exclusive", "1Gi"), vcfg("roomy", "exclusive", "1Gi")})
	now := time.Now().UTC()
	seed(f.a, "full", 6<<20, 1<<30, now) // ~0.6%
	seed(f.a, "roomy", 900<<20, 1<<30, now)

	f.a.warnLowVolumes(context.Background(), f.runID, []PodVolume{{Name: "roomy"}})
	assert.Empty(t, f.logText(), "a healthy volume emits nothing")

	f.a.warnLowVolumes(context.Background(), f.runID, []PodVolume{{Name: "full"}, {Name: "roomy"}})
	out := f.logText()
	assert.Equal(t, 1, strings.Count(out, "this run may fail for lack of space"), out)
	for _, want := range []string{"full", "6.0MiB", "0.6%", "1%"} {
		assert.Contains(t, out, want)
	}
	assert.NotContains(t, out, "roomy")
}

func TestExecuteRun_WarnsWhenPreRunReadingIsLow(t *testing.T) {
	f := newVolFixture(t, []config.VolumeConfig{vcfg("go-cache", "exclusive", "10Gi")})
	ex := &statfsExec{override: func(_ context.Context, _ []string, stdout io.Writer) (int, error) {
		_, _ = io.WriteString(stdout, reading(6<<20, 1<<30))
		return 0, nil
	}}
	f.a.exec = &stepAndStatfsExec{steps: f.ex, statfs: ex}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f.a.executeRun(ctx, f.claim([]dsl.VolumeMount{mount("go-cache", "go-cache", ".cache")}))
	require.Equal(t, api.RunSucceeded, f.waitFinish(t))
	assert.Contains(t, f.logText(), "this run may fail for lack of space")
}

func TestExecuteRun_NoWarningWhenPreRunReadingIsHealthy(t *testing.T) {
	f := newVolFixture(t, []config.VolumeConfig{vcfg("go-cache", "exclusive", "10Gi")})
	ex := &statfsExec{override: func(_ context.Context, _ []string, stdout io.Writer) (int, error) {
		_, _ = io.WriteString(stdout, reading(900<<20, 1<<30))
		return 0, nil
	}}
	f.a.exec = &stepAndStatfsExec{steps: f.ex, statfs: ex}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f.a.executeRun(ctx, f.claim([]dsl.VolumeMount{mount("go-cache", "go-cache", ".cache")}))
	require.Equal(t, api.RunSucceeded, f.waitFinish(t))
	assert.NotContains(t, f.logText(), "lack of space")
}
