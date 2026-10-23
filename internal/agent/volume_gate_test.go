package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/config"
)

// lockedBuf is a bytes.Buffer safe to share with the slog handler.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// captureGateLogs routes slog to a buffer for the rest of the test.
func captureGateLogs(t *testing.T) *lockedBuf {
	t.Helper()
	buf := &lockedBuf{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return buf
}

// seededSampler measures paths once with stat and returns the sampler.
func seededSampler(stat func(string) (uint64, uint64, error), paths []string) *storageSampler {
	s := newStorageSampler(stat, DefaultHeartbeatInterval)
	s.sampleAll(paths)
	s.wg.Wait()
	return s
}

// volStats builds a statfs fake: per-path (free,total), anything else is 100/100.
func volStats(m map[string][2]uint64) func(string) (uint64, uint64, error) {
	return func(p string) (uint64, uint64, error) {
		if v, ok := m[p]; ok {
			return v[0], v[1], nil
		}
		return 100, 100, nil
	}
}

func TestGatedVolumes_BelowThresholdIsGatedAtOrAboveIsNot(t *testing.T) {
	a, ws := newStorageTestAgent(t, 0, nil)
	cache, data := a.Volumes.PathOf("cache"), a.Volumes.PathOf("data")
	s := seededSampler(volStats(map[string][2]uint64{cache: {5, 1000}, data: {10, 1000}}), a.storagePaths(ws))

	gated, unsat := gatedVolumes(a.Volumes, s, config.DiskThreshold{Bytes: 10, Set: true})

	require.Equal(t, []string{"cache"}, gated, "free==threshold is not below it")
	require.Empty(t, unsat)
}

func TestGatedVolumes_PercentUsesEachVolumesOwnTotal(t *testing.T) {
	a, ws := newStorageTestAgent(t, 0, nil)
	cache, data := a.Volumes.PathOf("cache"), a.Volumes.PathOf("data")
	// 1% of 1000 = 10, of 100000 = 1000.
	s := seededSampler(volStats(map[string][2]uint64{cache: {9, 1000}, data: {900, 100000}}), a.storagePaths(ws))

	gated, _ := gatedVolumes(a.Volumes, s, config.DiskThreshold{Percent: 1, Set: true})

	require.Equal(t, []string{"cache", "data"}, gated)
}

func TestGatedVolumes_OffAndUnsetNeverGate(t *testing.T) {
	a, ws := newStorageTestAgent(t, 0, nil)
	s := seededSampler(volStats(map[string][2]uint64{a.Volumes.PathOf("cache"): {0, 1000}}), a.storagePaths(ws))

	for _, th := range []config.DiskThreshold{{}, {Set: true, Off: true}} {
		gated, unsat := gatedVolumes(a.Volumes, s, th)
		require.Empty(t, gated)
		require.Empty(t, unsat)
	}
}

func TestGatedVolumes_NeverMeasuredOrStaleIsNotGated(t *testing.T) {
	a, ws := newStorageTestAgent(t, 0, nil)
	cache := a.Volumes.PathOf("cache")
	clk := &fakeClock{t: time.Now()}
	s := newStorageSampler(volStats(map[string][2]uint64{cache: {0, 1000}}), time.Second)
	s.now = clk.now
	th := config.DiskThreshold{Bytes: 10, Set: true}

	gated, _ := gatedVolumes(a.Volumes, s, th)
	require.Empty(t, gated, "never measured")

	s.sampleAll(a.storagePaths(ws))
	s.wg.Wait()
	gated, _ = gatedVolumes(a.Volumes, s, th)
	require.Equal(t, []string{"cache"}, gated)

	clk.advance(10 * time.Second)
	gated, _ = gatedVolumes(a.Volumes, s, th)
	require.Empty(t, gated, "a stale reading fails open")
}

func TestGatedVolumes_AbsoluteThresholdNotSmallerThanVolumeIsUnsatisfiableAndSkipped(t *testing.T) {
	a, ws := newStorageTestAgent(t, 0, nil)
	cache, data := a.Volumes.PathOf("cache"), a.Volumes.PathOf("data")
	s := seededSampler(volStats(map[string][2]uint64{cache: {1, 50}, data: {1, 5000}}), a.storagePaths(ws))

	gated, unsat := gatedVolumes(a.Volumes, s, config.DiskThreshold{Bytes: 100, Set: true})

	require.Equal(t, []string{"data"}, gated)
	require.Equal(t, []string{"cache"}, unsat)
}

func TestVolumeGate_UnsatisfiableWarnsOncePerVolumePerProcess(t *testing.T) {
	logs := captureGateLogs(t)
	a, ws := newStorageTestAgent(t, 0, nil)
	s := seededSampler(volStats(map[string][2]uint64{a.Volumes.PathOf("cache"): {1, 50}, a.Volumes.PathOf("data"): {500, 5000}}), a.storagePaths(ws))
	th := config.DiskThreshold{Bytes: 100, Set: true}

	for i := 0; i < 3; i++ {
		require.Empty(t, a.volGate.names(a.Volumes, s, th))
	}

	require.Equal(t, 1, strings.Count(logs.String(), "not smaller than the volume"), logs.String())
	require.Contains(t, logs.String(), "volume=cache")
}

func TestVolumeGate_LogsEnteringAndLeavingOnceEach(t *testing.T) {
	logs := captureGateLogs(t)
	a, ws := newStorageTestAgent(t, 0, nil)
	cache := a.Volumes.PathOf("cache")
	var mu sync.Mutex
	free := uint64(5)
	stat := func(p string) (uint64, uint64, error) {
		mu.Lock()
		defer mu.Unlock()
		if p == cache {
			return free, 1000, nil
		}
		return 100, 100, nil
	}
	s := newStorageSampler(stat, DefaultHeartbeatInterval)
	th := config.DiskThreshold{Bytes: 10, Set: true}
	measure := func() { s.sampleAll(a.storagePaths(ws)); s.wg.Wait() }

	measure()
	for i := 0; i < 5; i++ { // one per 2s claim iteration
		require.Equal(t, []string{"cache"}, a.volGate.names(a.Volumes, s, th))
	}
	require.Equal(t, 1, strings.Count(logs.String(), "volume below minFreeDisk"), logs.String())
	require.Contains(t, logs.String(), "level=WARN")

	mu.Lock()
	free = 500
	mu.Unlock()
	measure()
	for i := 0; i < 5; i++ {
		require.Empty(t, a.volGate.names(a.Volumes, s, th))
	}
	require.Equal(t, 1, strings.Count(logs.String(), "volume no longer below minFreeDisk"), logs.String())
	require.Equal(t, 1, strings.Count(logs.String(), "volume below minFreeDisk"), "entering is not logged again")
}

// claimRecorder serves register and claim, records each claim, and cancels
// after n claims.
type claimRecorder struct {
	mu      sync.Mutex
	bodies  []api.ClaimRequest
	raw     []string
	queries []string
}

func (c *claimRecorder) server(t *testing.T, id string, n int, cancel context.CancelFunc) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", registerHandler)
	mux.HandleFunc("POST /api/v1/agents/"+id+"/claim", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req api.ClaimRequest
		_ = json.Unmarshal(raw, &req)
		c.mu.Lock()
		c.bodies = append(c.bodies, req)
		c.raw = append(c.raw, string(raw))
		c.queries = append(c.queries, r.URL.RawQuery)
		done := len(c.bodies) >= n
		c.mu.Unlock()
		if done {
			cancel()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.ClaimResponse{})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (c *claimRecorder) snapshot() []api.ClaimRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]api.ClaimRequest(nil), c.bodies...)
}

