package controller

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
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
    allowUnauthenticated: true
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

// TestWebhookIngress_NoAuth_RejectsLegacyRowWithoutFlag verifies that a stored
// receiver row with auth.type "none" and no allowUnauthenticated flag (the
// shape of every pre-existing row, and the shape any legacy or hand-crafted
// row bypassing dsl.Validate() would have — its Go zero value is false) is
// rejected at ingress with 401, and — critically — no Run is created. This is
// the live enforcement of the same rule dsl.Validate() applies at parse time;
// the store read path here never re-parses, so the flag must also be checked
// on the ingress path itself.
func TestWebhookIngress_NoAuth_RejectsLegacyRowWithoutFlag(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "build", "unified-cd/v1",
		[]byte(`{"steps":[{"name":"s","run":"echo x"}]}`))
	// Written directly via the store, bypassing Validate(), exactly like a
	// legacy pre-migration row: type "none", no allowUnauthenticated.
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
	require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "allowUnauthenticated")

	runs, err := pg.ListRunsByJob(t.Context(), "build", 10)
	require.NoError(t, err)
	assert.Empty(t, runs, "an unauthenticated legacy row must not be allowed to create a run")
}

// TestWebhookIngress_NoAuth_AllowedWithFlag verifies that a stored receiver
// row with auth.type "none" AND allowUnauthenticated: true — the deliberate
// opt-in — is accepted at ingress and creates the Run, same as before this
// fix.
func TestWebhookIngress_NoAuth_AllowedWithFlag(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "build", "unified-cd/v1",
		[]byte(`{"steps":[{"name":"s","run":"echo x"}]}`))
	spec, _ := json.Marshal(map[string]any{
		"trigger":       map[string]any{"job": "build"},
		"auth":          map[string]any{"type": "none", "allowUnauthenticated": true},
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
		"auth":          map[string]any{"type": "none", "allowUnauthenticated": true},
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
		"auth":    map[string]any{"type": "none", "allowUnauthenticated": true},
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
		"auth":    map[string]any{"type": "none", "allowUnauthenticated": true},
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
		"auth":    map[string]any{"type": "none", "allowUnauthenticated": true},
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

// TestWebhookIngress_HMACSecretNotFound verifies that a receiver referencing a
// secret that was never created returns a clear "not found" message, not a
// generic signature mismatch.
func TestWebhookIngress_HMACSecretNotFound(t *testing.T) {
	s, pg := newTestServer(t)
	s.SetKeyManager(testKeyManager(t))

	spec, _ := json.Marshal(map[string]any{
		"trigger": map[string]any{"job": "build"},
		"auth":    map[string]any{"type": "hmac-sha256", "secretRef": "never-set"},
	})
	_, _ = pg.UpsertWebhookReceiver(t.Context(), "secured-hook", spec)

	req := httptest.NewRequest(http.MethodPost, "/webhook/secured-hook", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("X-Signature", "sha256=deadbeef")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "not found")
	assert.Contains(t, rec.Body.String(), "never-set")
}

// TestWebhookIngress_HMACEmptySecret verifies that an empty stored secret (e.g.
// created by piping an empty value) yields a clear "empty" message rather than a
// generic signature mismatch.
func TestWebhookIngress_HMACEmptySecret(t *testing.T) {
	s, pg := newTestServer(t)
	s.SetKeyManager(testKeyManager(t))

	setBody, _ := json.Marshal(api.SetSecretRequest{Name: "webhook-secret", Value: ""})
	sreq := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewReader(setBody))
	sreq.Header.Set("Authorization", "Bearer secret")
	sreq.Header.Set("Content-Type", "application/json")
	s.Router().ServeHTTP(httptest.NewRecorder(), sreq)

	spec, _ := json.Marshal(map[string]any{
		"trigger": map[string]any{"job": "build"},
		"auth":    map[string]any{"type": "hmac-sha256", "secretRef": "webhook-secret"},
	})
	_, _ = pg.UpsertWebhookReceiver(t.Context(), "secured-hook", spec)

	req := httptest.NewRequest(http.MethodPost, "/webhook/secured-hook", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("X-Signature", "sha256=deadbeef")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "empty")
}

// TestWebhookIngress_GithubMissingSignatureHeader verifies that a github
// receiver that receives no signature header is told which header is missing.
func TestWebhookIngress_GithubMissingSignatureHeader(t *testing.T) {
	s, pg := newTestServer(t)
	s.SetKeyManager(testKeyManager(t))

	setBody, _ := json.Marshal(api.SetSecretRequest{Name: "gh-secret", Value: "mysecret"})
	sreq := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewReader(setBody))
	sreq.Header.Set("Authorization", "Bearer secret")
	sreq.Header.Set("Content-Type", "application/json")
	s.Router().ServeHTTP(httptest.NewRecorder(), sreq)

	spec, _ := json.Marshal(map[string]any{
		"trigger": map[string]any{"job": "build"},
		"auth":    map[string]any{"type": "github", "secretRef": "gh-secret"},
	})
	_, _ = pg.UpsertWebhookReceiver(t.Context(), "gh-hook", spec)

	req := httptest.NewRequest(http.MethodPost, "/webhook/gh-hook", bytes.NewReader([]byte(`{}`)))
	// no X-Hub-Signature-256 header
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "X-Hub-Signature-256")
}

