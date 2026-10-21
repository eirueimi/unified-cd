package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/config"
)

// readingOf samples every path of a once, waits for the measurements to land,
// and returns the reading the heartbeat would send. The sampler's interval is
// long, so nothing it measured goes stale during the test.
func readingOf(a *Agent, wsBase, agentName string) *api.AgentStorage {
	s := newStorageSampler(a.statfs(), DefaultHeartbeatInterval)
	s.sampleAll(a.storagePaths(wsBase))
	s.wg.Wait()
	return a.storageReading(s, wsBase, agentName)
}

// fakeClock is a settable clock for aging measurements without sleeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// A path that never returns costs one parked goroutine, not one more every
// interval, and a measurement older than two intervals is dropped rather than
// reported.
func TestStorageSampler_OneInFlightPerPathAndStaleEntriesAreDropped(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var calls atomic.Int32
	stat := func(string) (uint64, uint64, error) {
		if calls.Add(1) == 1 {
			return 5, 10, nil
		}
		<-release // every later measurement hangs, like statfs on a dead mount
		return 0, 0, nil
	}
	clock := &fakeClock{t: time.Unix(1_000_000, 0)}
	s := newStorageSampler(stat, time.Second)
	s.now = clock.now
	paths := []string{"/p"}

	s.sampleAll(paths)
	s.wg.Wait() // the first call returns
	free, total, _, ok := s.read("/p")
	require.True(t, ok)
	require.EqualValues(t, 5, free)
	require.EqualValues(t, 10, total)

	// Within two intervals the old number is still served.
	clock.advance(2 * time.Second)
	_, _, _, ok = s.read("/p")
	require.True(t, ok, "a reading exactly two intervals old is still fresh")

	// The next sample hangs; sampling again and again must not start more.
	for i := 0; i < 5; i++ {
		s.sampleAll(paths)
	}
	require.Eventually(t, func() bool { return calls.Load() == 2 }, 2*time.Second, 5*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	require.EqualValues(t, 2, calls.Load(), "a hung path must have at most one measurement in flight")

	// Past two intervals the stuck path is omitted, not reported stale.
	clock.advance(time.Second)
	_, _, _, ok = s.read("/p")
	require.False(t, ok, "a reading older than two intervals must be omitted")
}

// The workspace is held to the same staleness rule as a volume.
func TestStorageSampler_AStaleWorkspaceIsNilledOut(t *testing.T) {
	a, ws := newStorageTestAgent(t, 0, func(string) (uint64, uint64, error) { return 5, 10, nil })
	clock := &fakeClock{t: time.Unix(1_000_000, 0)}
	s := newStorageSampler(a.statfs(), time.Second)
	s.now = clock.now
	s.sampleAll(a.storagePaths(ws))
	s.wg.Wait()
	require.NotNil(t, a.storageReading(s, ws, "h").Workspace)

	clock.advance(3 * time.Second)
	got := a.storageReading(s, ws, "h")
	require.Nil(t, got.Workspace)
	require.Empty(t, got.Volumes)
	require.NotNil(t, got.MinFreeDiskBytes, "the agent's own settings are not measurements and are not dropped")
}

// The point of moving the measurement off the heartbeat goroutine: a statfs
// that never returns (a hard-mounted NFS export whose server is gone) must not
// stop the beats, must not leak into the reading, and must not hold up
// shutdown.
func TestHeartbeat_ABlockedStatfsDoesNotStallBeatsOrShutdown(t *testing.T) {
	oldInterval := heartbeatInterval
	heartbeatInterval = 20 * time.Millisecond
	defer func() { heartbeatInterval = oldInterval }()

	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // free the parked goroutines so nothing leaks

	var hits atomic.Int32
	var cacheEverSeen atomic.Bool
	var dataSeen atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", registerHandler)
	mux.HandleFunc("POST /api/v1/agents/a-hung/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		var req api.HeartbeatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		hits.Add(1)
		if req.Storage != nil {
			if _, ok := req.Storage.Volumes["cache"]; ok {
				cacheEverSeen.Store(true)
			}
			if _, ok := req.Storage.Volumes["data"]; ok {
				dataSeen.Store(true)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/a-hung/claim", func(w http.ResponseWriter, r *http.Request) {
		drainRequestBody(r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.ClaimResponse{})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ws := t.TempDir()
	vm, err := NewVolumeManager(ws, []config.VolumeConfig{
		{Name: "cache", Mode: VolumeModeExclusive},
		{Name: "data", Mode: VolumeModeShared},
	})
	require.NoError(t, err)
	cachePath := vm.PathOf("cache")
	a := &Agent{
		ID:            "a-hung",
		Client:        NewClient(srv.URL, "tok"),
		MaxConcurrent: 1,
		WorkspaceDir:  ws,
		Volumes:       vm,
		diskStatsFn: func(path string) (uint64, uint64, error) {
			if path == cachePath {
				<-release
				return 0, 0, nil
			}
			return 7, 50, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	// Beats keep flowing at the normal cadence: 20ms intervals, so well over
	// ten beats fit in the window even on a slow runner.
	require.Eventually(t, func() bool { return hits.Load() >= 10 }, 5*time.Second, 10*time.Millisecond,
		"heartbeats stalled behind a hung statfs")
	require.Eventually(t, dataSeen.Load, 5*time.Second, 10*time.Millisecond, "the healthy volume was not reported")
	require.False(t, cacheEverSeen.Load(), "the hung volume must be absent from every reading")

	// Shutdown does not wait for the parked measurement.
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return while a statfs was hung")
	}
}

// The blocked flag is a count of declining slots, not a shared bool. One slot
// recovers while the other still sees low space: the agent must keep
// reporting blocked, which a last-writer-wins flag would not.
func TestHeartbeat_ClaimBlockedStaysTrueWhileAnyOneSlotIsStillDeclining(t *testing.T) {
	claimParked := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drainRequestBody(r)
		select { // the recovered slot waits here, so it stops checking the gate
		case <-r.Context().Done():
		case <-claimParked:
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.ClaimResponse{})
	}))
	defer srv.Close()
	defer close(claimParked)

	// Calls 1 and 2 are the two slots' first checks, call 3 is one slot's
	// second check (still low), and call 4 is the other slot's second check:
	// it recovers and parks in Claim. The still-declining slot checks again
	// only after its 2s sleep, so a last-writer-wins bool would read false
	// for that whole window.
	var calls atomic.Int32
	a := &Agent{
		ID:          "a1",
		Client:      NewClient(srv.URL, "t"),
		MinFreeDisk: uint64(1) << 30,
		diskStatsFn: func(string) (uint64, uint64, error) {
			if calls.Add(1) == 4 {
				return uint64(2) << 30, uint64(1) << 40, nil
			}
			return 1, uint64(1) << 40, nil
		},
	}
	claimCtx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for slot := 0; slot < 2; slot++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.runLoop(claimCtx, context.Background(), slot, t.TempDir(), NewRunSet(), NewRunSet(), false)
		}()
	}
	defer func() { cancel(); wg.Wait() }()

	require.Eventually(t, func() bool { return calls.Load() >= 4 }, 10*time.Second, 10*time.Millisecond)
	idle := newStorageSampler(a.statfs(), DefaultHeartbeatInterval) // never sampled: only the flag is read
	blocked := func() bool { return *a.storageReading(idle, "", "h").ClaimBlockedByDisk }
	require.Never(t, func() bool { return !blocked() }, time.Second, 10*time.Millisecond,
		"the agent reported unblocked while a slot was still declining")
	require.True(t, blocked())
}
