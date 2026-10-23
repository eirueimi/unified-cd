package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/config"
)

// newStorageTestAgent builds an Agent with two declared volumes and a fake
// statfs keyed by path, so a test controls what each filesystem reports.
func newStorageTestAgent(t *testing.T, minFreeDisk uint64, stats func(path string) (free, total uint64, err error)) (*Agent, string) {
	t.Helper()
	ws := t.TempDir()
	vm, err := NewVolumeManager(ws, []config.VolumeConfig{
		{Name: "cache", Mode: VolumeModeExclusive},
		{Name: "data", Mode: VolumeModeShared},
	})
	require.NoError(t, err)
	return &Agent{ID: "a1", Volumes: vm, MinFreeDisk: config.DiskThreshold{Bytes: minFreeDisk, Set: minFreeDisk > 0}, diskStatsFn: stats}, ws
}

func TestHeartbeat_HostReportsWorkspaceAndEachVolume(t *testing.T) {
	var ws string
	var a *Agent
	a, ws = newStorageTestAgent(t, 0, func(path string) (uint64, uint64, error) {
		if path == ws {
			return 40, 100, nil
		}
		return 7, 50, nil
	})

	got := readingOf(a, ws, "host-1")

	require.NotNil(t, got)
	require.Equal(t, "host-1", got.AgentName)
	require.Equal(t, &api.FilesystemStats{FreeBytes: 40, TotalBytes: 100}, got.Workspace)
	require.Len(t, got.Volumes, 2)
	for _, name := range []string{"cache", "data"} {
		v, ok := got.Volumes[name]
		require.True(t, ok, "volume %q missing", name)
		require.EqualValues(t, 7, v.FreeBytes)
		require.EqualValues(t, 50, v.TotalBytes)
		require.LessOrEqual(t, v.FreeBytes, v.TotalBytes)
		require.WithinDuration(t, time.Now(), v.MeasuredAt, time.Minute)
	}
}

func TestHeartbeat_MeasuresEachVolumeAtItsResolvedPath(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	a, ws := newStorageTestAgent(t, 0, func(path string) (uint64, uint64, error) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, path)
		return 1, 2, nil
	})

	readingOf(a, ws, "h")

	require.ElementsMatch(t, []string{ws, a.Volumes.PathOf("cache"), a.Volumes.PathOf("data")}, seen)
	require.Equal(t, filepath.Join(ws, "volumes", "cache"), a.Volumes.PathOf("cache"))
}

func TestHeartbeat_ReportsTheConfiguredThreshold(t *testing.T) {
	const fiveGiB = uint64(5) << 30
	a, ws := newStorageTestAgent(t, fiveGiB, func(string) (uint64, uint64, error) { return 1 << 40, 1 << 41, nil })

	got := readingOf(a, ws, "h")

	require.NotNil(t, got.MinFreeDiskBytes)
	require.Equal(t, fiveGiB, *got.MinFreeDiskBytes)
}

// An unset threshold is reported as 0, not omitted: nil would mean "this agent
// has no such setting" (a Kubernetes agent), and the disabled case is itself a
// signal an operator needs to see.
func TestHeartbeat_UnsetThresholdReportsZero(t *testing.T) {
	a, ws := newStorageTestAgent(t, 0, func(string) (uint64, uint64, error) { return 1, 2, nil })

	got := readingOf(a, ws, "h")

	require.NotNil(t, got.MinFreeDiskBytes, "a host agent always reports its threshold")
	require.EqualValues(t, 0, *got.MinFreeDiskBytes)
	require.NotNil(t, got.ClaimBlockedByDisk, "a host agent always reports whether it is blocked")
	require.False(t, *got.ClaimBlockedByDisk)
}

