package e2e

import (
	"context"
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

// TestFailingStepSkipsRestAndFailsRun is a regression test for the bug where a
// failed (or skipped) step's report to the controller was rejected because the
// step_reports CHECK constraint only allowed ('Running','Succeeded','Failed',
// 'Cancelled') — not 'Skipped' or 'WaitingApproval'. The rejected report (HTTP
// 500) caused the agent's retryUntilSuccess to retry forever, wedging the
// agent's slot and leaving the run stuck in Running forever.
//
// This test runs a 2-step job where step 1 fails (exit 1). It asserts that:
//   - the run reaches a terminal Failed status (i.e. it does not hang)
//   - step 2 is reported as Skipped (implicit skip-after-failure)
func TestFailingStepSkipsRestAndFailsRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("e2e harness (dockertest postgres) is linux/mac only")
	}

	pg := store.NewTestPostgres(t)
	srv := controller.NewServer(controller.Config{Token: "t", LegacyAgentToken: "t"}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, "t"))
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go controller.RunScheduler(ctx, pg, 50*time.Millisecond)

	agentToken := issueAgentAccessToken(t, pg, "agent-e2e")
	ag := agent.New("agent-e2e", agent.NewClient(httpSrv.URL, agentToken))
	agErr := make(chan error, 1)
	go func() { agErr <- ag.Run(ctx) }()

	// 1) apply a 2-step job where the first step fails.
	yamlJob := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: failing-step-job
spec:
  native: true
  steps:
    - name: step-one-fails
      run: exit 1
    - name: step-two-should-be-skipped
      run: printf "should not run\n"
`
	body, _ := json.Marshal(api.ApplyJobRequest{YAML: yamlJob})
	req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/jobs", asReader(body))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// 2) trigger
	body, _ = json.Marshal(api.TriggerRunRequest{JobName: "failing-step-job"})
	req, _ = http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/runs", asReader(body))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var run api.Run
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&run))
	resp.Body.Close()

	// 3) the run must reach a terminal Failed status and must NOT hang in
	// Running forever (this is the regression being guarded against).
	require.Eventually(t, func() bool {
		r, err := pg.GetRun(ctx, run.ID)
		if err != nil {
			return false
		}
		return r.Status == api.RunFailed
	}, 15*time.Second, 100*time.Millisecond, "run did not reach Failed status (may be wedged/hanging)")

	// 4) step 1 Failed, step 2 Skipped.
	steps, err := pg.GetRunSteps(ctx, run.ID)
	require.NoError(t, err)
	require.Len(t, steps, 2, "expected 2 step reports")
	assert.Equal(t, "Failed", steps[0].Status, "step 1 status")
	assert.Equal(t, "Skipped", steps[1].Status, "step 2 status")

	cancel()
	select {
	case <-agErr:
	case <-time.After(2 * time.Second):
	}
}
