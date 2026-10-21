package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// queueRun creates a run that requires caps and moves it to Queued.
func queueRun(t *testing.T, pg *Postgres, caps ...string) string {
	t.Helper()
	ctx := context.Background()
	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), []string{}, caps, "test", "")
	require.NoError(t, err)
	_, err = pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)
	return run.ID
}

// A row with NULL capabilities is an agent that never advertised any, which
// predates the capability feature and is therefore a host agent. It keeps
// claiming what a host agent could always run, and nothing else.
func TestClaimNextRun_NullCapabilitiesMeansLegacyHostBaseline(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	ctx := context.Background()

	t.Run("cannot claim a pod run", func(t *testing.T) {
		pg := NewTestPostgres(t)
		require.NoError(t, pg.UpsertAgent(ctx, "legacy", "h", "linux", "", []string{}, nil, nil, nil))
		queueRun(t, pg, "pod")
		got, err := pg.ClaimNextRun(ctx, "legacy", []string{}, nil)
		require.NoError(t, err)
		assert.Nil(t, got, "a NULL-capabilities agent must not receive a pod run")
	})

	for _, capName := range []string{"container", "native"} {
		t.Run("still claims a "+capName+" run", func(t *testing.T) {
			pg := NewTestPostgres(t)
			require.NoError(t, pg.UpsertAgent(ctx, "legacy", "h", "linux", "", []string{}, nil, nil, nil))
			id := queueRun(t, pg, capName)
			got, err := pg.ClaimNextRun(ctx, "legacy", []string{}, nil)
			require.NoError(t, err)
			require.NotNil(t, got, "the legacy baseline is {native, container}")
			assert.Equal(t, id, got.ID)
		})
	}
}

// An agent ID with no agents row at all is also NULL-capabilities (the LEFT
// JOIN yields NULL), so it gets the same baseline rather than a free pass.
func TestClaimNextRun_UnregisteredAgentGetsTheLegacyBaseline(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	ctx := context.Background()
	pg := NewTestPostgres(t)
	podID := queueRun(t, pg, "pod")

	got, err := pg.ClaimNextRun(ctx, "ghost", []string{}, nil)
	require.NoError(t, err)
	assert.Nil(t, got, "an unregistered agent must not receive a pod run")

	// Label-only matching still works for the runs a host agent may take.
	containerID := queueRun(t, pg, "container")
	got, err = pg.ClaimNextRun(ctx, "ghost", []string{}, nil)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, containerID, got.ID)
	assert.NotEqual(t, podID, got.ID)
}

// The claim path restores capabilities with the same guard it uses for
// volumes: written when supplied, never blanked by an empty list.
func TestUpsertAgentOnClaim_CapabilitiesAreWrittenWhenSuppliedAndNeverBlanked(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	ctx := context.Background()
	pg := NewTestPostgres(t)
	k8s := []string{"pod", "container"}

	require.NoError(t, pg.UpsertAgent(ctx, "a", "h", "linux/k8s", "", nil, k8s, nil, nil))
	// Neither nil (an older agent) nor an empty set may blank what is stored.
	require.NoError(t, pg.UpsertAgentOnClaim(ctx, "a", "", "", "", nil, nil, nil, nil))
	require.NoError(t, pg.UpsertAgentOnClaim(ctx, "a", "", "", "", nil, []string{}, nil, nil))
	info, err := pg.GetAgent(ctx, "a")
	require.NoError(t, err)
	assert.ElementsMatch(t, k8s, info.Capabilities)

	// A supplied set replaces the stored one.
	require.NoError(t, pg.UpsertAgentOnClaim(ctx, "a", "", "", "", nil, []string{"native", "container"}, nil, nil))
	info, err = pg.GetAgent(ctx, "a")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"native", "container"}, info.Capabilities)

	// A claim that creates the row records the set too.
	require.NoError(t, pg.UpsertAgentOnClaim(ctx, "fresh", "", "", "", nil, k8s, nil, nil))
	info, err = pg.GetAgent(ctx, "fresh")
	require.NoError(t, err)
	assert.ElementsMatch(t, k8s, info.Capabilities)

	// A claim that creates the row without a set leaves NULL, not an empty
	// array, so an agent that never advertised keeps the legacy baseline.
	require.NoError(t, pg.UpsertAgentOnClaim(ctx, "bare", "", "", "", nil, nil, nil, nil))
	var isNull bool
	require.NoError(t, pg.pool.QueryRow(ctx, `SELECT capabilities IS NULL FROM agents WHERE id = 'bare'`).Scan(&isNull))
	assert.True(t, isNull)
}

