package e2e

import (
	"bufio"
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
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPhase3_AdvisoryLockScheduler(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("phase 3 is linux/mac only")
	}
	pg := store.NewTestPostgres(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	_, _ = pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go controller.RunScheduler(ctx, pg, 20*time.Millisecond)
	go controller.RunScheduler(ctx, pg, 20*time.Millisecond)
	go controller.RunScheduler(ctx, pg, 20*time.Millisecond)

	require.Eventually(t, func() bool {
		runs, _ := pg.ListRunsByJob(ctx, "j", 10)
		return len(runs) == 1 && runs[0].Status == api.RunQueued
	}, 5*time.Second, 100*time.Millisecond)

	runs, _ := pg.ListRunsByJob(ctx, "j", 10)
	assert.Equal(t, 1, len(runs))
	assert.Equal(t, api.RunQueued, runs[0].Status)
}

func TestPhase3_LogArchival(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("phase 3 is linux/mac only")
	}
	pg := store.NewTestPostgres(t)
	obj := objectstore.NewLocalObjectStore(t.TempDir())

	srv := controller.NewServer(controller.Config{Token: "t", AgentToken: "t"}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, "t"))
	srv.SetObjectStore(obj)
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go controller.RunScheduler(ctx, pg, 50*time.Millisecond)
	go controller.RunLogArchiver(ctx, pg, obj, 200*time.Millisecond)

	ag := agent.New("agent-e2e", agent.NewClient(httpSrv.URL, "t"))
	go func() { _ = ag.Run(ctx) }()

	yamlJob := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: archive-test
spec:
  native: true
  steps:
    - name: hello
      run: 'printf "ARCHIVED_LOG_LINE\n"'
`
	applyBody, _ := json.Marshal(api.ApplyJobRequest{YAML: yamlJob})
	req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/jobs", asReader(applyBody))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	triggerBody, _ := json.Marshal(api.TriggerRunRequest{JobName: "archive-test"})
	req, _ = http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/runs", asReader(triggerBody))
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

	require.Eventually(t, func() bool {
		arch, _ := pg.GetLogArchive(ctx, run.ID)
		return arch != nil
	}, 5*time.Second, 100*time.Millisecond, "log archive should be created")

	req, _ = http.NewRequest(http.MethodGet, httpSrv.URL+"/api/v1/runs/"+run.ID+"/logs/archive", nil)
	req.Header.Set("Authorization", "Bearer t")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var foundLine bool
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), "ARCHIVED_LOG_LINE") {
			foundLine = true
			break
		}
	}
	assert.True(t, foundLine, "archived NDJSON should contain the log line")
}

func TestPhase3_SSE(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("phase 3 is linux/mac only")
	}
	pg := store.NewTestPostgres(t)
	srv := controller.NewServer(controller.Config{Token: "t", AgentToken: "t"}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, "t"))
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go controller.RunScheduler(ctx, pg, 50*time.Millisecond)

	ag := agent.New("agent-sse", agent.NewClient(httpSrv.URL, "t"))
	go func() { _ = ag.Run(ctx) }()

	yamlJob := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: sse-test
spec:
  native: true
  steps:
    - name: hello
      run: 'printf "SSE_TEST_LINE\n"'
`
	applyBody, _ := json.Marshal(api.ApplyJobRequest{YAML: yamlJob})
	req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/jobs", asReader(applyBody))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	triggerBody, _ := json.Marshal(api.TriggerRunRequest{JobName: "sse-test"})
	req, _ = http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/runs", asReader(triggerBody))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	var run api.Run
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&run))
	resp.Body.Close()

	// Wait for the Run to complete
	require.Eventually(t, func() bool {
		r, err := pg.GetRun(ctx, run.ID)
		return err == nil && r.Status == api.RunSucceeded
	}, 15*time.Second, 100*time.Millisecond)

	req, _ = http.NewRequest(http.MethodGet, httpSrv.URL+"/api/v1/runs/"+run.ID+"/events", nil)
	req.Header.Set("Authorization", "Bearer t")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	var body strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		body.WriteString(scanner.Text())
		body.WriteByte('\n')
	}
	assert.Contains(t, body.String(), "SSE_TEST_LINE")
	assert.Contains(t, body.String(), "Succeeded")
}

func TestPhase3_BulkLogPush(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("phase 3 is linux/mac only")
	}
	pg := store.NewTestPostgres(t)
	srv := controller.NewServer(controller.Config{Token: "t", AgentToken: "t"}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, "t"))
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go controller.RunScheduler(ctx, pg, 50*time.Millisecond)

	ag := agent.New("agent-bulk", agent.NewClient(httpSrv.URL, "t"))
	go func() { _ = ag.Run(ctx) }()

	yamlJob := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: bulk-test
spec:
  native: true
  steps:
    - name: multiline
      run: 'for i in 1 2 3 4 5; do printf "LINE_%d\n" $i; done'
`
	applyBody, _ := json.Marshal(api.ApplyJobRequest{YAML: yamlJob})
	req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/jobs", asReader(applyBody))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	triggerBody, _ := json.Marshal(api.TriggerRunRequest{JobName: "bulk-test"})
	req, _ = http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/runs", asReader(triggerBody))
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

	lines, err := pg.TailLogs(ctx, run.ID, 0, 100)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(lines), 5, "all 5 log lines should be stored")
}
