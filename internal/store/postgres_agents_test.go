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
	require.NoError(t, pg.UpsertAgent(ctx, "agent-1", "host1", "linux", "dev", []string{"kind:linux"}, nil))
	require.NoError(t, pg.UpsertAgent(ctx, "agent-2", "host2", "darwin", "dev", []string{"kind:mac"}, nil))
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
		[]string{"kind:docker"}, map[string]string{"PATH": "/usr/bin", "HOME": "/root"})
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

	require.NoError(t, st.UpsertAgent(ctx, "agent-1", "host1", "linux", "v1.0.0", []string{"a", "b"}, nil))
	require.NoError(t, st.UpsertAgent(ctx, "agent-1", "host1", "linux", "v1.0.0", []string{"a"}, nil))

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

	require.NoError(t, st.UpsertAgent(ctx, "agent-1", "host1", "linux", "v1.0.0", []string{"kind:linux", "hostname:host1"}, nil))
	require.NoError(t, st.UpsertAgentOnClaim(ctx, "agent-1", "", "", "", []string{"kind:linux"}, nil))

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
		if err := st.UpsertAgent(ctx, id, "host", "linux", "dev", nil, nil); err != nil {
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
	run, err := st.CreateRun(ctx, "test-job", nil, []byte(`{}`), nil, "api")
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
