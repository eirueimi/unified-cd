package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPostgres_RenameJob_SimpleRename covers the plain rename path: the old (bare)
// row exists, the new (qualified) name does NOT yet exist. The row is renamed in
// place and run history is repointed to the new name.
func TestPostgres_RenameJob_SimpleRename(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	spec := []byte(`{"steps":[{"name":"s","run":"echo x"}]}`)
	_, err := pg.UpsertJob(ctx, "build", "unified-cd/v1", spec)
	require.NoError(t, err)

	run, err := pg.CreateRun(ctx, "build", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)

	require.NoError(t, pg.RenameJob(ctx, "build", "team-a/build"))

	// Old row gone.
	_, err = pg.GetJob(ctx, "build")
	require.Error(t, err, "bare row must be gone after rename")

	// New row exists with the same spec (proving it was renamed, not recreated empty).
	nj, err := pg.GetJob(ctx, "team-a/build")
	require.NoError(t, err)
	assert.Equal(t, "team-a/build", nj.Name)
	assert.JSONEq(t, string(spec), string(nj.Spec))

	// Run history repointed.
	got, err := pg.GetRun(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, "team-a/build", got.JobName, "run must now reference the qualified name")
}

// TestPostgres_RenameJob_QualifiedAlreadyExists covers the reconcile path that
// applies in the reconciler: applyResource already UpsertJob'd the qualified row
// before prune runs, so the bare row is an ORPHAN. RenameJob must repoint run
// history from bare -> qualified and DELETE the bare orphan, leaving exactly one
// row under the qualified name.
func TestPostgres_RenameJob_QualifiedAlreadyExists(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	bareSpec := []byte(`{"steps":[{"name":"legacy","run":"legacy-marker"}]}`)
	_, err := pg.UpsertJob(ctx, "build", "unified-cd/v1", bareSpec)
	require.NoError(t, err)

	// A run referencing the bare name (legacy history).
	run, err := pg.CreateRun(ctx, "build", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)

	// The qualified row already exists (as applyResource would have created it).
	qualifiedSpec := []byte(`{"steps":[{"name":"c","run":"true"}]}`)
	_, err = pg.UpsertJob(ctx, "team-a/build", "unified-cd/v1", qualifiedSpec)
	require.NoError(t, err)

	require.NoError(t, pg.RenameJob(ctx, "build", "team-a/build"))

	// Bare orphan deleted.
	_, err = pg.GetJob(ctx, "build")
	require.Error(t, err, "bare orphan must be deleted when qualified already exists")

	// Qualified row still present, keeping the already-applied spec (not overwritten).
	nj, err := pg.GetJob(ctx, "team-a/build")
	require.NoError(t, err)
	assert.JSONEq(t, string(qualifiedSpec), string(nj.Spec),
		"existing qualified row must not be clobbered by the rename")

	// Run history repointed to the qualified name.
	got, err := pg.GetRun(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, "team-a/build", got.JobName)
}

// TestPostgres_RenameJob_Noop is safe when the old name does not exist (e.g. a
// concurrent sync already renamed it). It must not error and must not touch the
// qualified row.
func TestPostgres_RenameJob_MissingOldIsNoop(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	qualifiedSpec := []byte(`{"steps":[{"name":"c","run":"true"}]}`)
	_, err := pg.UpsertJob(ctx, "team-a/build", "unified-cd/v1", qualifiedSpec)
	require.NoError(t, err)

	require.NoError(t, pg.RenameJob(ctx, "build", "team-a/build"))

	nj, err := pg.GetJob(ctx, "team-a/build")
	require.NoError(t, err)
	assert.JSONEq(t, string(qualifiedSpec), string(nj.Spec))
}
