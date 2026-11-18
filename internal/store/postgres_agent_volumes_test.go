package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
)

func TestAgentVolumeCRUD(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	require.NoError(t, pg.UpsertAgentVolume(ctx, "zeta", []byte(`{"mode":"shared","size":"1Gi"}`)))
	require.NoError(t, pg.UpsertAgentVolume(ctx, "alpha", []byte(`{"mode":"exclusive","size":"1Gi"}`)))
	// Upserting again replaces the spec; it is not a second row.
	require.NoError(t, pg.UpsertAgentVolume(ctx, "alpha", []byte(`{"mode":"exclusive","size":"2Gi"}`)))

	got, err := pg.ListAgentVolumes(ctx)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "alpha", got[0].Name, "sorted by name")
	assert.Equal(t, "zeta", got[1].Name)
	assert.JSONEq(t, `{"mode":"exclusive","size":"2Gi"}`, string(got[0].Spec))
	assert.False(t, got[0].UpdatedAt.IsZero())

	one, err := pg.GetAgentVolume(ctx, "zeta")
	require.NoError(t, err)
	require.NotNil(t, one)
	assert.JSONEq(t, `{"mode":"shared","size":"1Gi"}`, string(one.Spec))

	absent, err := pg.GetAgentVolume(ctx, "nope")
	require.NoError(t, err)
	assert.Nil(t, absent)

	require.NoError(t, pg.DeleteAgentVolume(ctx, "alpha"))
	require.NoError(t, pg.DeleteAgentVolume(ctx, "alpha"), "deleting an absent volume is not an error")
	got, err = pg.ListAgentVolumes(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "zeta", got[0].Name)
}

func TestSetAgentVolumeStatusReplaces(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	require.NoError(t, pg.UpsertAgentOnClaim(ctx, "a", "", "", "", nil, nil, nil, false, nil))

	require.NoError(t, pg.SetAgentVolumeStatus(ctx, "a", []api.AgentVolumeStatus{
		{Name: "unity", Ready: true},
		{Name: "cache", Ready: false, Reason: "mkdir failed"},
	}))
	got, err := pg.GetAgent(ctx, "a")
	require.NoError(t, err)
	assert.Equal(t, []api.AgentVolumeStatus{
		{Name: "unity", Ready: true},
		{Name: "cache", Ready: false, Reason: "mkdir failed"},
	}, got.VolumeStatus)

	list, err := pg.ListAgents(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Len(t, list[0].VolumeStatus, 2)

	require.NoError(t, pg.SetAgentVolumeStatus(ctx, "a", nil))
	got, err = pg.GetAgent(ctx, "a")
	require.NoError(t, err)
	assert.Empty(t, got.VolumeStatus)
}

func TestUpsertAgentOnClaimVolumesKnown(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	require.NoError(t, pg.UpsertAgentOnClaim(ctx, "a", "", "", "", nil, nil, []string{"a"}, false, nil))

	// Not known: an empty list means "not supplied" and keeps the stored one.
	require.NoError(t, pg.UpsertAgentOnClaim(ctx, "a", "", "", "", nil, nil, []string{}, false, nil))
	got, err := pg.GetAgent(ctx, "a")
	require.NoError(t, err)
	assert.Equal(t, []string{"a"}, got.Volumes)

	// Known: an empty list is authoritative and replaces the stored one.
	require.NoError(t, pg.UpsertAgentOnClaim(ctx, "a", "", "", "", nil, nil, []string{}, true, nil))
	got, err = pg.GetAgent(ctx, "a")
	require.NoError(t, err)
	assert.Empty(t, got.Volumes)
}
