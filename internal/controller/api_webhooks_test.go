package controller

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testWebhookYAML = `
apiVersion: unified-cd/v1
kind: WebhookReceiver
metadata:
  name: test-hook
spec:
  trigger:
    job: build
  auth:
    type: none
`

func TestAPI_ApplyWebhook(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "build", "unified-cd/v1", []byte(`{"steps":[{"name":"s","run":"echo x"}]}`))

	body, _ := json.Marshal(api.ApplyWebhookRequest{YAML: testWebhookYAML})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var meta api.WebhookReceiverMeta
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &meta))
	assert.Equal(t, "test-hook", meta.Name)
}

func TestAPI_ListWebhooks(t *testing.T) {
	s, pg := newTestServer(t)
	spec, _ := json.Marshal(map[string]any{"trigger": map[string]any{"job": "build"}, "auth": map[string]any{"type": "none"}})
	_, _ = pg.UpsertWebhookReceiver(t.Context(), "hook1", spec)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/webhooks", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var list []api.WebhookReceiverMeta
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	assert.Len(t, list, 1)
}

func TestWebhookIngress_NoAuth(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "build", "unified-cd/v1",
		[]byte(`{"steps":[{"name":"s","run":"echo x"}]}`))
	spec, _ := json.Marshal(map[string]any{
		"trigger":       map[string]any{"job": "build"},
		"auth":          map[string]any{"type": "none"},
		"paramsMapping": map[string]any{"branch": `{{ index .Payload "ref" }}`},
	})
	_, _ = pg.UpsertWebhookReceiver(t.Context(), "test-hook", spec)

	payload, _ := json.Marshal(map[string]any{"ref": "refs/heads/main"})
	req := httptest.NewRequest(http.MethodPost, "/webhook/test-hook", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	runs, err := pg.ListRunsByJob(t.Context(), "build", 10)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, "refs/heads/main", runs[0].Params["branch"])
	assert.Equal(t, "webhook:test-hook", runs[0].TriggeredBy)
}

// TestWebhookIngress_ExpandsAgentSelectorParams verifies that the agentSelector template
// `{{ .Params.pool }}` is also expanded with the params determined by paramsMapping during webhook ingress.
func TestWebhookIngress_ExpandsAgentSelectorParams(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "build", "unified-cd/v1",
		[]byte(`{"agentSelector":["pool:{{ .Params.pool }}"],"steps":[{"name":"s","run":"echo x"}]}`))
	spec, _ := json.Marshal(map[string]any{
		"trigger":       map[string]any{"job": "build"},
		"auth":          map[string]any{"type": "none"},
		"paramsMapping": map[string]any{"pool": `{{ index .Payload "pool" }}`},
	})
	_, _ = pg.UpsertWebhookReceiver(t.Context(), "test-hook", spec)

	payload, _ := json.Marshal(map[string]any{"pool": "build"})
	req := httptest.NewRequest(http.MethodPost, "/webhook/test-hook", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	_, _ = pg.TransitionPendingToQueued(t.Context(), 10)
	claimed, err := pg.ClaimNextRun(t.Context(), "agent-with-label", []string{"pool:build"})
	require.NoError(t, err)
	require.NotNil(t, claimed, "agent with the expanded label must claim the run")
}

// TestWebhookIngress_MissingRequiredParam verifies that a webhook-triggered
// Run is rejected with 400 when the job declares a required input that the
// webhook's paramsMapping does not supply.
func TestWebhookIngress_MissingRequiredParam(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "build", "unified-cd/v1", []byte(`{
		"params": {"inputs": [{"name": "image", "type": "string", "required": true}]},
		"steps": [{"name": "s", "run": "echo x"}]
	}`))
	spec, _ := json.Marshal(map[string]any{
		"trigger": map[string]any{"job": "build"},
		"auth":    map[string]any{"type": "none"},
	})
	_, _ = pg.UpsertWebhookReceiver(t.Context(), "test-hook", spec)

	payload, _ := json.Marshal(map[string]any{"ref": "refs/heads/main"})
	req := httptest.NewRequest(http.MethodPost, "/webhook/test-hook", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "image")

	runs, err := pg.ListRunsByJob(t.Context(), "build", 10)
	require.NoError(t, err)
	assert.Empty(t, runs)
}

