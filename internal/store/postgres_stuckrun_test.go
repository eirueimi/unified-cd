package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListStuckRuns(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertJob(ctx, "hello", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)

	// stuckRun: claimed long ago by an agent that hasn't been seen in a while.
	stuckRun, err := pg.CreateRun(ctx, "hello", nil, []byte(`{}`), nil, nil, "", "")
	require.NoError(t, err)
	stuckRunID := stuckRun.ID

	// freshRun: claimed long ago, but the claiming agent has a fresh heartbeat.
	freshRun, err := pg.CreateRun(ctx, "hello", nil, []byte(`{}`), nil, nil, "", "")
	require.NoError(t, err)
	freshRunID := freshRun.ID

	// recentRun: claimed just now by a stale agent -- still within the grace window.
	recentRun, err := pg.CreateRun(ctx, "hello", nil, []byte(`{}`), nil, nil, "", "")
	require.NoError(t, err)
	recentRunID := recentRun.ID

	// pendingRun: never claimed, still Pending.
	pendingRun, err := pg.CreateRun(ctx, "hello", nil, []byte(`{}`), nil, nil, "", "")
	require.NoError(t, err)
	pendingRunID := pendingRun.ID

	require.NoError(t, pg.UpsertAgent(ctx, "agent-stale", "host", "linux", "dev", nil, nil, nil))
	require.NoError(t, pg.UpsertAgent(ctx, "agent-fresh", "host", "linux", "dev", nil, nil, nil))

	_, err = pg.pool.Exec(ctx,
		`UPDATE agents SET last_seen_at = NOW() - interval '5 minutes' WHERE id = $1`, "agent-stale")
	require.NoError(t, err)

	_, err = pg.pool.Exec(ctx,
		`UPDATE runs SET status = 'Running', claimed_by = $1, claimed_at = NOW() - interval '5 minutes' WHERE id = $2`,
		"agent-stale", stuckRunID)
	require.NoError(t, err)

	_, err = pg.pool.Exec(ctx,
		`UPDATE runs SET status = 'Running', claimed_by = $1, claimed_at = NOW() - interval '5 minutes' WHERE id = $2`,
		"agent-fresh", freshRunID)
	require.NoError(t, err)

	_, err = pg.pool.Exec(ctx,
		`UPDATE runs SET status = 'Running', claimed_by = $1, claimed_at = NOW() WHERE id = $2`,
		"agent-stale", recentRunID)
	require.NoError(t, err)

	_ = pendingRunID // stays Pending; no update needed

	refs, err := pg.ListStuckRuns(ctx, 90*time.Second, 60*time.Second)
	require.NoError(t, err)
	byID := stuckByID(refs)
	assert.Contains(t, byID, stuckRunID)
	assert.NotContains(t, byID, freshRunID)
	assert.NotContains(t, byID, recentRunID)
	assert.NotContains(t, byID, pendingRunID)
	// The stale-heartbeat run has an agents row, so it must NOT be reported as
	// "agent missing" — the reaper's confirmation delay applies only to the
	// evidence-free branch, and mislabelling here would slow every real reap.
	assert.False(t, byID[stuckRunID], "a stale heartbeat is not a missing row")
}

func stuckByID(refs []StuckRunRef) map[string]bool {
	m := map[string]bool{}
	for _, r := range refs {
		m[r.ID] = r.AgentMissing
	}
	return m
}

func TestListStuckRuns_MissingAgentCountsAsLost(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertJob(ctx, "hello", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)

	orphanRun, err := pg.CreateRun(ctx, "hello", nil, []byte(`{}`), nil, nil, "", "")
	require.NoError(t, err)
	orphanRunID := orphanRun.ID

	// claimed_by references an agent that no longer exists (e.g. deleted by DeleteStaleAgents).
	_, err = pg.pool.Exec(ctx,
		`UPDATE runs SET status = 'Running', claimed_by = $1, claimed_at = NOW() - interval '5 minutes' WHERE id = $2`,
		"agent-deleted", orphanRunID)
	require.NoError(t, err)

	refs, err := pg.ListStuckRuns(ctx, 90*time.Second, 60*time.Second)
	require.NoError(t, err)
	byID := stuckByID(refs)
	assert.Contains(t, byID, orphanRunID)
	// ...but it must be flagged so the reaper knows this match rests on the absence
	// of a row rather than on a measured silence, and confirms it before acting.
	assert.True(t, byID[orphanRunID], "a missing agents row must be reported as AgentMissing")
}

