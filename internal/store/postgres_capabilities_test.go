package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaimNextRun_CapabilityMatch(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	pg := NewTestPostgres(t)
	ctx := context.Background()

	// a native-cap agent and a pod-cap agent, no label constraints
	require.NoError(t, pg.UpsertAgent(ctx, "host-1", "h1", "linux", "", []string{}, []string{"native", "container"}, nil))
	require.NoError(t, pg.UpsertAgent(ctx, "k8s-1", "k1", "linux/k8s", "", []string{}, []string{"pod", "container"}, nil))

	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	// a run that needs native must be claimable only by host-1
	nativeRun, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), []string{}, []string{"native"}, "test")
	require.NoError(t, err)
	_, err = pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)

	// k8s-1 cannot claim it
	got, err := pg.ClaimNextRun(ctx, "k8s-1", []string{})
	require.NoError(t, err)
	assert.Nil(t, got, "k8s agent (no native cap) must not claim a native run")

	// host-1 can
	got, err = pg.ClaimNextRun(ctx, "host-1", []string{})
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, nativeRun.ID, got.ID)
}

func TestClaimNextRun_LegacyAgentSkipsCapCheck(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	pg := NewTestPostgres(t)
	ctx := context.Background()
	// legacy agent: capabilities NULL (passed as nil)
	require.NoError(t, pg.UpsertAgent(ctx, "legacy-1", "l1", "linux", "", []string{}, nil, nil))
	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	_, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), []string{}, []string{"native"}, "test")
	require.NoError(t, err)
	_, err = pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)
	got, err := pg.ClaimNextRun(ctx, "legacy-1", []string{})
	require.NoError(t, err)
	require.NotNil(t, got, "a legacy (null-caps) agent must still claim by labels only")
}

func TestUpsertAgentOnClaim_PreservesCapabilities(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	pg := NewTestPostgres(t)
	ctx := context.Background()
	require.NoError(t, pg.UpsertAgent(ctx, "a", "h", "linux", "", []string{"kind:docker"}, []string{"native", "container"}, nil))
	// claim-path upsert (no caps) must NOT wipe the registered caps
	require.NoError(t, pg.UpsertAgentOnClaim(ctx, "a", "h", "linux", "", []string{"kind:docker"}, nil))
	info, err := pg.GetAgent(ctx, "a")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"native", "container"}, info.Capabilities)
}
