package controller

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/secrets"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testKeyManager(t *testing.T) secrets.KeyManager {
	t.Helper()
	km, err := secrets.NewLocalKeyManager(hex.EncodeToString(secrets.GenerateKey()))
	require.NoError(t, err)
	return km
}

func newTestServerWithKM(t *testing.T) (*Server, store.Store) {
	t.Helper()
	pg := store.NewTestPostgres(t)
	_, err := pg.UpsertBootstrapPAT(context.Background(), "test-bootstrap", HashToken("secret"))
	require.NoError(t, err)
	km := testKeyManager(t)
	s := NewServer(Config{LegacyAgentToken: "agent-secret"}, pg)
	s.SetKeyManager(km)
	return s, pg
}

func TestAPI_SetSecret(t *testing.T) {
	s, _ := newTestServerWithKM(t)
	body, _ := json.Marshal(api.SetSecretRequest{Name: "AWS_KEY", Value: "AKID1234"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

func TestAPI_SetSecret_RejectsInvalidName(t *testing.T) {
	s, _ := newTestServerWithKM(t)
	// A space and '!' are not allowed; env-var-style names like AWS_KEY are.
	body, _ := json.Marshal(api.SetSecretRequest{Name: "bad name!", Value: "x"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "invalid")
}

func TestAPI_ListSecrets(t *testing.T) {
	s, _ := newTestServerWithKM(t)
	body, _ := json.Marshal(api.SetSecretRequest{Name: "AWS_KEY", Value: "AKID1234"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	s.Router().ServeHTTP(httptest.NewRecorder(), req)

	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/secrets", nil)
	req2.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req2)
	require.Equal(t, http.StatusOK, rec.Code)
	var list []api.SecretMeta
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list, 1)
	assert.Equal(t, "AWS_KEY", list[0].Name)
}

func TestAPI_DeleteSecret(t *testing.T) {
	s, _ := newTestServerWithKM(t)
	body, _ := json.Marshal(api.SetSecretRequest{Name: "AWS_KEY", Value: "val"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	s.Router().ServeHTTP(httptest.NewRecorder(), req)

	req2 := httptest.NewRequest(http.MethodDelete, "/api/v1/secrets/AWS_KEY", nil)
	req2.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req2)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestAgentAPI_FetchSecrets(t *testing.T) {
	s, pg := newTestServerWithKM(t)

	body, _ := json.Marshal(api.SetSecretRequest{Name: "MY_SECRET", Value: "plaintext-value"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	s.Router().ServeHTTP(httptest.NewRecorder(), req)

	_, err := pg.UpsertJob(t.Context(), "secret-fetch", "unified-cd/v1", []byte(`{"steps":[]}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(t.Context(), "secret-fetch", nil, []byte(`{"steps":[]}`), nil, nil, "")
	require.NoError(t, err)
	claimRunForTest(t, pg, "a1", run.ID)

	fetchBody, _ := json.Marshal(api.AgentFetchSecretsRequest{RunID: run.ID, Names: []string{"MY_SECRET"}})
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/secrets/fetch", bytes.NewReader(fetchBody))
	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	req2.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req2)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp api.AgentFetchSecretsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "plaintext-value", resp.Secrets["MY_SECRET"])
}

func TestAgentAPI_FetchSecrets_RejectsLegacyTokenPathImpersonation(t *testing.T) {
	s, pg := newTestServerWithKM(t)

	body, _ := json.Marshal(api.SetSecretRequest{Name: "MY_SECRET", Value: "plaintext-value"})
	set := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewReader(body))
	set.Header.Set("Authorization", "Bearer secret")
	set.Header.Set("Content-Type", "application/json")
	s.Router().ServeHTTP(httptest.NewRecorder(), set)

	_, err := pg.UpsertJob(t.Context(), "legacy-secret-fetch", "unified-cd/v1", []byte(`{"steps":[]}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(t.Context(), "legacy-secret-fetch", nil, []byte(`{"steps":[]}`), nil, nil, "")
	require.NoError(t, err)
	claimRunForTest(t, pg, "victim-agent", run.ID)

	fetchBody, _ := json.Marshal(api.AgentFetchSecretsRequest{RunID: run.ID, Names: []string{"MY_SECRET"}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/victim-agent/secrets/fetch", bytes.NewReader(fetchBody))
	req.Header.Set("Authorization", "Bearer agent-secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

func TestAgentAPI_FetchSecrets_RequiresOwningRunForBearerAgent(t *testing.T) {
	s, pg := newTestServerWithKM(t)

	body, _ := json.Marshal(api.SetSecretRequest{Name: "MY_SECRET", Value: "plaintext-value"})
	set := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewReader(body))
	set.Header.Set("Authorization", "Bearer secret")
	set.Header.Set("Content-Type", "application/json")
	s.Router().ServeHTTP(httptest.NewRecorder(), set)

	_, err := pg.UpsertJob(t.Context(), "secret-fetch-guard", "unified-cd/v1", []byte(`{"steps":[]}`))
	require.NoError(t, err)
	owned, err := pg.CreateRun(t.Context(), "secret-fetch-guard", nil, []byte(`{"steps":[]}`), nil, nil, "")
	require.NoError(t, err)
	claimRunForTest(t, pg, "agent-a", owned.ID)
	other, err := pg.CreateRun(t.Context(), "secret-fetch-guard", nil, []byte(`{"steps":[]}`), nil, nil, "")
	require.NoError(t, err)
	claimRunForTest(t, pg, "agent-b", other.ID)
	unclaimed, err := pg.CreateRun(t.Context(), "secret-fetch-guard", nil, []byte(`{"steps":[]}`), nil, nil, "")
	require.NoError(t, err)

	token := issueAgentAccessForTest(t, pg, "agent-a", nil, nil)
	for _, runID := range []string{other.ID, unclaimed.ID} {
		fetchBody, _ := json.Marshal(api.AgentFetchSecretsRequest{RunID: runID, Names: []string{"MY_SECRET"}})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-a/secrets/fetch", bytes.NewReader(fetchBody))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	}

	fetchBody, _ := json.Marshal(api.AgentFetchSecretsRequest{RunID: owned.ID, Names: []string{"MY_SECRET"}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-a/secrets/fetch", bytes.NewReader(fetchBody))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// A secret the controller cannot supply must be an error, not an omission.
// Silently returning 200 with the name missing produces a step that runs with
// an empty value — e.g. `curl -H "Authorization: Bearer $TOKEN"` with no token.
//
// Uses a real opaque agent credential + a claimed run, not the shared legacy
// token: handleAgentSecretsFetch forbids legacy credentials from fetching
// secrets at all (403, before the per-name lookup this test targets), so a
// "Bearer agent-secret" request never reaches the code under test here — see
// TestAgentAPI_FetchSecrets_RejectsLegacyTokenPathImpersonation above and the
// task-7 report for how this was discovered.
func TestAPI_FetchSecrets_MissingSecretIsAnError(t *testing.T) {
	s, pg := newTestServerWithKM(t)

	_, err := pg.UpsertJob(t.Context(), "fetch-missing-secret", "unified-cd/v1", []byte(`{"steps":[]}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(t.Context(), "fetch-missing-secret", nil, []byte(`{"steps":[]}`), nil, nil, "")
	require.NoError(t, err)
	claimRunForTest(t, pg, "agent-1", run.ID)
	token := issueAgentAccessForTest(t, pg, "agent-1", nil, nil)

	body, _ := json.Marshal(api.AgentFetchSecretsRequest{RunID: run.ID, Names: []string{"NO_SUCH_SECRET"}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-1/secrets/fetch", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "NO_SUCH_SECRET")
}

func TestAPI_FetchSecrets_ReturnsExistingSecret(t *testing.T) {
	s, pg := newTestServerWithKM(t)

	set, _ := json.Marshal(api.SetSecretRequest{Name: "PRESENT", Value: "v"})
	setReq := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewReader(set))
	setReq.Header.Set("Authorization", "Bearer secret")
	setReq.Header.Set("Content-Type", "application/json")
	setRec := httptest.NewRecorder()
	s.Router().ServeHTTP(setRec, setReq)
	require.Equal(t, http.StatusNoContent, setRec.Code, setRec.Body.String())

	_, err := pg.UpsertJob(t.Context(), "fetch-existing-secret", "unified-cd/v1", []byte(`{"steps":[]}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(t.Context(), "fetch-existing-secret", nil, []byte(`{"steps":[]}`), nil, nil, "")
	require.NoError(t, err)
	claimRunForTest(t, pg, "agent-1", run.ID)
	token := issueAgentAccessForTest(t, pg, "agent-1", nil, nil)

	body, _ := json.Marshal(api.AgentFetchSecretsRequest{RunID: run.ID, Names: []string{"PRESENT"}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-1/secrets/fetch", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp api.AgentFetchSecretsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "v", resp.Secrets["PRESENT"])
}

func TestAPI_FetchSecrets_NotConfigured(t *testing.T) {
	pg := store.NewTestPostgres(t)
	s := NewServer(Config{Token: "secret", LegacyAgentToken: "agent-secret"}, pg)
	// KeyManager is not configured.

	fetchBody, _ := json.Marshal(api.AgentFetchSecretsRequest{RunID: "run", Names: []string{"X"}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/secrets/fetch", bytes.NewReader(fetchBody))
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}
