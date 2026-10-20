package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
)

// queueRunWithRequiredVolumes creates a run, moves it to Queued, then forces its
// required_volumes column directly (queueing writes the column itself, so the
// override must come after it). Writing the column by hand keeps
// the claim-routing tests independent of the queue-time expansion, which has
// its own tests below.
func queueRunWithRequiredVolumes(t *testing.T, pg *Postgres, required string) string {
	t.Helper()
	ctx := context.Background()
	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), []string{}, []string{}, "test", "")
	require.NoError(t, err)
	_, err = pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)
	_, err = pg.pool.Exec(ctx, `UPDATE runs SET required_volumes = $1::jsonb WHERE id = $2`, required, run.ID)
	require.NoError(t, err)
	return run.ID
}

func TestClaimNextRun_VolumeRouting(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}

	cases := []struct {
		name      string
		advertise []string
		busy      []string
		required  string
		claimed   bool
	}{
		{"single name advertised", []string{"a"}, nil, `[["a"]]`, true},
		{"single name not advertised", []string{"a"}, nil, `[["b"]]`, false},
		{"any-of candidates, one advertised", []string{"a"}, nil, `[["a","b"]]`, true},
		{"all entries satisfied", []string{"a", "b"}, nil, `[["a"],["b"]]`, true},
		{"one entry unsatisfied", []string{"a"}, nil, `[["a"],["b"]]`, false},
		{"only matching volume is busy", []string{"a", "b"}, []string{"a"}, `[["a"]]`, false},
		{"any-of falls back to the free candidate", []string{"a", "b"}, []string{"a"}, `[["a","b"]]`, true},
		{"no required volumes, none advertised", []string{}, nil, `[]`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pg := NewTestPostgres(t)
			ctx := context.Background()
			require.NoError(t, pg.UpsertAgent(ctx, "ag", "h", "linux", "", []string{}, []string{"native"}, tc.advertise, nil))
			runID := queueRunWithRequiredVolumes(t, pg, tc.required)

			got, err := pg.ClaimNextRun(ctx, "ag", []string{}, tc.busy)
			require.NoError(t, err)
			if tc.claimed {
				require.NotNil(t, got)
				assert.Equal(t, runID, got.ID)
			} else {
				assert.Nil(t, got)
			}
		})
	}
}

func TestClaimNextRun_VolumeRouting_PicksTheAgentThatAdvertises(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	pg := NewTestPostgres(t)
	ctx := context.Background()
	require.NoError(t, pg.UpsertAgent(ctx, "without", "h1", "linux", "", []string{}, []string{"native"}, []string{}, nil))
	require.NoError(t, pg.UpsertAgent(ctx, "with", "h2", "linux", "", []string{}, []string{"native"}, []string{"cache"}, nil))
	runID := queueRunWithRequiredVolumes(t, pg, `[["cache"]]`)

	got, err := pg.ClaimNextRun(ctx, "without", []string{}, nil)
	require.NoError(t, err)
	assert.Nil(t, got, "an agent that does not advertise the volume must not claim the run")

	got, err = pg.ClaimNextRun(ctx, "with", []string{}, nil)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, runID, got.ID)
}

func TestUpsertAgent_StoresVolumes(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	pg := NewTestPostgres(t)
	ctx := context.Background()
	require.NoError(t, pg.UpsertAgent(ctx, "ag", "h", "linux", "", nil, nil, []string{"a", "b"}, nil))
	var got []string
	require.NoError(t, pg.pool.QueryRow(ctx, `SELECT volumes FROM agents WHERE id = 'ag'`).Scan(&got))
	assert.Equal(t, []string{"a", "b"}, got)

	// A re-registration that declares none replaces the set with an empty array.
	require.NoError(t, pg.UpsertAgent(ctx, "ag", "h", "linux", "", nil, nil, nil, nil))
	require.NoError(t, pg.pool.QueryRow(ctx, `SELECT volumes FROM agents WHERE id = 'ag'`).Scan(&got))
	assert.Empty(t, got)
}

func readRequiredVolumes(t *testing.T, pg *Postgres, runID string) string {
	t.Helper()
	var raw []byte
	require.NoError(t, pg.pool.QueryRow(context.Background(),
		`SELECT required_volumes FROM runs WHERE id = $1`, runID).Scan(&raw))
	return string(raw)
}

func TestTryQueueRun_PersistsExpandedRequiredVolumes(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	spec := []byte(`{"volumes":[` +
		`{"name":["lib-{{ .Params.LIB }}"],"as":"lib","mountPath":"/lib"},` +
		`{"name":["cache-1","cache-2"],"as":"cache","mountPath":"/cache"}]}`)
	run, err := pg.CreateRun(ctx, "j", map[string]string{"LIB": "x"}, spec, []string{}, []string{}, "test", "")
	require.NoError(t, err)
	_, err = pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)

	var got [][]string
	require.NoError(t, json.Unmarshal([]byte(readRequiredVolumes(t, pg, run.ID)), &got))
	assert.Equal(t, [][]string{{"lib-x"}, {"cache-1", "cache-2"}}, got)
}

func TestTryQueueRun_EmptyVolumeNameFailsTheRun(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	spec := []byte(`{"volumes":[{"name":["{{ .Params.MISSING }}"],"as":"v","mountPath":"/v"}]}`)
	run, err := pg.CreateRun(ctx, "j", nil, spec, []string{}, []string{}, "test", "")
	require.NoError(t, err)
	_, err = pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)

	got, err := pg.GetRun(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, api.RunFailed, got.Status, "an empty volume name must fail the run, not leave it Queued")

	logs, err := pg.TailLogs(ctx, run.ID, 0, 100)
	require.NoError(t, err)
	var joined strings.Builder
	for _, l := range logs {
		joined.WriteString(l.Line)
		joined.WriteString("\n")
	}
	assert.Contains(t, joined.String(), "spec.volumes")
}

func TestTryQueueRun_NoVolumesPersistsAnEmptyArray(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), []string{}, []string{}, "test", "")
	require.NoError(t, err)
	_, err = pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)

	// dsl.RequiredVolumes returns a nil slice here; it must reach the column as
	// [] and never as the JSON value null.
	assert.JSONEq(t, `[]`, readRequiredVolumes(t, pg, run.ID))
	var isNull bool
	require.NoError(t, pg.pool.QueryRow(context.Background(),
		`SELECT required_volumes = 'null'::jsonb FROM runs WHERE id = $1`, run.ID).Scan(&isNull))
	assert.False(t, isNull)
}