// runClaims runs one runLoop slot until n claims have been made.
func runClaims(t *testing.T, a *Agent, ws string, n int, detached bool) *claimRecorder {
	t.Helper()
	rec := &claimRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := rec.server(t, a.ID, n, cancel)
	a.Client = NewClient(srv.URL, "tok")
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.runLoop(ctx, context.Background(), 0, ws, NewRunSet(), NewRunSet(), detached)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runLoop did not finish")
	}
	return rec
}

// gateAgent builds a two-volume agent (cache exclusive, data shared) whose
// workspace statfs and background sampler both answer from stat.
func gateAgent(t *testing.T, id string, th config.DiskThreshold, stat func(a *Agent, path string) (uint64, uint64, error)) (*Agent, string) {
	t.Helper()
	var a *Agent
	fn := func(p string) (uint64, uint64, error) { return stat(a, p) }
	var ws string
	a, ws = newStorageTestAgent(t, 0, fn)
	a.ID = id
	a.MinFreeDisk = th
	a.sampler = seededSampler(fn, a.storagePaths(ws))
	return a, ws
}

func cacheLow(a *Agent, p string) (uint64, uint64, error) {
	if p == a.Volumes.PathOf("cache") {
		return 5, 1000, nil
	}
	return 500, 1000, nil
}