// TestWebhookIngress_InjectsDefaultParam verifies that a webhook-triggered Run
// gets defaults filled in for inputs the paramsMapping doesn't supply.
func TestWebhookIngress_InjectsDefaultParam(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "build", "unified-cd/v1", []byte(`{
		"params": {"inputs": [{"name": "tag", "type": "string", "default": "latest"}]},
		"steps": [{"name": "s", "run": "echo x"}]
	}`))
	spec, _ := json.Marshal(map[string]any{
		"trigger": map[string]any{"job": "build"},
		"auth":    map[string]any{"type": "none"},
	})
	_, _ = pg.UpsertWebhookReceiver(t.Context(), "test-hook", spec)

	payload, _ := json.Marshal(map[string]any{"ref": "refs/heads/main"})
	req := httptest.NewRequest(http.MethodPost, "/webhook/test-hook", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	runs, err := pg.ListRunsByJob(t.Context(), "build", 10)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, "latest", runs[0].Params["tag"])
}

func TestWebhookIngress_FilterRejectsNonMain(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "build", "unified-cd/v1",
		[]byte(`{"steps":[{"name":"s","run":"echo x"}]}`))
	spec, _ := json.Marshal(map[string]any{
		"trigger": map[string]any{"job": "build"},
		"auth":    map[string]any{"type": "none"},
		"filters": []string{`{{ eq (index .Payload "ref") "refs/heads/main" }}`},
	})
	_, _ = pg.UpsertWebhookReceiver(t.Context(), "filter-hook", spec)

	payload, _ := json.Marshal(map[string]any{"ref": "refs/heads/feature"})
	req := httptest.NewRequest(http.MethodPost, "/webhook/filter-hook", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNoContent, rec.Code, "filtered out — not an error")

	runs, _ := pg.ListRunsByJob(t.Context(), "build", 10)
	assert.Empty(t, runs, "filtered webhook should not create a run")
}

func TestWebhookIngress_HMACVerification(t *testing.T) {
	s, pg := newTestServer(t)
	km := testKeyManager(t)
	s.SetKeyManager(km)

	_, _ = pg.UpsertJob(t.Context(), "build", "unified-cd/v1",
		[]byte(`{"steps":[{"name":"s","run":"echo x"}]}`))

	setBody, _ := json.Marshal(api.SetSecretRequest{Name: "webhook-secret", Value: "mysecret"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewReader(setBody))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	s.Router().ServeHTTP(httptest.NewRecorder(), req)

	spec, _ := json.Marshal(map[string]any{
		"trigger": map[string]any{"job": "build"},
		"auth":    map[string]any{"type": "hmac-sha256", "secretRef": "webhook-secret"},
	})
	_, _ = pg.UpsertWebhookReceiver(t.Context(), "secured-hook", spec)

	payload := []byte(`{"event":"push"}`)
	mac := hmac.New(sha256.New, []byte("mysecret"))
	mac.Write(payload)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req2 := httptest.NewRequest(http.MethodPost, "/webhook/secured-hook", bytes.NewReader(payload))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Signature", sig)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req2)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestWebhookIngress_HMACBadSignature(t *testing.T) {
	s, pg := newTestServer(t)
	km := testKeyManager(t)
	s.SetKeyManager(km)

	_, _ = pg.UpsertJob(t.Context(), "build", "unified-cd/v1",
		[]byte(`{"steps":[{"name":"s","run":"echo x"}]}`))
	setBody, _ := json.Marshal(api.SetSecretRequest{Name: "webhook-secret", Value: "mysecret"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewReader(setBody))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	s.Router().ServeHTTP(httptest.NewRecorder(), req)

	spec, _ := json.Marshal(map[string]any{
		"trigger": map[string]any{"job": "build"},
		"auth":    map[string]any{"type": "hmac-sha256", "secretRef": "webhook-secret"},
	})
	_, _ = pg.UpsertWebhookReceiver(t.Context(), "secured-hook", spec)

	req2 := httptest.NewRequest(http.MethodPost, "/webhook/secured-hook", bytes.NewReader([]byte(`{}`)))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Signature", "sha256=badsignature")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req2)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
