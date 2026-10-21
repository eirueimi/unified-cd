package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgres_ListAgents(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	require.NoError(t, pg.UpsertAgent(ctx, "agent-1", "host1", "linux", "dev", []string{"kind:linux"}, nil, nil, nil))
	require.NoError(t, pg.UpsertAgent(ctx, "agent-2", "host2", "darwin", "dev", []string{"kind:mac"}, nil, nil, nil))
	agents, err := pg.ListAgents(ctx)
	require.NoError(t, err)
	require.Len(t, agents, 2)
	ids := make(map[string]bool)
	for _, a := range agents {
		ids[a.ID] = true
		assert.NotEmpty(t, a.Hostname)
	}
	assert.True(t, ids["agent-1"])
	assert.True(t, ids["agent-2"])
}

func TestGetAgent_ReturnsAgent(t *testing.T) {
	st := NewTestPostgres(t)
	ctx := context.Background()

	err := st.UpsertAgent(ctx, "agent-1", "host1", "linux", "v1.0.0",
		[]string{"kind:docker"}, nil, nil, map[string]string{"PATH": "/usr/bin", "HOME": "/root"})
	if err != nil {
		t.Fatal(err)
	}

	a, err := st.GetAgent(ctx, "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if a == nil {
		t.Fatal("expected agent, got nil")
	}
	if a.ID != "agent-1" {
		t.Errorf("ID: want agent-1, got %s", a.ID)
	}
	if a.Version != "v1.0.0" {
		t.Errorf("Version: want v1.0.0, got %s", a.Version)
	}
	if a.Env["PATH"] != "/usr/bin" {
		t.Errorf("Env[PATH]: want /usr/bin, got %s", a.Env["PATH"])
	}
}

// TestUpsertAgent_ReRegistrationReplacesLabels verifies the TODO #23 fix: a fresh
// registration (UpsertAgent) is authoritative and must REPLACE the label set, so
// removing a label from an agent's config and restarting actually drops it from
// inventory. This is distinct from UpsertAgentOnClaim, which must merge instead.
func TestUpsertAgent_ReRegistrationReplacesLabels(t *testing.T) {
	st := NewTestPostgres(t)
	ctx := context.Background()

	require.NoError(t, st.UpsertAgent(ctx, "agent-1", "host1", "linux", "v1.0.0", []string{"a", "b"}, nil, nil, nil))
	require.NoError(t, st.UpsertAgent(ctx, "agent-1", "host1", "linux", "v1.0.0", []string{"a"}, nil, nil, nil))

	a, err := st.GetAgent(ctx, "agent-1")
	require.NoError(t, err)
	require.NotNil(t, a)
	assert.ElementsMatch(t, []string{"a"}, a.Labels, "re-registration must replace labels, not merge them")
}

// TestUpsertAgentOnClaim_MergesLabelsAndDoesNotClobber verifies that the claim-path
// upsert (UpsertAgentOnClaim) preserves the non-destructive #12 merge behavior: it
// must not drop richer registration data (hostname/os/version) or existing labels
// when the claim itself only supplies a subset of labels.
func TestUpsertAgentOnClaim_MergesLabelsAndDoesNotClobber(t *testing.T) {
	st := NewTestPostgres(t)
	ctx := context.Background()

	require.NoError(t, st.UpsertAgent(ctx, "agent-1", "host1", "linux", "v1.0.0", []string{"kind:linux", "hostname:host1"}, nil, nil, nil))
	require.NoError(t, st.UpsertAgentOnClaim(ctx, "agent-1", "", "", "", []string{"kind:linux"}, nil, nil, nil))

	a, err := st.GetAgent(ctx, "agent-1")
	require.NoError(t, err)
	require.NotNil(t, a)
	assert.Equal(t, "host1", a.Hostname)
	assert.Equal(t, "linux", a.OS)
	assert.Equal(t, "v1.0.0", a.Version)
	assert.Contains(t, a.Labels, "hostname:host1", "claim upsert must not drop a register-only label")
	assert.Contains(t, a.Labels, "kind:linux")
}

