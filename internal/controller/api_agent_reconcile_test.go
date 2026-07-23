package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reconcilePost POSTs the reconcile endpoint as the given bearer token and
// returns the recorder.
func reconcilePost(s *Server, agentID, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/runs/reconcile", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec
}

// TestAPI_AgentReconcile_FailsOrphanedRunsAndCascades pins the orphan-recovery
// semantics (mirroring the stuck-run reaper): a Running run still claimed by
// the reconciling agent becomes Failed, its call: descendants are cascade-
// cancelled, other agents' runs are untouched, and the operation is
// idempotent.
func TestAPI_AgentReconcile_FailsOrphanedRunsAndCascades(t *testing.T) {
	s, pg := newTestServer(t)
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)

	parent, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	child, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	bystander, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	_, err = pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)

	// FIFO claim order matches creation order.
	c1, err := pg.ClaimNextRun(ctx, "agent-1", nil)
	require.NoError(t, err)
	require.Equal(t, parent.ID, c1.ID)
	c2, err := pg.ClaimNextRun(ctx, "agent-2", nil)
	require.NoError(t, err)
	require.Equal(t, child.ID, c2.ID)
	c3, err := pg.ClaimNextRun(ctx, "agent-2", nil)
	require.NoError(t, err)
	require.Equal(t, bystander.ID, c3.ID)
	for _, id := range []string{parent.ID, child.ID, bystander.ID} {
		require.NoError(t, pg.MarkRunRunning(ctx, id))
	}
	// Link parent→child the same way a call: step does.
	require.NoError(t, pg.UpsertStepReport(ctx, parent.ID, 0, 0, "call-step", "", "Running", nil, nil, nil, child.ID, "childjob"))

	agentToken := issueAgentAccessForTest(t, pg, "agent-1", nil, nil)
	rec := reconcilePost(s, "agent-1", agentToken)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body map[string]int
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, 1, body["failedRuns"])

	got, err := pg.GetRun(ctx, parent.ID)
	require.NoError(t, err)
	assert.Equal(t, api.RunFailed, got.Status, "orphaned parent must be Failed")
	got, err = pg.GetRun(ctx, child.ID)
	require.NoError(t, err)
	assert.Equal(t, api.RunCancelled, got.Status, "call: descendant must be cascade-cancelled")
	got, err = pg.GetRun(ctx, bystander.ID)
	require.NoError(t, err)
	assert.Equal(t, api.RunRunning, got.Status, "another agent's run must be untouched")

	// Idempotent: nothing left to fail.
	rec = reconcilePost(s, "agent-1", agentToken)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, 0, body["failedRuns"])
}

// TestAPI_AgentReconcile_RequiresAgentToken pins that the endpoint sits behind
// the agent bearer token, not user auth.
func TestAPI_AgentReconcile_RequiresAgentToken(t *testing.T) {
	s, _ := newTestServer(t)
	rec := reconcilePost(s, "agent-1", "secret") // user token, not agent token
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
