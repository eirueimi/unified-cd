package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgres_Vars_UpsertListDelete(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	name, err := pg.UpsertVars(ctx, "org-defaults", []byte(`{"vars":{"REGISTRY":"ghcr.io/myorg"}}`))
	require.NoError(t, err)
	assert.Equal(t, "org-defaults", name)

	got, err := pg.ListVars(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "org-defaults", got[0].Name)
	assert.JSONEq(t, `{"vars":{"REGISTRY":"ghcr.io/myorg"}}`, string(got[0].Spec))

	// Upsert is an update, not a second row.
	_, err = pg.UpsertVars(ctx, "org-defaults", []byte(`{"vars":{"REGISTRY":"ghcr.io/other"}}`))
	require.NoError(t, err)
	got, err = pg.ListVars(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.JSONEq(t, `{"vars":{"REGISTRY":"ghcr.io/other"}}`, string(got[0].Spec))

	require.NoError(t, pg.DeleteVars(ctx, "org-defaults"))
	got, err = pg.ListVars(ctx)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// Deleting a manifest that is not there is not an error: AppSource's delete
// path re-runs and must be idempotent.
func TestPostgres_DeleteVars_Missing(t *testing.T) {
	pg := NewTestPostgres(t)
	require.NoError(t, pg.DeleteVars(context.Background(), "never-existed"))
}

// ListVars is sorted by name, so the merge order in buildClaimResponse is
// deterministic and a collision error names the same two manifests every time.
func TestPostgres_ListVars_Sorted(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	for _, n := range []string{"zeta", "alpha", "mid"} {
		_, err := pg.UpsertVars(ctx, n, []byte(`{"vars":{}}`))
		require.NoError(t, err)
	}
	got, err := pg.ListVars(ctx)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, "alpha", got[0].Name)
	assert.Equal(t, "mid", got[1].Name)
	assert.Equal(t, "zeta", got[2].Name)
}