func TestClaim_GatedVolumeIsAddedToBusyVolumesAndTheClaimStillHappens(t *testing.T) {
	a, ws := gateAgent(t, "a-vg1", config.DiskThreshold{Bytes: 10, Set: true}, cacheLow)

	rec := runClaims(t, a, ws, 1, false)

	got := rec.snapshot()
	require.Len(t, got, 1)
	require.Equal(t, []string{"cache"}, got[0].BusyVolumes)
	require.Equal(t, []string{"cache", "data"}, got[0].Volumes)
}

// Ordering rule: Busy() is kept exactly as the volume manager returns it
// (held exclusive volumes, then reserved paths), then the gated names follow in
// name order, skipping any already present. A volume that is both held and
// gated therefore appears once, in its Busy() position.
func TestClaim_BusyEntriesAreKeptThenGatedAppendedWithoutDuplicates(t *testing.T) {
	a, ws := gateAgent(t, "a-vg2", config.DiskThreshold{Bytes: 10, Set: true}, func(a *Agent, p string) (uint64, uint64, error) {
		if p == a.Volumes.PathOf("cache") || p == a.Volumes.PathOf("data") {
			return 5, 1000, nil // both gated
		}
		return 500, 1000, nil
	})
	_, release, err := a.Volumes.Reserve("run-1", []string{"cache"}) // cache is now in Busy() too
	require.NoError(t, err)
	defer release()
	pathRelease, err := a.Volumes.ReservePath(t.TempDir())
	require.NoError(t, err)
	defer pathRelease()
	busy := a.Volumes.Busy()
	require.Len(t, busy, 2)
	require.Equal(t, "cache", busy[0])

	rec := runClaims(t, a, ws, 1, false)

	want := append(append([]string(nil), busy...), "data")
	require.Equal(t, want, rec.snapshot()[0].BusyVolumes)
}

func TestClaim_VolumeAtThresholdOrNeverMeasuredIsAbsent(t *testing.T) {
	a, ws := gateAgent(t, "a-vg3", config.DiskThreshold{Bytes: 10, Set: true}, func(a *Agent, p string) (uint64, uint64, error) {
		if p == a.Volumes.PathOf("cache") {
			return 10, 1000, nil // exactly at the threshold
		}
		if p == a.Volumes.PathOf("data") {
			return 0, 0, context.DeadlineExceeded // never measures
		}
		return 500, 1000, nil
	})

	rec := runClaims(t, a, ws, 1, false)

	require.Empty(t, rec.snapshot()[0].BusyVolumes)
}

