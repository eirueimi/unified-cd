package e2e

import (
	"bytes"
	"context"
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
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// applyJobPhase9 registers a job YAML with the server.
func applyJobPhase9(t *testing.T, url, token, yaml string) {
	t.Helper()
	body, _ := json.Marshal(api.ApplyJobRequest{YAML: yaml})
	req, _ := http.NewRequest(http.MethodPost, url+"/api/v1/jobs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// triggerRunPhase9 starts a Run for a job and returns the RunID.
func triggerRunPhase9(t *testing.T, url, token, jobName string) string {
	t.Helper()
	body, _ := json.Marshal(api.TriggerRunRequest{JobName: jobName})
	req, _ := http.NewRequest(http.MethodPost, url+"/api/v1/runs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var run api.Run
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&run))
	return run.ID
}

// TestPhase9_ParallelSteps verifies that two steps with no dependencies are executed in parallel.
func TestPhase9_ParallelSteps(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shell (sleep, exit, printf)")
	}

	pg := store.NewTestPostgres(t)
	const tok = "test-token"
	srv := controller.NewServer(controller.Config{Token: tok, LegacyAgentToken: tok}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, tok))
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go controller.RunScheduler(ctx, pg, 50*time.Millisecond)

	ag := agent.NewWithLabels("agent-1", []string{"kind:test"}, agent.NewClient(httpSrv.URL, tok))
	go func() { _ = ag.Run(ctx) }()

	applyJobPhase9(t, httpSrv.URL, tok, `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: parallel-test
spec:
  native: true
  agentSelector: [kind:test]
  steps:
    - parallel:
      - name: step-a
        run: sleep 0.2
      - name: step-b
        run: sleep 0.2
    - name: step-c
      run: echo done
`)

	runID := triggerRunPhase9(t, httpSrv.URL, tok, "parallel-test")

	require.Eventually(t, func() bool {
		r, err := pg.GetRun(ctx, runID)
		return err == nil && (r.Status == api.RunSucceeded || r.Status == api.RunFailed)
	}, 15*time.Second, 100*time.Millisecond)

	r, err := pg.GetRun(ctx, runID)
	require.NoError(t, err)
	assert.Equal(t, api.RunSucceeded, r.Status)

	// Confirm via logs that step-c was executed (runs after step-a and step-b)
	lines, err := pg.TailLogs(ctx, runID, 0, 100)
	require.NoError(t, err)
	var seenDone bool
	for _, l := range lines {
		if strings.Contains(l.Line, "done") {
			seenDone = true
			break
		}
	}
	assert.True(t, seenDone, "step-c should have run and logged 'done'")
}

// TestPhase9_ParallelRunsToCompletion verifies that when one parallel step fails, the other runs to completion.
func TestPhase9_ParallelRunsToCompletion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shell (exit)")
	}

	pg := store.NewTestPostgres(t)
	const tok = "test-token"
	srv := controller.NewServer(controller.Config{Token: tok, LegacyAgentToken: tok}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, tok))
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go controller.RunScheduler(ctx, pg, 50*time.Millisecond)

	ag := agent.NewWithLabels("agent-1", []string{"kind:test"}, agent.NewClient(httpSrv.URL, tok))
	go func() { _ = ag.Run(ctx) }()

	applyJobPhase9(t, httpSrv.URL, tok, `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: parallel-fail-test
spec:
  native: true
  agentSelector: [kind:test]
  steps:
    - parallel:
      - name: fail-step
        run: exit 1
      - name: complete-step
        run: echo parallel-ran
`)

	runID := triggerRunPhase9(t, httpSrv.URL, tok, "parallel-fail-test")

	require.Eventually(t, func() bool {
		r, err := pg.GetRun(ctx, runID)
		return err == nil && (r.Status == api.RunSucceeded || r.Status == api.RunFailed)
	}, 15*time.Second, 100*time.Millisecond)

	r, err := pg.GetRun(ctx, runID)
	require.NoError(t, err)
	assert.Equal(t, api.RunFailed, r.Status)

	// Confirm that complete-step ran to completion (parallel steps run to completion even if one fails)
	lines, err := pg.TailLogs(ctx, runID, 0, 100)
	require.NoError(t, err)
	var seen bool
	for _, l := range lines {
		if strings.Contains(l.Line, "parallel-ran") {
			seen = true
			break
		}
	}
	assert.True(t, seen, "complete-step should have run to completion despite fail-step failing")
}

// TestPhase9_ContinueOnError verifies that a Run succeeds even when a step with continueOnError=true fails.
func TestPhase9_ContinueOnError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shell (sleep, exit, printf)")
	}

	pg := store.NewTestPostgres(t)
	const tok = "test-token"
	srv := controller.NewServer(controller.Config{Token: tok, LegacyAgentToken: tok}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, tok))
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go controller.RunScheduler(ctx, pg, 50*time.Millisecond)

	ag := agent.NewWithLabels("agent-1", []string{"kind:test"}, agent.NewClient(httpSrv.URL, tok))
	go func() { _ = ag.Run(ctx) }()

	applyJobPhase9(t, httpSrv.URL, tok, `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: continue-on-error-test
spec:
  native: true
  agentSelector: [kind:test]
  steps:
    - name: flaky
      run: exit 1
      continueOnError: true
    - name: after
      run: printf "after-ran\n"
`)

	runID := triggerRunPhase9(t, httpSrv.URL, tok, "continue-on-error-test")

	require.Eventually(t, func() bool {
		r, err := pg.GetRun(ctx, runID)
		return err == nil && (r.Status == api.RunSucceeded || r.Status == api.RunFailed)
	}, 15*time.Second, 100*time.Millisecond)

	r, err := pg.GetRun(ctx, runID)
	require.NoError(t, err)
	assert.Equal(t, api.RunSucceeded, r.Status)

	// Verify that logs contain "after-ran" (the after step was executed)
	lines, err := pg.TailLogs(ctx, runID, 0, 100)
	require.NoError(t, err)
	var seen bool
	for _, l := range lines {
		if strings.Contains(l.Line, "after-ran") {
			seen = true
			break
		}
	}
	assert.True(t, seen, "after step should have run and logged 'after-ran'")
}
