package e2e

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/controller"
	"github.com/eirueimi/unified-cd/internal/secrets"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testKM(t *testing.T) secrets.KeyManager {
	t.Helper()
	km, err := secrets.NewLocalKeyManager(hex.EncodeToString(secrets.GenerateKey()))
	require.NoError(t, err)
	return km
}

func newControllerWithKM(t *testing.T) (*controller.Server, *store.Postgres, *httptest.Server) {
	t.Helper()
	pg := store.NewTestPostgres(t)
	km := testKM(t)
	srv := controller.NewServer(controller.Config{Token: "t", LegacyAgentToken: "t"}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, "t"))
	srv.SetKeyManager(km)
	httpSrv := httptest.NewServer(srv.Router())
	t.Cleanup(httpSrv.Close)
	return srv, pg, httpSrv
}

// TestPhase5_SecretInjection verifies that a secret is injected as an env var and the step can use it.
func TestPhase5_SecretInjection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("phase 5 is linux/mac only")
	}
	_, pg, httpSrv := newControllerWithKM(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go controller.RunScheduler(ctx, pg, 50*time.Millisecond)

	ag := agent.New("agent-e2e", agent.NewClient(httpSrv.URL, "t"))
	go func() { _ = ag.Run(ctx) }()

	// Register the secret
	setBody, _ := json.Marshal(api.SetSecretRequest{Name: "MY_SECRET", Value: "hello-secret-value"})
	req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/secrets", asReader(setBody))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	resp.Body.Close()

	// Register a job that uses the secret as an env var
	yamlJob := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: secret-test
spec:
  native: true
  steps:
    - name: use-secret
      env:
        MY_VAR: "{{ secrets.MY_SECRET }}"
      run: 'echo "value=${MY_VAR}"'
`
	applyBody, _ := json.Marshal(api.ApplyJobRequest{YAML: yamlJob})
	req, _ = http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/jobs", asReader(applyBody))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	// Trigger a Run
	trigBody, _ := json.Marshal(api.TriggerRunRequest{JobName: "secret-test"})
	req, _ = http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/runs", asReader(trigBody))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	var run api.Run
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&run))
	resp.Body.Close()

	require.Eventually(t, func() bool {
		r, err := pg.GetRun(ctx, run.ID)
		return err == nil && r.Status == api.RunSucceeded
	}, 15*time.Second, 100*time.Millisecond)

	// Verify that the step executed with the secret value via env var and printed it to the logs.
	// Because the masker is active, the value is recorded as masked ("***") rather than plain text.
	logs, err := pg.TailLogs(ctx, run.ID, 0, 100)
	require.NoError(t, err)
	var found bool
	for _, l := range logs {
		if strings.Contains(l.Line, "value=") {
			found = true
			break
		}
	}
	assert.True(t, found, "step should have printed 'value=...' via env var; logs: %v", logs)

	// Verify that the plain-text secret value does not appear in the logs
	for _, l := range logs {
		assert.NotContains(t, l.Line, "hello-secret-value",
			"plain secret must not appear in logs: %q", l.Line)
	}
}

// TestPhase5_StdoutMasking verifies that secret values are masked in logs.
func TestPhase5_StdoutMasking(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("phase 5 is linux/mac only")
	}
	_, pg, httpSrv := newControllerWithKM(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go controller.RunScheduler(ctx, pg, 50*time.Millisecond)

	ag := agent.New("agent-mask", agent.NewClient(httpSrv.URL, "t"))
	go func() { _ = ag.Run(ctx) }()

	// Register the secret
	setBody, _ := json.Marshal(api.SetSecretRequest{Name: "MASK_ME", Value: "super-secret-xyz"})
	req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/secrets", asReader(setBody))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	// A job that echoes the secret value directly (logs should be masked)
	yamlJob := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: mask-test
spec:
  native: true
  steps:
    - name: leak-attempt
      env:
        SECRET_VAL: "{{ secrets.MASK_ME }}"
      run: 'echo "secret=${SECRET_VAL}" && echo "safe line"'
`
	applyBody, _ := json.Marshal(api.ApplyJobRequest{YAML: yamlJob})
	req, _ = http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/jobs", asReader(applyBody))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	trigBody, _ := json.Marshal(api.TriggerRunRequest{JobName: "mask-test"})
	req, _ = http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/runs", asReader(trigBody))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	var run api.Run
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&run))
	resp.Body.Close()

	require.Eventually(t, func() bool {
		r, err := pg.GetRun(ctx, run.ID)
		return err == nil && r.Status == api.RunSucceeded
	}, 15*time.Second, 100*time.Millisecond)

	logs, err := pg.TailLogs(ctx, run.ID, 0, 100)
	require.NoError(t, err)

	// Verify that the plain-text secret value is not present in the logs
	for _, l := range logs {
		assert.NotContains(t, l.Line, "super-secret-xyz",
			"plain secret must not appear in logs: %q", l.Line)
	}
	// Non-secret content should remain in the logs as-is
	var safeFound bool
	for _, l := range logs {
		if l.Line == "safe line" {
			safeFound = true
		}
	}
	assert.True(t, safeFound, "non-secret content should pass through unmasked")
}
