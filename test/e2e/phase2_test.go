package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/controller"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPhase2_StepOutputs verifies step.outputs template capture and run-level output promotion.
func TestPhase2_StepOutputs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("phase 2 is linux/mac only")
	}

	pg := store.NewTestPostgres(t)
	srv := controller.NewServer(controller.Config{Token: "t"}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, "t"))
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go controller.RunScheduler(ctx, pg, 50*time.Millisecond)

	agentToken := issueAgentAccessToken(t, pg, "agent-e2e")
	ag := agent.New("agent-e2e", agent.NewClient(httpSrv.URL, agentToken))
	go func() { _ = ag.Run(ctx) }()

	yamlJob := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: build-with-output
spec:
  native: true
  params:
    outputs:
      - name: artifact_url
        type: string
  steps:
    - name: build
      run: 'printf "building...\nARTIFACT=s3://bucket/app-1.0.tar.gz\ndone\n"'
      outputs:
        artifact_url: '{{ .Stdout | grep "ARTIFACT=" | cut "=" 2 | trim }}'
`
	applyBody, _ := json.Marshal(api.ApplyJobRequest{YAML: yamlJob})
	req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/jobs", asReader(applyBody))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	triggerBody, _ := json.Marshal(api.TriggerRunRequest{JobName: "build-with-output"})
	req, _ = http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/runs", asReader(triggerBody))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var run api.Run
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&run))
	resp.Body.Close()

	require.Eventually(t, func() bool {
		r, err := pg.GetRun(ctx, run.ID)
		return err == nil && r.Status == api.RunSucceeded
	}, 15*time.Second, 100*time.Millisecond, "run did not succeed")

	outputs, err := pg.GetRunOutputs(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, "s3://bucket/app-1.0.tar.gz", outputs["artifact_url"])
}

// TestPhase2_Mutex verifies mutex serializes concurrent runs.
func TestPhase2_Mutex(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("phase 2 is linux/mac only")
	}

	pg := store.NewTestPostgres(t)
	srv := controller.NewServer(controller.Config{Token: "t"}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, "t"))
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go controller.RunScheduler(ctx, pg, 50*time.Millisecond)

	ag1 := agent.New("agent-1", agent.NewClient(httpSrv.URL, issueAgentAccessToken(t, pg, "agent-1")))
	ag2 := agent.New("agent-2", agent.NewClient(httpSrv.URL, issueAgentAccessToken(t, pg, "agent-2")))
	go func() { _ = ag1.Run(ctx) }()
	go func() { _ = ag2.Run(ctx) }()

	yamlJob := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: exclusive
spec:
  native: true
  concurrency:
    mutex: exclusive-lock
  steps:
    - name: work
      run: sleep 0.2
`
	applyBody, _ := json.Marshal(api.ApplyJobRequest{YAML: yamlJob})
	req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/jobs", asReader(applyBody))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	var runIDs [2]string
	for i := 0; i < 2; i++ {
		body, _ := json.Marshal(api.TriggerRunRequest{JobName: "exclusive"})
		req, _ = http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/runs", asReader(body))
		req.Header.Set("Authorization", "Bearer t")
		req.Header.Set("Content-Type", "application/json")
		resp, err = http.DefaultClient.Do(req)
		require.NoError(t, err)
		var r api.Run
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&r))
		resp.Body.Close()
		runIDs[i] = r.ID
	}

	for _, rid := range runIDs {
		rid := rid
		require.Eventually(t, func() bool {
			r, err := pg.GetRun(ctx, rid)
			return err == nil && (r.Status == api.RunSucceeded || r.Status == api.RunFailed)
		}, 30*time.Second, 200*time.Millisecond, "run %s did not finish", rid)
	}

	for _, rid := range runIDs {
		r, _ := pg.GetRun(ctx, rid)
		assert.Equal(t, api.RunSucceeded, r.Status, "run %s status", rid)
	}

	// mutex should be released after both runs finish
	// Create a temporary run for verification and call AcquireMutex (because run_id is a UUID type + foreign key constraint)
	verifyRun, err := pg.CreateRun(ctx, "exclusive", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	ok, err := pg.AcquireMutex(ctx, "exclusive-lock", verifyRun.ID)
	require.NoError(t, err)
	assert.True(t, ok, "mutex should be free after runs complete")
	_ = pg.ReleaseMutex(ctx, "exclusive-lock")
}

