package store

import (
	"context"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const storageStaleAfter = 90 * time.Second

func volumeReading(free, total uint64, at time.Time) api.VolumeStats {
	return api.VolumeStats{
		FilesystemStats: api.FilesystemStats{FreeBytes: free, TotalBytes: total},
		MeasuredAt:      at,
	}
}

func TestSetAgentStorage_RoundTripsEveryField(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	require.NoError(t, pg.UpsertAgent(ctx, "agent-1", "host1", "linux", "v1", nil, nil, nil, nil))

	at := time.Now().UTC().Truncate(time.Millisecond)
	want := api.AgentStorage{
		AgentName:          "host1",
		Workspace:          &api.FilesystemStats{FreeBytes: 10, TotalBytes: 100},
		Volumes:            map[string]api.VolumeStats{"cache": volumeReading(1, 2, at)},
		MinFreeDiskBytes:   5,
		ClaimBlockedByDisk: true,
	}
	require.NoError(t, pg.SetAgentStorage(ctx, "agent-1", want))

	got, err := pg.ListAgentStorage(ctx, storageStaleAfter)
	require.NoError(t, err)
	require.Contains(t, got, "agent-1")
	g := got["agent-1"]
	assert.Equal(t, "host1", g.AgentName)
	assert.Equal(t, want.Workspace, g.Workspace)
	assert.Equal(t, uint64(5), g.MinFreeDiskBytes)
	assert.True(t, g.ClaimBlockedByDisk)
	require.Contains(t, g.Volumes, "cache")
	assert.Equal(t, uint64(1), g.Volumes["cache"].FreeBytes)
	assert.Equal(t, uint64(2), g.Volumes["cache"].TotalBytes)
	assert.True(t, at.Equal(g.Volumes["cache"].MeasuredAt), "MeasuredAt must survive the round trip")
}

// An agent that cannot answer part of the reading (a Kubernetes agent has no
// workspace filesystem and no minFreeDisk) must read back with that part absent,
// never as a zero: a zero would look like "plenty of space".
func TestSetAgentStorage_AbsentStaysAbsent(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	require.NoError(t, pg.UpsertAgent(ctx, "k8s-1", "", "linux", "v1", nil, nil, nil, nil))

	at := time.Now().UTC()
	require.NoError(t, pg.SetAgentStorage(ctx, "k8s-1", api.AgentStorage{
		AgentName: "builders",
		Volumes:   map[string]api.VolumeStats{"cache": volumeReading(7, 9, at)},
	}))

	got, err := pg.ListAgentStorage(ctx, storageStaleAfter)
	require.NoError(t, err)
	require.Contains(t, got, "k8s-1")
	g := got["k8s-1"]
	assert.Nil(t, g.Workspace, "no workspace filesystem must not become a zero-valued one")
	assert.Zero(t, g.MinFreeDiskBytes)
	assert.False(t, g.ClaimBlockedByDisk)
	assert.Len(t, g.Volumes, 1)

	// And the other way round: a host agent with a workspace but no named volumes.
	require.NoError(t, pg.UpsertAgent(ctx, "host-1", "h", "linux", "v1", nil, nil, nil, nil))
	require.NoError(t, pg.SetAgentStorage(ctx, "host-1", api.AgentStorage{
		Workspace: &api.FilesystemStats{FreeBytes: 3, TotalBytes: 4},
	}))
	got, err = pg.ListAgentStorage(ctx, storageStaleAfter)
	require.NoError(t, err)
	require.Contains(t, got, "host-1")
	assert.Empty(t, got["host-1"].Volumes)
	require.NotNil(t, got["host-1"].Workspace)
}

// An agent that has never reported has no reading, which is different from a
// reading of zeros: it must be absent from the result.
func TestListAgentStorage_OmitsAnAgentThatNeverReported(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	require.NoError(t, pg.UpsertAgent(ctx, "silent", "h", "linux", "v1", nil, nil, nil, nil))

	got, err := pg.ListAgentStorage(ctx, storageStaleAfter)
	require.NoError(t, err)
	assert.NotContains(t, got, "silent")
}

func TestSetAgentStorage_ReplacesTheWholeReading(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	require.NoError(t, pg.UpsertAgent(ctx, "agent-1", "h", "linux", "v1", nil, nil, nil, nil))

	at := time.Now().UTC()
	require.NoError(t, pg.SetAgentStorage(ctx, "agent-1", api.AgentStorage{
		Workspace: &api.FilesystemStats{FreeBytes: 1, TotalBytes: 2},
		Volumes: map[string]api.VolumeStats{
			"a": volumeReading(1, 10, at),
			"b": volumeReading(2, 10, at),
		},
		MinFreeDiskBytes:   50,
		ClaimBlockedByDisk: true,
	}))
	require.NoError(t, pg.SetAgentStorage(ctx, "agent-1", api.AgentStorage{
		Volumes: map[string]api.VolumeStats{"a": volumeReading(3, 10, at)},
	}))

	got, err := pg.ListAgentStorage(ctx, storageStaleAfter)
	require.NoError(t, err)
	require.Contains(t, got, "agent-1")
	g := got["agent-1"]
	assert.Len(t, g.Volumes, 1, "a volume dropped from the configuration must lose its row")
	assert.Contains(t, g.Volumes, "a")
	assert.NotContains(t, g.Volumes, "b")
	assert.Equal(t, uint64(3), g.Volumes["a"].FreeBytes)
	assert.Nil(t, g.Workspace, "the new snapshot has no workspace; the old one must not survive")
	assert.Zero(t, g.MinFreeDiskBytes)
	assert.False(t, g.ClaimBlockedByDisk)
}

func TestListAgentStorage_OmitsAgentsPastStaleAfter(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	for _, id := range []string{"fresh", "stale"} {
		require.NoError(t, pg.UpsertAgent(ctx, id, "h", "linux", "v1", nil, nil, nil, nil))
		require.NoError(t, pg.SetAgentStorage(ctx, id, api.AgentStorage{
			Workspace: &api.FilesystemStats{FreeBytes: 1, TotalBytes: 2},
		}))
	}
	_, err := pg.pool.Exec(ctx,
		`UPDATE agents SET last_seen_at = NOW() - interval '10 minutes' WHERE id = $1`, "stale")
	require.NoError(t, err)

	got, err := pg.ListAgentStorage(ctx, storageStaleAfter)
	require.NoError(t, err)
	assert.Contains(t, got, "fresh")
	assert.NotContains(t, got, "stale")
}

// The reading lives on the agent's row, so deleting the agent deletes it. The
// metrics collector must never keep exporting a series for an agent that is gone.
func TestListAgentStorage_OmitsADeletedAgent(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	require.NoError(t, pg.UpsertAgent(ctx, "agent-1", "h", "linux", "v1", nil, nil, nil, nil))
	require.NoError(t, pg.SetAgentStorage(ctx, "agent-1", api.AgentStorage{
		Workspace: &api.FilesystemStats{FreeBytes: 1, TotalBytes: 2},
	}))
	require.NoError(t, pg.DeleteAgent(ctx, "agent-1"))

	got, err := pg.ListAgentStorage(ctx, storageStaleAfter)
	require.NoError(t, err)
	assert.NotContains(t, got, "agent-1")

	// A heartbeat re-creates the row (TouchAgent); the old reading must not
	// come back with it.
	_, err = pg.TouchAgent(ctx, "agent-1")
	require.NoError(t, err)
	got, err = pg.ListAgentStorage(ctx, storageStaleAfter)
	require.NoError(t, err)
	assert.NotContains(t, got, "agent-1")
}

// handleAgentHeartbeat calls TouchAgent first, which re-creates a missing row,
// so a write for an unknown agent is only reachable when a deregister races in
// between. That is benign and not worth failing a heartbeat over.
func TestSetAgentStorage_UnknownAgentIsANoOp(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	err := pg.SetAgentStorage(ctx, "ghost", api.AgentStorage{
		Workspace: &api.FilesystemStats{FreeBytes: 1, TotalBytes: 2},
	})
	require.NoError(t, err)

	var n int
	require.NoError(t, pg.pool.QueryRow(ctx, `SELECT COUNT(*) FROM agents`).Scan(&n))
	assert.Zero(t, n, "a storage write must not create an agent row")
	got, err := pg.ListAgentStorage(ctx, storageStaleAfter)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// A storage write is not a heartbeat: it must not extend the agent's liveness.
func TestSetAgentStorage_DoesNotRefreshLastSeen(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	require.NoError(t, pg.UpsertAgent(ctx, "agent-1", "h", "linux", "v1", nil, nil, nil, nil))
	_, err := pg.pool.Exec(ctx,
		`UPDATE agents SET last_seen_at = NOW() - interval '10 minutes' WHERE id = $1`, "agent-1")
	require.NoError(t, err)

	require.NoError(t, pg.SetAgentStorage(ctx, "agent-1", api.AgentStorage{
		Workspace: &api.FilesystemStats{FreeBytes: 1, TotalBytes: 2},
	}))

	got, err := pg.ListAgentStorage(ctx, storageStaleAfter)
	require.NoError(t, err)
	assert.NotContains(t, got, "agent-1")
}
