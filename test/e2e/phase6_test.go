package e2e

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/controller"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPhase6_PATAuthentication verifies that a PAT can authenticate API calls.
func TestPhase6_PATAuthentication(t *testing.T) {
	pg := store.NewTestPostgres(t)
	srv := controller.NewServer(controller.Config{Token: "admin-secret", LegacyAgentToken: "agent-secret"}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, "admin-secret"))
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	// Admin creates a PAT
	body, _ := json.Marshal(api.CreatePATRequest{Name: "ci-token"})
	req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/tokens", asReader(body))
	req.Header.Set("Authorization", "Bearer admin-secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var patResp api.CreatePATResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&patResp))
	resp.Body.Close()

	// Use the PAT to list jobs
	req2, _ := http.NewRequest(http.MethodGet, httpSrv.URL+"/api/v1/jobs", nil)
	req2.Header.Set("Authorization", "Bearer "+patResp.Token)
	resp2, err := http.DefaultClient.Do(req2)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp2.StatusCode, "PAT should authenticate API calls")
	resp2.Body.Close()

	// Invalid token → 401
	req3, _ := http.NewRequest(http.MethodGet, httpSrv.URL+"/api/v1/jobs", nil)
	req3.Header.Set("Authorization", "Bearer exc_wrongtoken12345678")
	resp3, err := http.DefaultClient.Do(req3)
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, resp3.StatusCode)
	resp3.Body.Close()
}

// TestPhase6_WebhookTrigger verifies that a webhook can trigger a job.
func TestPhase6_WebhookTrigger(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("phase 6 is linux/mac only")
	}
	pg := store.NewTestPostgres(t)
	srv := controller.NewServer(controller.Config{Token: "t", LegacyAgentToken: "t"}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, "t"))
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go controller.RunScheduler(ctx, pg, 50*time.Millisecond)

	ag := agent.New("agent-e2e", agent.NewClient(httpSrv.URL, issueAgentAccessToken(t, pg, "agent-e2e")))
	go func() { _ = ag.Run(ctx) }()

	// Register the job
	applyBody, _ := json.Marshal(api.ApplyJobRequest{YAML: `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: webhook-triggered
spec:
  native: true
  params:
    inputs:
      - name: branch
        type: string
        pattern: '^[A-Za-z0-9._/-]+$'
  steps:
    - name: run
      run: echo hello-from-webhook
`})
	req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/jobs", asReader(applyBody))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	// Register a WebhookReceiver
	applyWH, _ := json.Marshal(api.ApplyWebhookRequest{YAML: `
apiVersion: unified-cd/v1
kind: WebhookReceiver
metadata:
  name: push-hook
spec:
  trigger:
    job: webhook-triggered
  auth:
    type: none
    allowUnauthenticated: true
  paramsMapping:
    branch: '{{ index .Payload "ref" }}'
`})
	req2, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/webhooks/", asReader(applyWH))
	req2.Header.Set("Authorization", "Bearer t")
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req2)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp2.StatusCode)
	resp2.Body.Close()

	// Fire the webhook
	payload, _ := json.Marshal(map[string]any{"ref": "refs/heads/main"})
	req3, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/webhook/push-hook", bytes.NewReader(payload))
	req3.Header.Set("Content-Type", "application/json")
	resp3, err := http.DefaultClient.Do(req3)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp3.StatusCode)
	var run api.Run
	require.NoError(t, json.NewDecoder(resp3.Body).Decode(&run))
	resp3.Body.Close()

	assert.Equal(t, "webhook:push-hook", run.TriggeredBy)
	assert.Equal(t, "refs/heads/main", run.Params["branch"])

	require.Eventually(t, func() bool {
		r, err := pg.GetRun(ctx, run.ID)
		return err == nil && r.Status == api.RunSucceeded
	}, 15*time.Second, 100*time.Millisecond)
}