// The end-to-end shape of gap 1: a Kubernetes agent whose row was recreated by
// a heartbeat has NULL capabilities, so it cannot take a pod run until its next
// claim poll restores them.
func TestKubernetesAgentRowRecreatedByTouchAgentRestoresPodOnNextClaimPoll(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	ctx := context.Background()
	pg := NewTestPostgres(t)
	k8s := []string{"pod", "container"}

	require.NoError(t, pg.UpsertAgent(ctx, "k8s", "h", "linux/k8s", "", []string{}, k8s, nil, nil))
	// A duplicate-ID sibling deregisters, then the live agent's heartbeat
	// recreates a minimal row.
	require.NoError(t, pg.DeleteAgent(ctx, "k8s"))
	recreated, err := pg.TouchAgent(ctx, "k8s")
	require.NoError(t, err)
	require.True(t, recreated)
	info, err := pg.GetAgent(ctx, "k8s")
	require.NoError(t, err)
	require.Empty(t, info.Capabilities, "the heartbeat's row carries no capabilities")

	podID := queueRun(t, pg, "pod")
	got, err := pg.ClaimNextRun(ctx, "k8s", []string{}, nil)
	require.NoError(t, err)
	assert.Nil(t, got, "until it advertises pod, the recreated row must not claim a pod run")

	// The agent's next claim poll carries its capability set.
	require.NoError(t, pg.UpsertAgentOnClaim(ctx, "k8s", "", "", "", []string{}, k8s, nil, nil))
	got, err = pg.ClaimNextRun(ctx, "k8s", []string{}, nil)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, podID, got.ID)
}

func requiredCaps(t *testing.T, pg *Postgres, runID string) []string {
	t.Helper()
	var caps []string
	require.NoError(t, pg.pool.QueryRow(context.Background(), `SELECT required_caps FROM runs WHERE id = $1`, runID).Scan(&caps))
	return caps
}

// The git-template resolver rewrites a Pending run's spec after CreateRun fixed
// required_caps, so UpdateRunSpec must keep the two in step.
func TestUpdateRunSpec_RecomputesRequiredCaps(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	ctx := context.Background()

	const plain = `{"steps":[{"name":"s","run":"true"}]}`
	const withPodVolumes = `{"podTemplate":{"spec":{"containers":[{"name":"c","image":"alpine"}],"volumes":[{"name":"v","emptyDir":{}}]}}}`
	const hostRunnable = `{"podTemplate":{"spec":{"containers":[{"name":"c","image":"alpine"}]}}}`

	t.Run("a resolved spec that gains a pod-level key moves the run to pod", func(t *testing.T) {
		pg := NewTestPostgres(t)
		require.NoError(t, pg.UpsertAgent(ctx, "host", "h", "linux", "", []string{}, []string{"native", "container"}, nil, nil))
		require.NoError(t, pg.UpsertAgent(ctx, "k8s", "k", "linux/k8s", "", []string{}, []string{"pod", "container"}, nil, nil))
		_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
		run, err := pg.CreateRun(ctx, "j", nil, []byte(plain), []string{}, []string{"container"}, "test", "")
		require.NoError(t, err)

		require.NoError(t, pg.UpdateRunSpec(ctx, run.ID, []byte(withPodVolumes)))
		assert.Equal(t, []string{"pod"}, requiredCaps(t, pg, run.ID))

		_, err = pg.TransitionPendingToQueued(ctx, 10)
		require.NoError(t, err)
		got, err := pg.ClaimNextRun(ctx, "host", []string{}, nil)
		require.NoError(t, err)
		assert.Nil(t, got, "a host agent must no longer be able to claim it")
		got, err = pg.ClaimNextRun(ctx, "k8s", []string{}, nil)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, run.ID, got.ID)
	})

	t.Run("a resolution that changes nothing relevant leaves it alone", func(t *testing.T) {
		pg := NewTestPostgres(t)
		_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
		run, err := pg.CreateRun(ctx, "j", nil, []byte(hostRunnable), []string{}, []string{"container"}, "test", "")
		require.NoError(t, err)
		require.NoError(t, pg.UpdateRunSpec(ctx, run.ID, []byte(hostRunnable)))
		assert.Equal(t, []string{"container"}, requiredCaps(t, pg, run.ID))

		nat, err := pg.CreateRun(ctx, "j", nil, []byte(`{"native":true}`), []string{}, []string{"native"}, "test", "")
		require.NoError(t, err)
		require.NoError(t, pg.UpdateRunSpec(ctx, nat.ID, []byte(`{"native":true,"steps":[{"name":"s","run":"true"}]}`)))
		assert.Equal(t, []string{"native"}, requiredCaps(t, pg, nat.ID))
	})

	t.Run("a run that is no longer Pending keeps both spec and caps", func(t *testing.T) {
		pg := NewTestPostgres(t)
		_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
		run, err := pg.CreateRun(ctx, "j", nil, []byte(plain), []string{}, []string{"container"}, "test", "")
		require.NoError(t, err)
		_, err = pg.TransitionPendingToQueued(ctx, 10)
		require.NoError(t, err)
		require.NoError(t, pg.UpdateRunSpec(ctx, run.ID, []byte(withPodVolumes)))
		assert.Equal(t, []string{"container"}, requiredCaps(t, pg, run.ID))
	})

	t.Run("an unparseable spec is refused rather than written with stale caps", func(t *testing.T) {
		pg := NewTestPostgres(t)
		_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
		run, err := pg.CreateRun(ctx, "j", nil, []byte(plain), []string{}, []string{"container"}, "test", "")
		require.NoError(t, err)
		require.Error(t, pg.UpdateRunSpec(ctx, run.ID, []byte(`not json`)))
	})
}