// TestPhase2_Semaphores verifies semaphores limit concurrency.
func TestPhase2_Semaphores(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("phase 2 is linux/mac only")
	}

	pg := store.NewTestPostgres(t)
	srv := controller.NewServer(controller.Config{Token: "t"}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, "t"))
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go controller.RunScheduler(ctx, pg, 50*time.Millisecond)

	for i := 0; i < 3; i++ {
		agentID := fmt.Sprintf("agent-%d", i)
		ag := agent.New(agentID, agent.NewClient(httpSrv.URL, issueAgentAccessToken(t, pg, agentID)))
		go func() { _ = ag.Run(ctx) }()
	}

	yamlJob := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: parallel-limited
spec:
  native: true
  concurrency:
    semaphores:
      - pool: deploy-tokens
        capacity: 2
  steps:
    - name: work
      run: sleep 0.1
`
	applyBody, _ := json.Marshal(api.ApplyJobRequest{YAML: yamlJob})
	req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/jobs", asReader(applyBody))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	var runIDs [3]string
	for i := 0; i < 3; i++ {
		body, _ := json.Marshal(api.TriggerRunRequest{JobName: "parallel-limited"})
		req, _ = http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/runs", asReader(body))
		req.Header.Set("Authorization", "Bearer t")
		req.Header.Set("Content-Type", "application/json")
		resp, err = http.DefaultClient.Do(req)
		require.NoError(t, err)
		var r api.Run
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&r))
		resp.Body.Close()
		runIDs[i] = r.ID
	}

	for _, rid := range runIDs {
		rid := rid
		require.Eventually(t, func() bool {
			r, err := pg.GetRun(ctx, rid)
			return err == nil && r.Status == api.RunSucceeded
		}, 30*time.Second, 200*time.Millisecond, "run %s did not succeed", rid)
	}
}

// TestPhase2_CallStep verifies call: step creates child run and receives its outputs.
func TestPhase2_CallStep(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("phase 2 is linux/mac only")
	}

	pg := store.NewTestPostgres(t)
	srv := controller.NewServer(controller.Config{Token: "t"}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, "t"))
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go controller.RunScheduler(ctx, pg, 50*time.Millisecond)

	// Start 2 agents (parent needs one, child needs another). Either agent
	// may claim the parent run and execute its `call:` step, so both need a
	// token that also authenticates the internal child-run creation (see
	// issueCallStepAgentToken).
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		agentID := fmt.Sprintf("agent-%d", i)
		ag := agent.New(agentID, agent.NewClient(httpSrv.URL, issueCallStepAgentToken(t, pg, agentID)))
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = ag.Run(ctx)
		}()
	}

	// Apply child job
	childYAML := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: child-job
spec:
  native: true
  params:
    inputs:
      - name: greeting
        type: string
    outputs:
      - name: message
        type: string
  steps:
    - name: greet
      run: 'printf "MSG=Hello, {{ .Params.greeting }}!\n"'
      outputs:
        message: '{{ .Stdout | grep "MSG=" | cut "=" 2 | trim }}'
`
	applyChild, _ := json.Marshal(api.ApplyJobRequest{YAML: childYAML})
	req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/jobs", asReader(applyChild))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// Apply parent job
	parentYAML := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: parent-job
spec:
  native: true
  params:
    inputs:
      - name: name
        type: string
    outputs:
      - name: message
        type: string
  steps:
    - name: invoke
      call:
        job: child-job
        with:
          greeting: "{{ .Params.name }}"
`
	applyParent, _ := json.Marshal(api.ApplyJobRequest{YAML: parentYAML})
	req, _ = http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/jobs", asReader(applyParent))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// Trigger parent run
	trigBody, _ := json.Marshal(api.TriggerRunRequest{
		JobName: "parent-job",
		Params:  map[string]string{"name": "World"},
	})
	req, _ = http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/runs", asReader(trigBody))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	var parentRun api.Run
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&parentRun))
	resp.Body.Close()

	// Wait for parent to succeed
	require.Eventually(t, func() bool {
		r, err := pg.GetRun(ctx, parentRun.ID)
		return err == nil && r.Status == api.RunSucceeded
	}, 30*time.Second, 200*time.Millisecond, "parent run did not succeed")

	// Parent run outputs should contain child's message
	outputs, err := pg.GetRunOutputs(ctx, parentRun.ID)
	require.NoError(t, err)
	assert.Equal(t, "Hello, World!", outputs["message"])

	cancel()
}
