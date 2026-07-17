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

func TestWalkingSkeleton_EndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("phase 1 is linux/mac only")
	}

	pg := store.NewTestPostgres(t)
	srv := controller.NewServer(controller.Config{Token: "t", LegacyAgentToken: "t"}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, "t"))
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go controller.RunScheduler(ctx, pg, 50*time.Millisecond)

	ag := agent.New("agent-e2e", agent.NewClient(httpSrv.URL, "t"))
	agErr := make(chan error, 1)
	go func() { agErr <- ag.Run(ctx) }()

	// 1) apply
	yamlJob := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: e2e
spec:
  native: true
  steps:
    - name: hello
      run: printf "hello-from-step\n"
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
	body, _ = json.Marshal(api.TriggerRunRequest{JobName: "e2e"})
	req, _ = http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/runs", asReader(body))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var run api.Run
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&run))
	resp.Body.Close()

	// 3) wait for Succeeded
	require.Eventually(t, func() bool {
		r, err := pg.GetRun(ctx, run.ID)
		if err != nil {
			return false
		}
		return r.Status == api.RunSucceeded
	}, 15*time.Second, 100*time.Millisecond, "run did not reach Succeeded")

	// 4) logs include expected line
	lines, err := pg.TailLogs(ctx, run.ID, 0, 100)
	require.NoError(t, err)
	var seen bool
	for _, l := range lines {
		if l.Line == "hello-from-step" {
			seen = true
			break
		}
	}
	assert.True(t, seen, "expected log line not found: %+v", lines)

	cancel()
	select {
	case <-agErr:
	case <-time.After(2 * time.Second):
	}
}
