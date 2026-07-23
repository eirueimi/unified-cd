package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAudit_JobApplyRecorded verifies that applying a Job is recorded with the
// actor resolved from the PAT, the correct action classification, the job
// name as the resource (pulled from the response body), and the response
// status code.
func TestAudit_JobApplyRecorded(t *testing.T) {
	s, pg := newTestServer(t)

	body := api.ApplyJobRequest{YAML: `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: hello
spec:
  steps:
    - name: greet
      run: echo hi
`}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	logs, err := pg.ListAuditLogs(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Len(t, logs, 1)
	assert.Equal(t, "test-bootstrap", logs[0].Actor)
	assert.Equal(t, "job.apply", logs[0].Action)
	assert.Equal(t, "hello", logs[0].Resource)
	assert.Equal(t, "POST", logs[0].Method)
	assert.Equal(t, "/api/v1/jobs", logs[0].Path)
	assert.Equal(t, 200, logs[0].Status)
}

// TestAudit_JobDeleteRecordsQualifiedName verifies that deleting a Job whose
// qualified name contains "/" (e.g. hierarchical jobs like "team-a/build") is
// recorded with action "job.delete" and the full qualified name as the
// resource. The job routes are registered as a catch-all "/jobs/*" (to allow
// slash-containing names), not "/jobs/{name}", so this also guards against
// regressing the audit route-pattern classification and resource resolution.
func TestAudit_JobDeleteRecordsQualifiedName(t *testing.T) {
	s, pg := newTestServer(t)

	// Hierarchical job names (containing "/") cannot be created via the YAML
	// apply endpoint (dsl name validation forbids "/" in metadata.name); seed
	// the store directly, mirroring TestAPI_GetJob_SlashInName.
	_, err := pg.UpsertJob(t.Context(), "team-a/build", "unified-cd/v1", []byte(`{"steps":[]}`))
	require.NoError(t, err)

	delReq := httptest.NewRequest(http.MethodDelete, "/api/v1/jobs/team-a/build", nil)
	delReq.Header.Set("Authorization", "Bearer secret")
	delRec := httptest.NewRecorder()
	s.Router().ServeHTTP(delRec, delReq)
	require.Equal(t, http.StatusNoContent, delRec.Code, delRec.Body.String())

	logs, err := pg.ListAuditLogs(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Len(t, logs, 1)
	assert.Equal(t, "job.delete", logs[0].Action)
	assert.Equal(t, "team-a/build", logs[0].Resource)
	assert.Equal(t, "DELETE", logs[0].Method)
	assert.Equal(t, "/api/v1/jobs/team-a/build", logs[0].Path)
}

// TestAudit_SecretSetRecordsNameOnly verifies that setting a secret records
// only the secret's name, and that the value never appears anywhere in the
// audit row (path, action, or resource).
func TestAudit_SecretSetRecordsNameOnly(t *testing.T) {
	s, pg := newTestServerWithKM(t)

	body, _ := json.Marshal(api.SetSecretRequest{Name: "AWS_KEY", Value: "super-secret-value-xyz"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	logs, err := pg.ListAuditLogs(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Len(t, logs, 1)
	assert.Equal(t, "secret.set", logs[0].Action)
	assert.Equal(t, "AWS_KEY", logs[0].Resource)
	assert.NotContains(t, logs[0].Resource, "super-secret-value-xyz")
	assert.NotContains(t, logs[0].Path, "super-secret-value-xyz")
}

// TestAudit_SecretDeleteRecordsName verifies deletes use the URL param, not a body.
func TestAudit_SecretDeleteRecordsName(t *testing.T) {
	s, pg := newTestServerWithKM(t)

	setBody, _ := json.Marshal(api.SetSecretRequest{Name: "DB_PASS", Value: "hunter2"})
	setReq := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewReader(setBody))
	setReq.Header.Set("Authorization", "Bearer secret")
	setReq.Header.Set("Content-Type", "application/json")
	s.Router().ServeHTTP(httptest.NewRecorder(), setReq)

	delReq := httptest.NewRequest(http.MethodDelete, "/api/v1/secrets/DB_PASS", nil)
	delReq.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, delReq)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	logs, err := pg.ListAuditLogs(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Len(t, logs, 2)                          // set + delete
	assert.Equal(t, "secret.delete", logs[0].Action) // newest first
	assert.Equal(t, "DB_PASS", logs[0].Resource)
}

// TestAudit_GitCredentialUpsertAndDelete verifies the gitcredential lifecycle
// is recorded with the credential name: from the request body for upsert
// (the handler responds 204 No Content, so a response-body source would
// always resolve to an empty resource), and from the URL param for delete.
func TestAudit_GitCredentialUpsertAndDelete(t *testing.T) {
	s, pg := newTestServer(t)

	body, _ := json.Marshal(api.UpsertGitCredentialRequest{
		Name: "github-creds", Host: "github.com", CredType: "token", SecretRef: "gh-token",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/gitcredentials", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	delReq := httptest.NewRequest(http.MethodDelete, "/api/v1/gitcredentials/github-creds", nil)
	delReq.Header.Set("Authorization", "Bearer secret")
	delRec := httptest.NewRecorder()
	s.Router().ServeHTTP(delRec, delReq)
	require.Equal(t, http.StatusNoContent, delRec.Code, delRec.Body.String())

	logs, err := pg.ListAuditLogs(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Len(t, logs, 2)                                 // upsert + delete
	assert.Equal(t, "gitcredential.delete", logs[0].Action) // newest first
	assert.Equal(t, "github-creds", logs[0].Resource)
	assert.Equal(t, "gitcredential.upsert", logs[1].Action)
	assert.Equal(t, "github-creds", logs[1].Resource)
}

// TestAudit_GetRequestsNotRecorded verifies read-only GET calls are never audited.
func TestAudit_GetRequestsNotRecorded(t *testing.T) {
	s, pg := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	logs, err := pg.ListAuditLogs(t.Context(), 10, 0)
	require.NoError(t, err)
	assert.Len(t, logs, 0)
}

// TestAudit_AgentEndpointsNotRecorded verifies agent-facing BearerAuth routes
// (which have no human Principal) are excluded from the audit trail.
func TestAudit_AgentEndpointsNotRecorded(t *testing.T) {
	s, pg := newTestServer(t)

	token := issueAgentAccessForTest(t, pg, "audit-agent", nil, nil)
	body, _ := json.Marshal(map[string]any{"agentId": "audit-agent", "hostname": "h", "os": "linux"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	logs, err := pg.ListAuditLogs(t.Context(), 10, 0)
	require.NoError(t, err)
	assert.Len(t, logs, 0)
}

// TestAudit_TokenCreateAndDelete verifies the token lifecycle is recorded with
// the token name (from the response body for create, URL param for delete).
func TestAudit_TokenCreateAndDelete(t *testing.T) {
	s, pg := newTestServer(t)

	body, _ := json.Marshal(api.CreatePATRequest{Name: "ci-bot"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp api.CreatePATResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	delReq := httptest.NewRequest(http.MethodDelete, "/api/v1/tokens/"+resp.ID, nil)
	delReq.Header.Set("Authorization", "Bearer secret")
	delRec := httptest.NewRecorder()
	s.Router().ServeHTTP(delRec, delReq)
	require.Equal(t, http.StatusNoContent, delRec.Code, delRec.Body.String())

	logs, err := pg.ListAuditLogs(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Len(t, logs, 2)
	assert.Equal(t, "token.delete", logs[0].Action)
	assert.Equal(t, resp.ID, logs[0].Resource)
	assert.Equal(t, "token.create", logs[1].Action)
	assert.Equal(t, "ci-bot", logs[1].Resource)
}

func TestAudit_AgentLifecycleClassification(t *testing.T) {
	for _, tc := range []struct {
		method, pattern, want string
	}{
		{http.MethodPost, "/api/v1/agent-enrollments", "agent.enrollment.create"},
		{http.MethodDelete, "/api/v1/agent-enrollments/{id}", "agent.enrollment.revoke"},
		{http.MethodPost, "/api/v1/agent-identities/{agentId}/enable", "agent.identity.enable"},
		{http.MethodPost, "/api/v1/agent-identities/{agentId}/disable", "agent.identity.disable"},
		{http.MethodPost, "/api/v1/agent-identities/{agentId}/credentials/revoke", "agent.credentials.revoke"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			got, ok := classifyAudit(tc.method, tc.pattern)
			require.True(t, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}