// The agent reports ClaimBlockedByDisk itself, at the moment its claim loop
// declines, rather than leaving the controller to compare a threshold against
// a reading taken at another time. This drives the real claim loops (two
// slots, so the shared state is exercised under -race) and reads the flag off
// the heartbeats the agent sends.
func TestHeartbeat_ClaimBlockedIsReportedByTheAgent(t *testing.T) {
	oldInterval := heartbeatInterval
	heartbeatInterval = 20 * time.Millisecond
	defer func() { heartbeatInterval = oldInterval }()

	var lastBlocked atomic.Value // *bool, as the last heartbeat carried it
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", registerHandler)
	mux.HandleFunc("POST /api/v1/agents/a-blocked/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		var req api.HeartbeatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Storage != nil {
			lastBlocked.Store(req.Storage.ClaimBlockedByDisk)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/a-blocked/claim", func(w http.ResponseWriter, r *http.Request) {
		drainRequestBody(r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.ClaimResponse{})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var freeMu sync.Mutex
	free := uint64(100) << 20 // below the threshold
	a := &Agent{
		ID:            "a-blocked",
		Client:        NewClient(srv.URL, "tok"),
		MaxConcurrent: 2,
		WorkspaceDir:  t.TempDir(),
		MinFreeDisk:   config.DiskThreshold{Bytes: 1 << 30, Set: true},
		diskStatsFn: func(string) (uint64, uint64, error) {
			freeMu.Lock()
			defer freeMu.Unlock()
			return free, uint64(1) << 40, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer func() { cancel(); <-done }()

	reported := func(want bool) func() bool {
		return func() bool {
			b, _ := lastBlocked.Load().(*bool)
			return b != nil && *b == want
		}
	}
	require.Eventually(t, reported(true), 5*time.Second, 10*time.Millisecond, "a declining agent never reported ClaimBlockedByDisk=true")

	freeMu.Lock()
	free = uint64(2) << 30
	freeMu.Unlock()
	require.Eventually(t, reported(false), 5*time.Second, 10*time.Millisecond, "ClaimBlockedByDisk stayed true after free space recovered")
}

// A slot that exits while blocked must not leave the agent reported as blocked
// for good.
func TestHeartbeat_ClaimBlockedClearsWhenTheDecliningSlotExits(t *testing.T) {
	a := &Agent{
		ID:          "a1",
		MinFreeDisk: config.DiskThreshold{Bytes: 1 << 30, Set: true},
		diskStatsFn: func(string) (uint64, uint64, error) { return 1, 2, nil },
	}
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		a.runLoop(ctx, context.Background(), 0, t.TempDir(), NewRunSet(), NewRunSet(), false)
	}()

	require.Eventually(t, func() bool { return *readingOf(a, "", "h").ClaimBlockedByDisk }, 5*time.Second, 10*time.Millisecond)
	cancel()
	<-exited
	require.False(t, *readingOf(a, "", "h").ClaimBlockedByDisk)
}

// A volume whose statfs fails is omitted: no zero entry, no timestamp. A zero
// would read as "plenty of space". The other volume is still reported.
func TestHeartbeat_AVolumeWhoseStatfsFailsIsOmitted(t *testing.T) {
	var a *Agent
	var ws string
	a, ws = newStorageTestAgent(t, 0, func(path string) (uint64, uint64, error) {
		if path == a.Volumes.PathOf("cache") {
			return 0, 0, errors.New("statfs: boom")
		}
		return 9, 10, nil
	})

	got := readingOf(a, ws, "h")

	require.NotNil(t, got)
	require.NotContains(t, got.Volumes, "cache")
	require.Contains(t, got.Volumes, "data")
	require.EqualValues(t, 9, got.Volumes["data"].FreeBytes)
	require.NotNil(t, got.Workspace)
}

// The workspace is held to the same rule: an unmeasurable one is absent, not
// zero.
func TestHeartbeat_AWorkspaceWhoseStatfsFailsIsOmitted(t *testing.T) {
	var ws string
	var a *Agent
	a, ws = newStorageTestAgent(t, 0, func(path string) (uint64, uint64, error) {
		if path == ws {
			return 0, 0, errors.New("statfs: boom")
		}
		return 9, 10, nil
	})

	got := readingOf(a, ws, "h")

	require.Nil(t, got.Workspace)
	require.Len(t, got.Volumes, 2)
	require.NotNil(t, got.MinFreeDiskBytes)
	require.NotNil(t, got.ClaimBlockedByDisk)
}

// TestStartHeartbeat_SendsStorageFromProvider is the storage counterpart of the
// active-run and pod-binding tests: each beat calls the provider and forwards
// its reading unchanged.
func TestStartHeartbeat_SendsStorageFromProvider(t *testing.T) {
	var lastBody atomic.Value // api.HeartbeatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got api.HeartbeatRequest
		_ = json.NewDecoder(r.Body).Decode(&got)
		lastBody.Store(got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "t")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	storage := &api.AgentStorage{AgentName: "host-1", Workspace: &api.FilesystemStats{FreeBytes: 1, TotalBytes: 2}}
	done := StartHeartbeat(ctx, c, "a1", 20*time.Millisecond, func() []string { return []string{} }, nil, func() *api.AgentStorage { return storage })
	defer func() { cancel(); <-done }()

	require.Eventually(t, func() bool {
		v, ok := lastBody.Load().(api.HeartbeatRequest)
		return ok && v.Storage != nil && v.Storage.AgentName == "host-1" && v.Storage.Workspace != nil && v.Storage.Workspace.FreeBytes == 1
	}, 2*time.Second, 5*time.Millisecond, "heartbeat never carried the provider's storage reading")
}
