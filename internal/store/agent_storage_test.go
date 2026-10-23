package store

import (
	"context"
	"encoding/json"
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

func u64p(v uint64) *uint64 { return &v }
func boolp(v bool) *bool    { return &v }

func TestSetAgentStorage_RoundTripsEveryField(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	require.NoError(t, pg.UpsertAgent(ctx, "agent-1", "host1", "linux", "v1", nil, nil, nil, nil))

	at := time.Now().UTC().Truncate(time.Millisecond)
	want := api.AgentStorage{
		AgentName:          "host1",
		Workspace:          &api.FilesystemStats{FreeBytes: 10, TotalBytes: 100},
		Volumes:            map[string]api.VolumeStats{"cache": volumeReading(1, 2, at)},
		MinFreeDiskBytes:   u64p(5),
		ClaimBlockedByDisk: boolp(true),
	}
	require.NoError(t, pg.SetAgentStorage(ctx, "agent-1", &want))

	got, err := pg.ListAgentStorage(ctx, storageStaleAfter)
	require.NoError(t, err)
	require.Contains(t, got, "agent-1")
	g := got["agent-1"]
	assert.Equal(t, "host1", g.AgentName)
	assert.Equal(t, want.Workspace, g.Workspace)
	require.NotNil(t, g.MinFreeDiskBytes)
	assert.Equal(t, uint64(5), *g.MinFreeDiskBytes)
	require.NotNil(t, g.ClaimBlockedByDisk)
	assert.True(t, *g.ClaimBlockedByDisk)
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
	require.NoError(t, pg.SetAgentStorage(ctx, "k8s-1", &api.AgentStorage{
		AgentName: "builders",
		Volumes:   map[string]api.VolumeStats{"cache": volumeReading(7, 9, at)},
	}))

	got, err := pg.ListAgentStorage(ctx, storageStaleAfter)
	require.NoError(t, err)
	require.Contains(t, got, "k8s-1")
	g := got["k8s-1"]
	assert.Nil(t, g.Workspace, "no workspace filesystem must not become a zero-valued one")
	assert.Nil(t, g.MinFreeDiskBytes, "a Kubernetes agent has no minFreeDisk setting; that must not become a configured 0")
	assert.Nil(t, g.ClaimBlockedByDisk, "a Kubernetes agent does no claim-time disk check; that must not become 'checked, not blocked'")
	assert.Len(t, g.Volumes, 1)
}

// A host agent whose minFreeDisk is 0 (the check is disabled) and which is not
// blocked must read back with all three of workspace, minFreeDisk and
// claimBlocked present: the disabled case is itself a warning an operator needs
// to see, so it must stay distinguishable from the Kubernetes-shaped reading
// where those are absent.
func TestSetAgentStorage_ConfiguredZeroIsNotAbsent(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	require.NoError(t, pg.UpsertAgent(ctx, "host-1", "h", "linux", "v1", nil, nil, nil, nil))
	require.NoError(t, pg.UpsertAgent(ctx, "k8s-1", "", "linux", "v1", nil, nil, nil, nil))

	require.NoError(t, pg.SetAgentStorage(ctx, "host-1", &api.AgentStorage{
		Workspace:          &api.FilesystemStats{FreeBytes: 3, TotalBytes: 4},
		MinFreeDiskBytes:   u64p(0),
		ClaimBlockedByDisk: boolp(false),
	}))
	require.NoError(t, pg.SetAgentStorage(ctx, "k8s-1", &api.AgentStorage{AgentName: "builders"}))

	got, err := pg.ListAgentStorage(ctx, storageStaleAfter)
	require.NoError(t, err)

	host := got["host-1"]
	require.NotNil(t, host.Workspace)
	require.NotNil(t, host.MinFreeDiskBytes, "minFreeDisk configured as 0 must stay present")
	assert.Equal(t, uint64(0), *host.MinFreeDiskBytes)
	require.NotNil(t, host.ClaimBlockedByDisk, "'checked, not blocked' must stay present")
	assert.False(t, *host.ClaimBlockedByDisk)
	assert.Empty(t, host.Volumes)

	k8s := got["k8s-1"]
	assert.Nil(t, k8s.Workspace)
	assert.Nil(t, k8s.MinFreeDiskBytes)
	assert.Nil(t, k8s.ClaimBlockedByDisk)
}

// A host agent whose workspace statfs failed has no Workspace but still
// performs the claim check, and its blocked flag matters most then. Presence of
// the flag must not depend on the workspace figure.
func TestSetAgentStorage_ClaimFlagsAreIndependentOfWorkspace(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	require.NoError(t, pg.UpsertAgent(ctx, "host-1", "h", "linux", "v1", nil, nil, nil, nil))
	require.NoError(t, pg.SetAgentStorage(ctx, "host-1", &api.AgentStorage{
		MinFreeDiskBytes:   u64p(1000),
		ClaimBlockedByDisk: boolp(true),
	}))

	got, err := pg.ListAgentStorage(ctx, storageStaleAfter)
	require.NoError(t, err)
	g := got["host-1"]
	assert.Nil(t, g.Workspace)
	require.NotNil(t, g.MinFreeDiskBytes)
	assert.Equal(t, uint64(1000), *g.MinFreeDiskBytes)
	require.NotNil(t, g.ClaimBlockedByDisk)
	assert.True(t, *g.ClaimBlockedByDisk)
}

// A heartbeat that carried no reading writes nil. That must clear what was
// stored, or an agent that is still heartbeating (after a downgrade, or when its
// measurement gives up) would keep exporting its last reading indefinitely.
func TestSetAgentStorage_NilClearsTheReading(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	require.NoError(t, pg.UpsertAgent(ctx, "agent-1", "h", "linux", "v1", nil, nil, nil, nil))
	require.NoError(t, pg.SetAgentStorage(ctx, "agent-1", &api.AgentStorage{
		Workspace: &api.FilesystemStats{FreeBytes: 1, TotalBytes: 2},
	}))
	got, err := pg.ListAgentStorage(ctx, storageStaleAfter)
	require.NoError(t, err)
	require.Contains(t, got, "agent-1")

	require.NoError(t, pg.SetAgentStorage(ctx, "agent-1", nil))

	got, err = pg.ListAgentStorage(ctx, storageStaleAfter)
	require.NoError(t, err)
	assert.NotContains(t, got, "agent-1", "a cleared reading must leave no series, though the agent is live")

	var isNull bool
	require.NoError(t, pg.pool.QueryRow(ctx, `SELECT storage IS NULL FROM agents WHERE id = $1`, "agent-1").Scan(&isNull))
	assert.True(t, isNull, "nil must be stored as SQL NULL, not as the JSON value null")
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
	require.NoError(t, pg.SetAgentStorage(ctx, "agent-1", &api.AgentStorage{
		Workspace: &api.FilesystemStats{FreeBytes: 1, TotalBytes: 2},
		Volumes: map[string]api.VolumeStats{
			"a": volumeReading(1, 10, at),
			"b": volumeReading(2, 10, at),
		},
		MinFreeDiskBytes:   u64p(50),
		ClaimBlockedByDisk: boolp(true),
	}))
	require.NoError(t, pg.SetAgentStorage(ctx, "agent-1", &api.AgentStorage{
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
	assert.Nil(t, g.MinFreeDiskBytes)
	assert.Nil(t, g.ClaimBlockedByDisk)
}

func TestListAgentStorage_OmitsAgentsPastStaleAfter(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	for _, id := range []string{"fresh", "stale"} {
		require.NoError(t, pg.UpsertAgent(ctx, id, "h", "linux", "v1", nil, nil, nil, nil))
		require.NoError(t, pg.SetAgentStorage(ctx, id, &api.AgentStorage{
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
	require.NoError(t, pg.SetAgentStorage(ctx, "agent-1", &api.AgentStorage{
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

	err := pg.SetAgentStorage(ctx, "ghost", &api.AgentStorage{
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

	require.NoError(t, pg.SetAgentStorage(ctx, "agent-1", &api.AgentStorage{
		Workspace: &api.FilesystemStats{FreeBytes: 1, TotalBytes: 2},
	}))

	got, err := pg.ListAgentStorage(ctx, storageStaleAfter)
	require.NoError(t, err)
	assert.NotContains(t, got, "agent-1")
}

func TestSetAgentStorage_RoundTripsVolumeClaimBlocked(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	require.NoError(t, pg.UpsertAgent(ctx, "agent-1", "host1", "linux", "v1", nil, nil, nil, nil))

	at := time.Now().UTC().Truncate(time.Millisecond)
	blocked := volumeReading(1, 100, at)
	blocked.ClaimBlocked = true
	require.NoError(t, pg.SetAgentStorage(ctx, "agent-1", &api.AgentStorage{
		AgentName: "host1",
		Volumes:   map[string]api.VolumeStats{"full": blocked, "roomy": volumeReading(90, 100, at)},
	}))

	got, err := pg.ListAgentStorage(ctx, storageStaleAfter)
	require.NoError(t, err)
	assert.True(t, got["agent-1"].Volumes["full"].ClaimBlocked)
	assert.False(t, got["agent-1"].Volumes["roomy"].ClaimBlocked)
}

// A reading written by an agent that predates the field has no claimBlocked key.
func TestListAgentStorage_MissingClaimBlockedKeyIsFalse(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	require.NoError(t, pg.UpsertAgent(ctx, "agent-1", "host1", "linux", "v1", nil, nil, nil, nil))

	var old api.AgentStorage
	require.NoError(t, json.Unmarshal([]byte(`{"agentName":"host1","volumes":{"cache":{"freeBytes":1,"totalBytes":2,"measuredAt":"2026-10-21T09:00:00Z"}}}`), &old))
	require.NoError(t, pg.SetAgentStorage(ctx, "agent-1", &old))

	got, err := pg.ListAgentStorage(ctx, storageStaleAfter)
	require.NoError(t, err)
	require.Contains(t, got["agent-1"].Volumes, "cache")
	assert.False(t, got["agent-1"].Volumes["cache"].ClaimBlocked)
}