// A heartbeat must be able to RECREATE a deleted agents row, not merely refresh an
// existing one. The row is not owned by the process heartbeating into it — a
// duplicate-ID sibling's ordinary deregistration deletes it — and while it was
// absent the stuck-run reaper failed the healthy agent's run as "agent lost".
func TestTouchAgent_RecreatesDeletedRowSoLivenessIsNotLost(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	require.NoError(t, pg.UpsertAgent(ctx, "a1", "host1", "linux", "dev", []string{"kind:linux"}, []string{"native"}, nil))

	recreated, err := pg.TouchAgent(ctx, "a1")
	require.NoError(t, err)
	assert.False(t, recreated, "a heartbeat into an existing row is an ordinary refresh")

	// Exactly what DeleteAgent (agent deregistration) and DeleteStaleAgents run.
	require.NoError(t, pg.DeleteAgent(ctx, "a1"))
	a, err := pg.GetAgent(ctx, "a1")
	require.NoError(t, err)
	require.Nil(t, a)

	recreated, err = pg.TouchAgent(ctx, "a1")
	require.NoError(t, err)
	assert.True(t, recreated, "the heartbeat must report that it restored a missing row")

	a, err = pg.GetAgent(ctx, "a1")
	require.NoError(t, err)
	require.NotNil(t, a, "a live agent's heartbeat must restore its inventory row")
}

// The end-to-end shape of the reported defect, at the store layer: a run claimed by
// a healthy, heartbeating agent whose agents row was deleted by something else must
// stop matching the reaper's predicate as soon as the agent heartbeats again.
func TestListStuckRuns_HeartbeatAfterRowDeletionClearsTheOrphanMatch(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertJob(ctx, "hello", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(ctx, "hello", nil, []byte(`{}`), nil, nil, "", "")
	require.NoError(t, err)

	require.NoError(t, pg.UpsertAgent(ctx, "agent1", "host", "linux", "dev", nil, nil, nil))
	_, err = pg.pool.Exec(ctx,
		`UPDATE runs SET status = 'Running', claimed_by = $1, claimed_at = NOW() - interval '5 minutes' WHERE id = $2`,
		"agent1", run.ID)
	require.NoError(t, err)

	// Healthy agent, fresh heartbeat: not stuck.
	refs, err := pg.ListStuckRuns(ctx, 90*time.Second, 60*time.Second)
	require.NoError(t, err)
	assert.NotContains(t, stuckByID(refs), run.ID)

	// A duplicate-ID sibling finishes draining and deregisters, deleting the row
	// the live process is using.
	require.NoError(t, pg.DeleteAgent(ctx, "agent1"))
	refs, err = pg.ListStuckRuns(ctx, 90*time.Second, 60*time.Second)
	require.NoError(t, err)
	byID := stuckByID(refs)
	require.Contains(t, byID, run.ID)
	assert.True(t, byID[run.ID], "the match rests only on the row's absence")

	// The agent's very next heartbeat answers for itself.
	recreated, err := pg.TouchAgent(ctx, "agent1")
	require.NoError(t, err)
	require.True(t, recreated)
	refs, err = pg.ListStuckRuns(ctx, 90*time.Second, 60*time.Second)
	require.NoError(t, err)
	assert.NotContains(t, stuckByID(refs), run.ID,
		"a heartbeating agent's run must stop looking orphaned once the heartbeat restores the row")
}