func TestClaim_PercentThresholdGatesTheWorkspaceAndVolumes(t *testing.T) {
	// 1% workspace: free 0.5% blocks, 2% claims.
	var mu sync.Mutex
	wsFree := uint64(5)
	var wsPath string
	a, ws := gateAgent(t, "a-vg7", config.DiskThreshold{Percent: 1, Set: true}, func(a *Agent, p string) (uint64, uint64, error) {
		mu.Lock()
		defer mu.Unlock()
		if p == wsPath {
			return wsFree, 1000, nil
		}
		return 500, 1000, nil
	})
	wsPath = ws
	logs := captureGateLogs(t)
	// Re-seed so the sampler sees the workspace answer too (irrelevant to the gate).
	rec := &claimRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	srv := rec.server(t, a.ID, 1, cancel)
	a.Client = NewClient(srv.URL, "tok")
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.runLoop(ctx, context.Background(), 0, ws, NewRunSet(), NewRunSet(), false)
	}()

	require.Eventually(t, func() bool { return a.blockedSlots.Load() == 1 }, 5*time.Second, 10*time.Millisecond)
	require.Empty(t, rec.snapshot(), "0.5 percent free is below 1 percent")
	require.Contains(t, logs.String(), "thresholdBytes=10")
	require.Contains(t, logs.String(), "thresholdPercent=1")

	mu.Lock()
	wsFree = 20 // 2%
	mu.Unlock()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("claim did not resume once free space reached 2%")
	}
	require.Len(t, rec.snapshot(), 1)
	require.EqualValues(t, 0, a.blockedSlots.Load())
}

func TestClaim_OffNeverGatesTheWorkspace(t *testing.T) {
	a, ws := gateAgent(t, "a-vg8", config.DiskThreshold{Set: true, Off: true}, func(*Agent, string) (uint64, uint64, error) { return 0, 1000, nil })

	rec := runClaims(t, a, ws, 1, false)

	require.Len(t, rec.snapshot(), 1)
	require.Empty(t, rec.snapshot()[0].BusyVolumes)
}

func TestClaim_WorkspaceBlocksEvenWhenVolumesAreFine(t *testing.T) {
	a, ws := gateAgent(t, "a-vg4", config.DiskThreshold{Bytes: 10, Set: true}, func(*Agent, string) (uint64, uint64, error) { return 5, 1000, nil })
	rec := &claimRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	srv := rec.server(t, a.ID, 1, cancel)
	a.Client = NewClient(srv.URL, "tok")
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.runLoop(ctx, context.Background(), 0, ws, NewRunSet(), NewRunSet(), false)
	}()
	require.Eventually(t, func() bool { return a.blockedSlots.Load() == 1 }, 5*time.Second, 10*time.Millisecond)
	cancel()
	<-done
	require.Empty(t, rec.snapshot(), "the workspace gate still stops every claim")
}

func TestClaim_DetachedLoopIgnoresVolumes(t *testing.T) {
	a, ws := gateAgent(t, "a-vg5", config.DiskThreshold{Bytes: 10, Set: true}, cacheLow)

	rec := runClaims(t, a, ws, 1, true)

	require.Contains(t, rec.queries[0], "kind=detached")
	require.Equal(t, "", rec.raw[0], "a detached claim carries no body")
}

// Rulings #8: a volume's statfs can block forever (dead NFS); the claim must
// never make that call itself.
func TestClaim_ABlockedVolumeStatfsNeverStallsTheClaim(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	var volumeCalls atomic.Int32
	a, ws := gateAgent(t, "a-vg6", config.DiskThreshold{Bytes: 10, Set: true}, func(*Agent, string) (uint64, uint64, error) { return 500, 1000, nil })
	a.diskStatsFn = func(p string) (uint64, uint64, error) {
		if p != ws {
			volumeCalls.Add(1)
			<-block
		}
		return 500, 1000, nil
	}
	// The sampler holds no reading for the volumes (its measurement would block).
	a.sampler = newStorageSampler(a.diskStatsFn, DefaultHeartbeatInterval)

	rec := runClaims(t, a, ws, 2, false)

	require.Len(t, rec.snapshot(), 2)
	require.EqualValues(t, 0, volumeCalls.Load(), "runLoop called statfs on a volume path")
}

// With name: [a, b] and a gated, the run must take b: the controller let it
// through on the strength of b, and the agent used to pick a (the first free).
func TestReserveSet_PrefersAnUngatedCandidate(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("a"), exclusive("b"))
	m.SetGated([]string{"a"})
	chosen, rel, err := m.ReserveSet(context.Background(), "run-1", [][]string{{"a", "b"}}, nil, nil)
	require.NoError(t, err)
	defer rel()
	require.Equal(t, []string{"b"}, chosen)
}