func TestGetAgent_ReturnsNilWhenNotFound(t *testing.T) {
	st := NewTestPostgres(t)
	a, err := st.GetAgent(context.Background(), "nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if a != nil {
		t.Errorf("expected nil, got %+v", a)
	}
}

func TestDeleteStaleAgents(t *testing.T) {
	st := NewTestPostgres(t)
	ctx := context.Background()

	for _, id := range []string{"agent-old", "agent-new"} {
		if err := st.UpsertAgent(ctx, id, "host", "linux", "dev", nil, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}

	_, err := st.pool.Exec(ctx,
		`UPDATE agents SET last_seen_at = NOW() - interval '10 minutes' WHERE id = $1`, "agent-old")
	if err != nil {
		t.Fatal(err)
	}

	deleted, err := st.DeleteStaleAgents(ctx, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Errorf("deleted: want 1, got %d", deleted)
	}

	a, _ := st.GetAgent(ctx, "agent-new")
	if a == nil {
		t.Error("agent-new should still exist")
	}
	a, _ = st.GetAgent(ctx, "agent-old")
	if a != nil {
		t.Error("agent-old should be deleted")
	}
}

func TestListRunsByAgent(t *testing.T) {
	st := NewTestPostgres(t)
	ctx := context.Background()

	if _, err := st.UpsertJob(ctx, "test-job", "v1", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateRun(ctx, "test-job", nil, []byte(`{}`), nil, nil, "api", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.pool.Exec(ctx,
		`UPDATE runs SET claimed_by = $1, status = 'Running' WHERE id = $2`, "agent-1", run.ID)
	if err != nil {
		t.Fatal(err)
	}

	runs, err := st.ListRunsByAgent(ctx, "agent-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Errorf("want 1 run, got %d", len(runs))
	}
}

// A claim can recreate an agent row (the database was reset under a running
// agent). The claim carries the agent's full volume list so the recreated row
// routes volume jobs again, but a claim that carries none must never wipe what
// registration recorded.
func TestUpsertAgentOnClaim_VolumesAreWrittenWhenSuppliedAndNeverWiped(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	// A claim recreating a missing row records the volumes it supplies.
	require.NoError(t, pg.UpsertAgentOnClaim(ctx, "a", "", "", "", nil, nil, []string{"unity", "go-cache"}, nil))
	got, err := pg.GetAgent(ctx, "a")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"unity", "go-cache"}, got.Volumes)

	// A claim that supplies none leaves them alone.
	require.NoError(t, pg.UpsertAgentOnClaim(ctx, "a", "", "", "", nil, nil, nil, nil))
	require.NoError(t, pg.UpsertAgentOnClaim(ctx, "a", "", "", "", nil, nil, []string{}, nil))
	got, err = pg.GetAgent(ctx, "a")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"unity", "go-cache"}, got.Volumes, "an empty claim-time list must not wipe the registered volumes")

	// A claim that supplies a list replaces it: the claim carries the full set.
	require.NoError(t, pg.UpsertAgentOnClaim(ctx, "a", "", "", "", nil, nil, []string{"unity"}, nil))
	got, err = pg.GetAgent(ctx, "a")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"unity"}, got.Volumes)
}

// The controller judges whether a run is schedulable by comparing the volumes
// a job names with what agents advertise, so both agent reads must return them.
func TestPostgres_AgentReadsReturnVolumes(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	require.NoError(t, pg.UpsertAgent(ctx, "agent-1", "host1", "linux", "dev", nil, nil, []string{"unity-main", "build-cache"}, nil))
	require.NoError(t, pg.UpsertAgent(ctx, "agent-2", "host2", "linux", "dev", nil, nil, nil, nil))

	agents, err := pg.ListAgents(ctx)
	require.NoError(t, err)
	got := map[string][]string{}
	for _, a := range agents {
		got[a.ID] = a.Volumes
	}
	assert.ElementsMatch(t, []string{"unity-main", "build-cache"}, got["agent-1"])
	assert.Empty(t, got["agent-2"])

	one, err := pg.GetAgent(ctx, "agent-1")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"unity-main", "build-cache"}, one.Volumes)
}
