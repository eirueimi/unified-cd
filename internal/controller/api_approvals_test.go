package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApprovals_DecideFlow(t *testing.T) {
	s, pg := newTestServer(t)

	// Create a PAT named "alice" with a known plain token so we can verify decided_by.
	plain := "test-alice-token"
	_, err := pg.CreatePAT(t.Context(), "alice", HashToken(plain), "admin", nil)
	require.NoError(t, err)

	// Create a job and run to hang approvals off.
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{"steps":[{"name":"s","run":"echo x"}]}`))
	run, err := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "api")
	require.NoError(t, err)

	// Create a pending approval for step 0 directly via the store.
	err = pg.CreatePendingApproval(t.Context(), run.ID, 0, "gate", "please approve", nil)
	require.NoError(t, err)

	// 1. POST decision with valid PAT → 204; GetApproval shows Approved + decided_by = "alice".
	body, _ := json.Marshal(api.ApprovalDecisionRequest{Decision: "approve", Comment: "lgtm"})
	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/runs/%s/approvals/0", run.ID),
		bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+plain)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	got, err := pg.GetApproval(t.Context(), run.ID, 0)
	require.NoError(t, err)
	assert.Equal(t, "Approved", got.Status)
	assert.Equal(t, "alice", got.DecidedBy)
	assert.Equal(t, "lgtm", got.Comment)

	// 2. Second POST → 409 (already decided).
	body2, _ := json.Marshal(api.ApprovalDecisionRequest{Decision: "reject", Comment: "nope"})
	req2 := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/runs/%s/approvals/0", run.ID),
		bytes.NewReader(body2))
	req2.Header.Set("Authorization", "Bearer "+plain)
	rec2 := httptest.NewRecorder()
	s.Router().ServeHTTP(rec2, req2)
	assert.Equal(t, http.StatusConflict, rec2.Code, rec2.Body.String())

	// 3. POST to a (run, step) with no pending row → 404.
	body3, _ := json.Marshal(api.ApprovalDecisionRequest{Decision: "approve"})
	req3 := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/runs/%s/approvals/99", run.ID),
		bytes.NewReader(body3))
	req3.Header.Set("Authorization", "Bearer "+plain)
	rec3 := httptest.NewRecorder()
	s.Router().ServeHTTP(rec3, req3)
	assert.Equal(t, http.StatusNotFound, rec3.Code, rec3.Body.String())
}

func TestApprovals_ListRunApprovals(t *testing.T) {
	s, pg := newTestServer(t)

	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	run, err := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "api")
	require.NoError(t, err)

	require.NoError(t, pg.CreatePendingApproval(t.Context(), run.ID, 0, "gate-a", "msg a", nil))
	require.NoError(t, pg.CreatePendingApproval(t.Context(), run.ID, 1, "gate-b", "msg b", nil))

	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/v1/runs/%s/approvals", run.ID), nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var list []api.RunApproval
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	assert.Len(t, list, 2)
}

func TestApprovals_AgentCreateAndGet(t *testing.T) {
	s, pg := newTestServer(t)

	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	run, err := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "api")
	require.NoError(t, err)

	// Register an agent so agentId exists.
	require.NoError(t, pg.UpsertAgent(t.Context(), "ag1", "host1", "linux", "dev", nil, nil, nil))
	// Claim the run as ag1 so the ownership guard on the agent-facing
	// create-approval endpoint lets this request through.
	claimRunForTest(t, pg, "ag1", run.ID)

	// Agent creates an approval.
	createBody, _ := json.Marshal(api.CreateApprovalRequest{
		StepIndex:      2,
		StepName:       "deploy-gate",
		Message:        "approve deploy?",
		TimeoutMinutes: 0,
	})
	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/agents/ag1/runs/%s/approvals", run.ID),
		bytes.NewReader(createBody))
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	// Agent polls the approval.
	req2 := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/v1/agents/ag1/runs/%s/approvals/2", run.ID), nil)
	req2.Header.Set("Authorization", "Bearer agent-secret")
	rec2 := httptest.NewRecorder()
	s.Router().ServeHTTP(rec2, req2)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())

	var approval api.RunApproval
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &approval))
	assert.Equal(t, "Pending", approval.Status)
	assert.Equal(t, "deploy-gate", approval.StepName)
}

func TestApprovals_AgentGet_NotFound(t *testing.T) {
	s, pg := newTestServer(t)

	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	run, err := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "api")
	require.NoError(t, err)

	require.NoError(t, pg.UpsertAgent(t.Context(), "ag1", "host1", "linux", "dev", nil, nil, nil))

	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/v1/agents/ag1/runs/%s/approvals/99", run.ID), nil)
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestApprovals_DecideFlow_BadDecision(t *testing.T) {
	s, pg := newTestServer(t)

	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	run, err := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "api")
	require.NoError(t, err)
	require.NoError(t, pg.CreatePendingApproval(t.Context(), run.ID, 0, "gate", "msg", nil))

	body, _ := json.Marshal(api.ApprovalDecisionRequest{Decision: "maybe"})
	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/runs/%s/approvals/0", run.ID),
		bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}
