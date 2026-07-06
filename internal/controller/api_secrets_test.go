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
	s := NewServer(Config{AgentToken: "agent-secret"}, pg)
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
	s, _ := newTestServerWithKM(t)

	body, _ := json.Marshal(api.SetSecretRequest{Name: "MY_SECRET", Value: "plaintext-value"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	s.Router().ServeHTTP(httptest.NewRecorder(), req)

	fetchBody, _ := json.Marshal(api.AgentFetchSecretsRequest{Names: []string{"MY_SECRET"}})
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/secrets/fetch", bytes.NewReader(fetchBody))
	req2.Header.Set("Authorization", "Bearer agent-secret")
	req2.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req2)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp api.AgentFetchSecretsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "plaintext-value", resp.Secrets["MY_SECRET"])
}

func TestAPI_FetchSecrets_NotConfigured(t *testing.T) {
	pg := store.NewTestPostgres(t)
	s := NewServer(Config{Token: "secret", AgentToken: "agent-secret"}, pg)
	// KeyManager is not configured.

	fetchBody, _ := json.Marshal(api.AgentFetchSecretsRequest{Names: []string{"X"}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/secrets/fetch", bytes.NewReader(fetchBody))
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}