// TestWebhookIngress_HMACMismatchMessage verifies that a genuine signature
// mismatch names the secret and points at the secret/body as the cause.
func TestWebhookIngress_HMACMismatchMessage(t *testing.T) {
	s, pg := newTestServer(t)
	s.SetKeyManager(testKeyManager(t))

	setBody, _ := json.Marshal(api.SetSecretRequest{Name: "webhook-secret", Value: "mysecret"})
	sreq := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewReader(setBody))
	sreq.Header.Set("Authorization", "Bearer secret")
	sreq.Header.Set("Content-Type", "application/json")
	s.Router().ServeHTTP(httptest.NewRecorder(), sreq)

	spec, _ := json.Marshal(map[string]any{
		"trigger": map[string]any{"job": "build"},
		"auth":    map[string]any{"type": "hmac-sha256", "secretRef": "webhook-secret"},
	})
	_, _ = pg.UpsertWebhookReceiver(t.Context(), "secured-hook", spec)

	req := httptest.NewRequest(http.MethodPost, "/webhook/secured-hook", bytes.NewReader([]byte(`{"event":"push"}`)))
	req.Header.Set("X-Signature", "sha256=00000000")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "does not match")
	assert.Contains(t, rec.Body.String(), "webhook-secret")
}

// TestWebhookIngress_AppSourceTrigger verifies that a webhook whose trigger
// targets an AppSource forces a re-sync (resets last_commit) instead of creating
// a Run, and returns 202.
func TestWebhookIngress_AppSourceTrigger(t *testing.T) {
	s, pg := newTestServer(t)

	appSpec, _ := json.Marshal(map[string]any{
		"repoURL":        "https://github.com/acme/pipelines",
		"targetRevision": "main",
		"path":           "jobs",
	})
	_, err := pg.UpsertAppSource(t.Context(), "my-pipelines", appSpec)
	require.NoError(t, err)
	// Give it a non-empty last_commit so we can observe the reset.
	require.NoError(t, pg.UpdateAppSourceSyncState(t.Context(), "my-pipelines", "abc123", time.Now(), nil))

	spec, _ := json.Marshal(map[string]any{
		"trigger": map[string]any{"appSource": "my-pipelines"},
		"auth":    map[string]any{"type": "none", "allowUnauthenticated": true},
		"filters": []string{`{{ eq (index .Payload "ref") "refs/heads/main" }}`},
	})
	_, _ = pg.UpsertWebhookReceiver(t.Context(), "gitops-hook", spec)

	payload, _ := json.Marshal(map[string]any{"ref": "refs/heads/main"})
	req := httptest.NewRequest(http.MethodPost, "/webhook/gitops-hook", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	got, err := pg.GetAppSource(t.Context(), "my-pipelines")
	require.NoError(t, err)
	assert.Equal(t, "", got.LastCommit, "last_commit should be reset to force a re-sync")

	// No run should have been created.
	runs, _ := pg.ListRunsByJob(t.Context(), "my-pipelines", 10)
	assert.Empty(t, runs)
}

// TestWebhookIngress_AppSourceTriggerFilteredOut verifies the filter still
// applies to AppSource triggers (a non-main push does not force a sync).
func TestWebhookIngress_AppSourceTriggerFilteredOut(t *testing.T) {
	s, pg := newTestServer(t)

	appSpec, _ := json.Marshal(map[string]any{"repoURL": "https://github.com/acme/pipelines", "targetRevision": "main", "path": "jobs"})
	_, _ = pg.UpsertAppSource(t.Context(), "my-pipelines", appSpec)
	require.NoError(t, pg.UpdateAppSourceSyncState(t.Context(), "my-pipelines", "abc123", time.Now(), nil))

	spec, _ := json.Marshal(map[string]any{
		"trigger": map[string]any{"appSource": "my-pipelines"},
		"auth":    map[string]any{"type": "none", "allowUnauthenticated": true},
		"filters": []string{`{{ eq (index .Payload "ref") "refs/heads/main" }}`},
	})
	_, _ = pg.UpsertWebhookReceiver(t.Context(), "gitops-hook", spec)

	payload, _ := json.Marshal(map[string]any{"ref": "refs/heads/feature"})
	req := httptest.NewRequest(http.MethodPost, "/webhook/gitops-hook", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code)
	got, _ := pg.GetAppSource(t.Context(), "my-pipelines")
	assert.Equal(t, "abc123", got.LastCommit, "filtered-out delivery must not reset last_commit")
}

// TestWebhookIngress_AppSourceNotFound verifies a clear error when the receiver
// references an AppSource that does not exist.
func TestWebhookIngress_AppSourceNotFound(t *testing.T) {
	s, pg := newTestServer(t)

	spec, _ := json.Marshal(map[string]any{
		"trigger": map[string]any{"appSource": "does-not-exist"},
		"auth":    map[string]any{"type": "none", "allowUnauthenticated": true},
	})
	_, _ = pg.UpsertWebhookReceiver(t.Context(), "gitops-hook", spec)

	payload, _ := json.Marshal(map[string]any{"ref": "refs/heads/main"})
	req := httptest.NewRequest(http.MethodPost, "/webhook/gitops-hook", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "does-not-exist")
}

func TestWebhookIngress_TokenVerification(t *testing.T) {
	s, pg := newTestServer(t)
	km := testKeyManager(t)
	s.SetKeyManager(km)

	_, _ = pg.UpsertJob(t.Context(), "build", "unified-cd/v1",
		[]byte(`{"steps":[{"name":"s","run":"echo x"}]}`))

	setBody, _ := json.Marshal(api.SetSecretRequest{Name: "gl-token", Value: "s3cr3t"})
	sreq := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewReader(setBody))
	sreq.Header.Set("Authorization", "Bearer secret")
	sreq.Header.Set("Content-Type", "application/json")
	s.Router().ServeHTTP(httptest.NewRecorder(), sreq)

	spec, _ := json.Marshal(map[string]any{
		"trigger": map[string]any{"job": "build"},
		"auth":    map[string]any{"type": "token", "secretRef": "gl-token"},
	})
	_, _ = pg.UpsertWebhookReceiver(t.Context(), "gl-hook", spec)

	req := httptest.NewRequest(http.MethodPost, "/webhook/gl-hook", bytes.NewReader([]byte(`{"ref":"refs/heads/main"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gitlab-Token", "s3cr3t")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestWebhookIngress_TokenBadToken(t *testing.T) {
	s, pg := newTestServer(t)
	km := testKeyManager(t)
	s.SetKeyManager(km)

	_, _ = pg.UpsertJob(t.Context(), "build", "unified-cd/v1",
		[]byte(`{"steps":[{"name":"s","run":"echo x"}]}`))
	setBody, _ := json.Marshal(api.SetSecretRequest{Name: "gl-token", Value: "s3cr3t"})
	sreq := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewReader(setBody))
	sreq.Header.Set("Authorization", "Bearer secret")
	sreq.Header.Set("Content-Type", "application/json")
	s.Router().ServeHTTP(httptest.NewRecorder(), sreq)

	spec, _ := json.Marshal(map[string]any{
		"trigger": map[string]any{"job": "build"},
		"auth":    map[string]any{"type": "token", "secretRef": "gl-token"},
	})
	_, _ = pg.UpsertWebhookReceiver(t.Context(), "gl-hook", spec)

	req := httptest.NewRequest(http.MethodPost, "/webhook/gl-hook", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gitlab-Token", "wrong")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestWebhookIngress_TokenMissingHeader(t *testing.T) {
	s, pg := newTestServer(t)
	km := testKeyManager(t)
	s.SetKeyManager(km)

	_, _ = pg.UpsertJob(t.Context(), "build", "unified-cd/v1",
		[]byte(`{"steps":[{"name":"s","run":"echo x"}]}`))
	setBody, _ := json.Marshal(api.SetSecretRequest{Name: "gl-token", Value: "s3cr3t"})
	sreq := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewReader(setBody))
	sreq.Header.Set("Authorization", "Bearer secret")
	sreq.Header.Set("Content-Type", "application/json")
	s.Router().ServeHTTP(httptest.NewRecorder(), sreq)

	spec, _ := json.Marshal(map[string]any{
		"trigger": map[string]any{"job": "build"},
		"auth":    map[string]any{"type": "token", "secretRef": "gl-token"},
	})
	_, _ = pg.UpsertWebhookReceiver(t.Context(), "gl-hook", spec)

	req := httptest.NewRequest(http.MethodPost, "/webhook/gl-hook", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestWebhookIngress_TokenCustomHeader(t *testing.T) {
	s, pg := newTestServer(t)
	km := testKeyManager(t)
	s.SetKeyManager(km)

	_, _ = pg.UpsertJob(t.Context(), "build", "unified-cd/v1",
		[]byte(`{"steps":[{"name":"s","run":"echo x"}]}`))
	setBody, _ := json.Marshal(api.SetSecretRequest{Name: "gl-token", Value: "s3cr3t"})
	sreq := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewReader(setBody))
	sreq.Header.Set("Authorization", "Bearer secret")
	sreq.Header.Set("Content-Type", "application/json")
	s.Router().ServeHTTP(httptest.NewRecorder(), sreq)

	spec, _ := json.Marshal(map[string]any{
		"trigger": map[string]any{"job": "build"},
		"auth":    map[string]any{"type": "token", "secretRef": "gl-token", "header": "X-Custom-Token"},
	})
	_, _ = pg.UpsertWebhookReceiver(t.Context(), "gl-hook", spec)

	req := httptest.NewRequest(http.MethodPost, "/webhook/gl-hook", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Custom-Token", "s3cr3t")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// TestWebhookIngressMetricOutcomes verifies that handleWebhookIngress records
// one metric outcome per return path: accepted (run created), filtered
// (a filter evaluated false), error (invalid JSON payload), and rejected
// (unknown receiver, recorded under name="unknown").
func TestWebhookIngressMetricOutcomes(t *testing.T) {
	s, pg := newTestServer(t)
	m := metrics.New()
	s.SetMetrics(m)

	_, _ = pg.UpsertJob(t.Context(), "wh-metrics-job", "unified-cd/v1",
		[]byte(`{"steps":[{"name":"s","run":"echo x"}]}`))

	// Receiver with no auth and a filter that only passes ref=="main".
	spec, _ := json.Marshal(map[string]any{
		"trigger": map[string]any{"job": "wh-metrics-job"},
		"auth":    map[string]any{"type": "none", "allowUnauthenticated": true},
		"filters": []string{`{{ eq .Payload.ref "main" }}`},
	})
	_, _ = pg.UpsertWebhookReceiver(t.Context(), "wh-metrics", spec)

	post := func(path, body string) int {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		return rec.Code
	}

	assert.Equal(t, http.StatusOK, post("/webhook/wh-metrics", `{"ref":"main"}`))
	assert.Equal(t, http.StatusNoContent, post("/webhook/wh-metrics", `{"ref":"dev"}`))
	assert.Equal(t, http.StatusBadRequest, post("/webhook/wh-metrics", `not-json`))
	assert.Equal(t, http.StatusNotFound, post("/webhook/no-such-receiver", `{}`))

	get := func(name, outcome string) float64 {
		return testutil.ToFloat64(m.WebhookEventsForTest(name, outcome))
	}
	assert.Equal(t, 1.0, get("wh-metrics", "accepted"))
	assert.Equal(t, 1.0, get("wh-metrics", "filtered"))
	assert.Equal(t, 1.0, get("wh-metrics", "error"))
	assert.Equal(t, 1.0, get("unknown", "rejected"))
}