// Every candidate gated: the run is already claimed, so it still gets its
// volume (the first free), exactly as with no gate.
func TestReserveSet_FallsBackWhenEveryCandidateIsGated(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("a"), exclusive("b"))
	m.SetGated([]string{"a", "b"})
	chosen, rel, err := m.ReserveSet(context.Background(), "run-1", [][]string{{"a", "b"}}, nil, nil)
	require.NoError(t, err)
	defer rel()
	require.Equal(t, []string{"a"}, chosen)
}

// Nothing gated: unchanged, first listed free candidate wins; clearing the gate
// restores that.
func TestReserveSet_NothingGatedKeepsListedOrder(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("a"), exclusive("b"))
	m.SetGated([]string{"a"})
	m.SetGated(nil)
	chosen, rel, err := m.ReserveSet(context.Background(), "run-1", [][]string{{"a", "b"}}, nil, nil)
	require.NoError(t, err)
	defer rel()
	require.Equal(t, []string{"a"}, chosen)
}

// The workspace gate is agent-wide: several slots (normal and detached) blocked
// for many loop iterations log ONE WARN, and recovery logs ONE INFO.
func TestWorkspaceGate_LogsTransitionsOnceAcrossSlots(t *testing.T) {
	old := workspaceGateWait
	workspaceGateWait = 5 * time.Millisecond
	t.Cleanup(func() { workspaceGateWait = old })

	var mu sync.Mutex
	wsFree := uint64(5)
	var wsPath string
	a, ws := gateAgent(t, "a-wg1", config.DiskThreshold{Bytes: 10, Set: true}, func(a *Agent, p string) (uint64, uint64, error) {
		mu.Lock()
		defer mu.Unlock()
		if p == wsPath {
			return wsFree, 1000, nil
		}
		return 500, 1000, nil
	})
	wsPath = ws
	logs := captureGateLogs(t)
	rec := &claimRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := rec.server(t, a.ID, 4, cancel) // ends once four claims are made after recovery
	a.Client = NewClient(srv.URL, "tok")
	var wg sync.WaitGroup
	for slot := 0; slot < 4; slot++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			a.runLoop(ctx, context.Background(), slot, ws, NewRunSet(), NewRunSet(), slot >= 2)
		}(slot)
	}

	require.Eventually(t, func() bool { return a.blockedSlots.Load() == 4 }, 5*time.Second, 5*time.Millisecond)
	time.Sleep(300 * time.Millisecond) // dozens of loop iterations per slot
	require.Equal(t, 1, strings.Count(logs.String(), "free disk space below minimum, skipping claim"),
		"one agent-wide WARN, not one per slot per check")
	require.NotContains(t, logs.String(), "slot=", "the entering WARN is agent-wide")

	mu.Lock()
	wsFree = 500
	mu.Unlock()
	wg.Wait()
	require.Equal(t, 1, strings.Count(logs.String(), "free disk space is no longer below minimum"), "one INFO on recovery")
	require.Equal(t, 1, strings.Count(logs.String(), "free disk space below minimum, skipping claim"))
}

func TestWorkspaceGate_RemindsOncePerMinuteWhileBlocked(t *testing.T) {
	logs := captureGateLogs(t)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	g := &workspaceGate{now: func() time.Time { return now }}
	for i := 0; i < 5; i++ {
		g.below("freeBytes", 1)
	}
	now = now.Add(59 * time.Second)
	g.below("freeBytes", 1)
	require.Equal(t, 0, strings.Count(logs.String(), "still below minimum"))
	now = now.Add(2 * time.Second)
	g.below("freeBytes", 1)
	g.below("freeBytes", 1)
	require.Equal(t, 1, strings.Count(logs.String(), "free disk space still below minimum, not claiming"))
	require.Equal(t, 1, strings.Count(logs.String(), "free disk space below minimum, skipping claim"))
	g.ok()
	g.ok()
	require.Equal(t, 1, strings.Count(logs.String(), "no longer below minimum"))
}