// TestPhase6_WebhookHMACVerification verifies a webhook with an HMAC signature.
func TestPhase6_WebhookHMACVerification(t *testing.T) {
	pg := store.NewTestPostgres(t)
	km := testKM(t)
	srv := controller.NewServer(controller.Config{Token: "t", LegacyAgentToken: "t"}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, "t"))
	srv.SetKeyManager(km)
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	// Register the HMAC secret
	setBody, _ := json.Marshal(api.SetSecretRequest{Name: "my-hook-secret", Value: "hook-secret-key"})
	req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/secrets", asReader(setBody))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	// Register the job
	applyJob, _ := json.Marshal(api.ApplyJobRequest{YAML: `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: hmac-job
spec:
  native: true
  steps:
    - name: run
      run: echo ok
`})
	req2, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/jobs", asReader(applyJob))
	req2.Header.Set("Authorization", "Bearer t")
	req2.Header.Set("Content-Type", "application/json")
	resp2, _ := http.DefaultClient.Do(req2)
	resp2.Body.Close()

	// Register a WebhookReceiver with HMAC authentication
	applyWH, _ := json.Marshal(api.ApplyWebhookRequest{YAML: `
apiVersion: unified-cd/v1
kind: WebhookReceiver
metadata:
  name: hmac-hook
spec:
  trigger:
    job: hmac-job
  auth:
    type: hmac-sha256
    secretRef: my-hook-secret
`})
	req3, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/webhooks/", asReader(applyWH))
	req3.Header.Set("Authorization", "Bearer t")
	req3.Header.Set("Content-Type", "application/json")
	resp3, _ := http.DefaultClient.Do(req3)
	resp3.Body.Close()

	// Fire the webhook with a correct signature
	payload := []byte(`{"event":"push"}`)
	mac := hmac.New(sha256.New, []byte("hook-secret-key"))
	mac.Write(payload)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req4, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/webhook/hmac-hook", bytes.NewReader(payload))
	req4.Header.Set("Content-Type", "application/json")
	req4.Header.Set("X-Signature", sig)
	resp4, err := http.DefaultClient.Do(req4)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp4.StatusCode)
	resp4.Body.Close()

	// Fire the webhook with an invalid signature → 401
	req5, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/webhook/hmac-hook", bytes.NewReader(payload))
	req5.Header.Set("Content-Type", "application/json")
	req5.Header.Set("X-Signature", "sha256=invalidsig")
	resp5, err := http.DefaultClient.Do(req5)
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, resp5.StatusCode)
	resp5.Body.Close()
}

// TestPhase6_WebhookFilter verifies that filter expressions reject non-matching payloads.
func TestPhase6_WebhookFilter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	pg := store.NewTestPostgres(t)
	srv := controller.NewServer(controller.Config{Token: "t", LegacyAgentToken: "t"}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, "t"))
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go controller.RunScheduler(ctx, pg, 50*time.Millisecond)

	// Register the job
	applyJob, _ := json.Marshal(api.ApplyJobRequest{YAML: `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: filter-job
spec:
  native: true
  steps:
    - name: run
      run: echo filtered
`})
	req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/jobs", asReader(applyJob))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	// Register a WebhookReceiver with a filter
	applyWH, _ := json.Marshal(api.ApplyWebhookRequest{YAML: `
apiVersion: unified-cd/v1
kind: WebhookReceiver
metadata:
  name: filtered-hook
spec:
  trigger:
    job: filter-job
  auth:
    type: none
    allowUnauthenticated: true
  filters:
    - '{{ eq (index .Payload "ref") "refs/heads/main" }}'
`})
	req2, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/webhooks/", asReader(applyWH))
	req2.Header.Set("Authorization", "Bearer t")
	req2.Header.Set("Content-Type", "application/json")
	resp2, _ := http.DefaultClient.Do(req2)
	resp2.Body.Close()

	// Fire for a non-main branch → filtered out (204)
	payload, _ := json.Marshal(map[string]any{"ref": "refs/heads/feature"})
	req3, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/webhook/filtered-hook", bytes.NewReader(payload))
	req3.Header.Set("Content-Type", "application/json")
	resp3, err := http.DefaultClient.Do(req3)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNoContent, resp3.StatusCode)
	resp3.Body.Close()

	// Verify that no Run was created
	runs, _ := pg.ListRunsByJob(ctx, "filter-job", 10)
	assert.Empty(t, runs)

	// Fire for the main branch → triggers a Run
	payload2, _ := json.Marshal(map[string]any{"ref": "refs/heads/main"})
	req4, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/webhook/filtered-hook", bytes.NewReader(payload2))
	req4.Header.Set("Content-Type", "application/json")
	resp4, err := http.DefaultClient.Do(req4)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp4.StatusCode)
	resp4.Body.Close()

	runs2, _ := pg.ListRunsByJob(ctx, "filter-job", 10)
	assert.Len(t, runs2, 1)
}
