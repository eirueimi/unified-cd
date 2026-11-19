package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedBindingRun(t *testing.T, pg *Postgres) string {
	t.Helper()
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "", "")
	require.NoError(t, err)
	return run.ID
}

func TestRunPodBindings_SetSemantics(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	runID := seedBindingRun(t, pg)

	require.NoError(t, pg.ReplaceRunPodBindings(ctx, runID, []RunPodBinding{{PodName: "claim", PodUID: "u1"}, {PodName: "scope", PodUID: "u2"}}))
	list, err := pg.ListRunPodBindings(ctx, runID)
	require.NoError(t, err)
	assert.Len(t, list, 2)

	// A smaller set unbinds the Pod that is missing; an empty one changes nothing.
	require.NoError(t, pg.ReplaceRunPodBindings(ctx, runID, []RunPodBinding{{PodName: "claim", PodUID: "u1"}}))
	require.NoError(t, pg.ReplaceRunPodBindings(ctx, runID, nil))
	list, err = pg.ListRunPodBindings(ctx, runID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "u1", list[0].PodUID)

	// The old single-binding call replaces the whole set.
	require.NoError(t, pg.UpsertRunPodBinding(ctx, runID, "other", "u9"))
	list, err = pg.ListRunPodBindings(ctx, runID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "u9", list[0].PodUID)
	b, ok, err := pg.GetRunPodBinding(ctx, runID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "u9", b.PodUID)

	// Deleting the run clears every row (ON DELETE CASCADE).
	require.NoError(t, pg.ReplaceRunPodBindings(ctx, runID, []RunPodBinding{{PodName: "a", PodUID: "x1"}, {PodName: "b", PodUID: "x2"}}))
	_, err = pg.pool.Exec(ctx, `DELETE FROM runs WHERE id=$1`, runID)
	require.NoError(t, err)
	list, err = pg.ListRunPodBindings(ctx, runID)
	require.NoError(t, err)
	assert.Empty(t, list)
}

// Migration 033 keeps the one-row-per-run data written by older controllers and
// can be reversed.
func TestMigration033RunPodBindingsMultiUpDown(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	up, err := migrationsFS.ReadFile("migrations/033_run_pod_bindings_multi.up.sql")
	require.NoError(t, err)
	down, err := migrationsFS.ReadFile("migrations/033_run_pod_bindings_multi.down.sql")
	require.NoError(t, err)
	runID := seedBindingRun(t, pg)

	require.NoError(t, pg.ReplaceRunPodBindings(ctx, runID, []RunPodBinding{{PodName: "claim", PodUID: "u1"}, {PodName: "scope", PodUID: "u2"}}))
	_, err = pg.pool.Exec(ctx, string(down))
	require.NoError(t, err)
	var n int
	require.NoError(t, pg.pool.QueryRow(ctx, `SELECT count(*) FROM run_pod_bindings WHERE run_id=$1`, runID).Scan(&n))
	assert.Equal(t, 1, n, "down keeps one Pod per run")
	_, err = pg.pool.Exec(ctx, `INSERT INTO run_pod_bindings (run_id, pod_name, pod_uid) VALUES ($1,'x','u3')`, runID)
	assert.Error(t, err, "the old primary key allows one row per run")

	_, err = pg.pool.Exec(ctx, string(up))
	require.NoError(t, err)
	list, err := pg.ListRunPodBindings(ctx, runID)
	require.NoError(t, err)
	assert.Len(t, list, 1, "the surviving old-format row is still read")
	require.NoError(t, pg.ReplaceRunPodBindings(ctx, runID, []RunPodBinding{{PodName: "a", PodUID: "n1"}, {PodName: "b", PodUID: "n2"}}))
}
