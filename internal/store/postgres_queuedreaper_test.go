package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPostgres_ListUnclaimableQueuedRuns verifies the queued-run reaper's query:
// a Queued run is "unclaimable" iff no LIVE agent has labels satisfying its
// agentSelector (empty selector matches any agent).
func TestPostgres_ListUnclaimableQueuedRuns(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	unityRun, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), []string{"unity"}, "")
	require.NoError(t, err)
	anyRun, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "")
	require.NoError(t, err)
	_, err = pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)

	collect := func() map[string][]string {
		refs, err := pg.ListUnclaimableQueuedRuns(ctx, 0, 60*time.Second)
		require.NoError(t, err)
		m := map[string][]string{}
		for _, r := range refs {
			m[r.ID] = r.AgentSelector
		}
		return m
	}

	// No agents at all: both queued runs are unclaimable.
	m := collect()
	require.Contains(t, m, unityRun.ID)
	assert.Equal(t, []string{"unity"}, m[unityRun.ID])
	require.Contains(t, m, anyRun.ID)

	// A live docker agent (no unity label): the empty-selector run becomes
	// claimable; the unity run still cannot be claimed.
	require.NoError(t, pg.UpsertAgent(ctx, "docker-1", "h", "linux", "dev", []string{"kind:docker"}, nil))
	m = collect()
	assert.Contains(t, m, unityRun.ID)
	assert.NotContains(t, m, anyRun.ID)

	// A live unity agent: the unity run is now claimable too.
	require.NoError(t, pg.UpsertAgent(ctx, "unity-1", "h2", "windows", "dev", []string{"unity", "macos"}, nil))
	m = collect()
	assert.NotContains(t, m, unityRun.ID)
	assert.NotContains(t, m, anyRun.ID)
}
