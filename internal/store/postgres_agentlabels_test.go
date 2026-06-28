package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgres_ClaimNextRun_LabelFilter(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))

	// run that requires the kubernetes label
	run1, _ := pg.CreateRun(ctx, "j", nil, []byte(`{}`), []string{"kind:kubernetes"}, "")
	// no selector (any agent can run it)
	run2, _ := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "")

	_, _ = pg.TransitionPendingToQueued(ctx, 10)

	// linux-only agent cannot claim run1 (which requires kubernetes)
	claimed, err := pg.ClaimNextRun(ctx, "linux-agent", []string{"kind:linux"})
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.Equal(t, run2.ID, claimed.ID, "linux agent should claim the run with no selector")

	// kubernetes agent can claim run1
	claimed2, err := pg.ClaimNextRun(ctx, "k8s-agent", []string{"kind:kubernetes", "pool:build"})
	require.NoError(t, err)
	require.NotNil(t, claimed2)
	assert.Equal(t, run1.ID, claimed2.ID, "k8s agent should claim the k8s run")
}

func TestPostgres_ClaimNextRun_NoLabels_AnyAgent(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "")
	_, _ = pg.TransitionPendingToQueued(ctx, 10)

	claimed, err := pg.ClaimNextRun(ctx, "any-agent", []string{})
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.Equal(t, run.ID, claimed.ID)
}
