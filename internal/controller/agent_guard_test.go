package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeGuardStore serves GetRun from a map and counts calls (to prove the
// claimed-by cache short-circuits repeat lookups on the hot path).
type fakeGuardStore struct {
	store.Store
	runs     map[string]*api.Run
	getCalls int
}

func (f *fakeGuardStore) GetRun(ctx context.Context, id string) (*api.Run, error) {
	f.getCalls++
	if r, ok := f.runs[id]; ok {
		return r, nil
	}
	return nil, store.ErrRunNotFound
}

func guardServer(st store.Store) *Server {
	return NewServer(Config{LegacyAgentToken: "agent-secret"}, st)
}

func TestAgentRunGuard_Verdicts(t *testing.T) {
	st := &fakeGuardStore{runs: map[string]*api.Run{
		"live":      {ID: "live", Status: api.RunRunning, ClaimedBy: "a1"},
		"done":      {ID: "done", Status: api.RunSucceeded, ClaimedBy: "a1"},
		"unclaimed": {ID: "unclaimed", Status: api.RunPending},
	}}
	s := guardServer(st)
	ctx := context.Background()

	cases := []struct {
		name           string
		agent, run     string
		rejectTerminal bool
		want           runWriteVerdict
	}{
		{"owner live", "a1", "live", true, runWriteOK},
		{"owner live no-terminal-check", "a1", "live", false, runWriteOK},
		{"wrong agent", "a2", "live", true, runWriteNotOwned},
		{"missing run", "a1", "nope", true, runWriteNotFound},
		{"unclaimed run", "a1", "unclaimed", true, runWriteNotOwned},
		{"terminal rejected", "a1", "done", true, runWriteTerminal},
		{"terminal allowed when not rejecting", "a1", "done", false, runWriteOK},
	}
	for _, c := range cases {
		v, err := s.agentRunGuard(ctx, c.agent, c.run, c.rejectTerminal)
		require.NoError(t, err, c.name)
		assert.Equal(t, c.want, v, c.name)
	}
}

func TestAgentRunGuard_CachesClaimedBy(t *testing.T) {
	st := &fakeGuardStore{runs: map[string]*api.Run{
		"live": {ID: "live", Status: api.RunRunning, ClaimedBy: "a1"},
	}}
	s := guardServer(st)
	ctx := context.Background()

	// First call populates the cache; subsequent non-terminal-checking calls
	// must not hit the store again (this is the hot log path).
	_, err := s.agentRunGuard(ctx, "a1", "live", false)
	require.NoError(t, err)
	after := st.getCalls
	for i := 0; i < 5; i++ {
		v, err := s.agentRunGuard(ctx, "a1", "live", false)
		require.NoError(t, err)
		assert.Equal(t, runWriteOK, v)
	}
	assert.Equal(t, after, st.getCalls, "cached ownership must not re-query")

	// Ownership mismatch is also answerable from cache.
	v, err := s.agentRunGuard(ctx, "a2", "live", false)
	require.NoError(t, err)
	assert.Equal(t, runWriteNotOwned, v)
	assert.Equal(t, after, st.getCalls)

	// rejectTerminal always needs live status: it must re-query.
	_, err = s.agentRunGuard(ctx, "a1", "live", true)
	require.NoError(t, err)
	assert.Greater(t, st.getCalls, after)
}

func TestAgentRunGuard_NilCacheIsSafe(t *testing.T) {
	// Tests in this package build Server via bare struct literals that skip
	// NewServer, leaving claimedBy nil. The guard must still work (uncached).
	st := &fakeGuardStore{runs: map[string]*api.Run{
		"live": {ID: "live", Status: api.RunRunning, ClaimedBy: "a1"},
	}}
	s := &Server{store: st}
	ctx := context.Background()

	v, err := s.agentRunGuard(ctx, "a1", "live", false)
	require.NoError(t, err)
	assert.Equal(t, runWriteOK, v)

	v, err = s.agentRunGuard(ctx, "a2", "live", true)
	require.NoError(t, err)
	assert.Equal(t, runWriteNotOwned, v)

	// Every call must hit the store since nothing is cached.
	assert.Equal(t, 2, st.getCalls)
}

// newGuardFixture wires a real (test-Postgres-backed) Server with the legacy
// shared token "agent-secret" configured, matching the fixture pattern used
// by the PR #63 legacy-agent tests elsewhere in this package (see
// api_agent_test.go's newTestServer / claimRunForTest).
func newGuardFixture(t *testing.T) (*Server, store.Store) {
	t.Helper()
	return newTestServer(t)
}

// mustCreateClaimedRun creates a job+run and claims it with agentID (via the
// store directly, mirroring claimRunForTest in api_agent_test.go), returning
// the run ID. Each call uses a fresh job name so repeated calls in the same
// test (one per agent) don't collide.
func mustCreateClaimedRun(t *testing.T, srv *Server, agentID string) string {
	t.Helper()
	st := srv.store
	jobName := "guard-fixture-" + agentID + "-" + fmt.Sprint(time.Now().UnixNano())
	_, err := st.UpsertJob(t.Context(), jobName, "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := st.CreateRun(t.Context(), jobName, nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	claimRunForTest(t, st, agentID, run.ID)
	return run.ID
}

// postStepReportAsLegacy posts a step report to /api/v1/agents/{pathAgentID}/steps
// using the legacy shared token, exactly as TestAgentAPI_ReportStep does.
func postStepReportAsLegacy(t *testing.T, srv *Server, pathAgentID, runID string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(api.StepReportRequest{
		RunID: runID, StepIndex: 0, Status: "Running", StartedAt: time.Now(),
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+pathAgentID+"/steps", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	return rec
}

func TestAgentRunGuard_AppliesToLegacyPrincipal(t *testing.T) {
	// A legacy (shared-token) caller must not be able to write to a run it did
	// not claim. Compatibility mode keeps such agents CONNECTED; it must not
	// exempt them from run ownership.
	srv, _ := newGuardFixture(t)
	claimedRun := mustCreateClaimedRun(t, srv, "agent-a")
	otherRun := mustCreateClaimedRun(t, srv, "agent-b")

	rr := postStepReportAsLegacy(t, srv, "agent-a", otherRun)
	assert.Equal(t, http.StatusForbidden, rr.Code, "legacy caller must not write to another agent's run")

	rr = postStepReportAsLegacy(t, srv, "agent-a", claimedRun)
	assert.Less(t, rr.Code, 400, "legacy caller must still write to the run it claimed")
}

func TestClaimedByCache_EvictsPastCap(t *testing.T) {
	c := newClaimedByCache(3)
	for i := 0; i < 5; i++ {
		c.put(fmt.Sprintf("run-%d", i), "a1")
	}
	assert.Equal(t, 3, c.len())
	_, ok := c.get("run-0")
	assert.False(t, ok, "oldest entries must be evicted")
	_, ok = c.get("run-4")
	assert.True(t, ok)
}
