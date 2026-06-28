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

// TestPhase4_AgentLabelFilter verifies that an agent with wrong labels cannot claim a labeled run.
func TestPhase4_AgentLabelFilter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("phase 4 is linux/mac only")
	}
	pg := store.NewTestPostgres(t)
	srv := controller.NewServer(controller.Config{Token: "t", AgentToken: "t"}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, "t"))
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go controller.RunScheduler(ctx, pg, 50*time.Millisecond)

	// linux-only agent
	linuxAgent := agent.NewWithLabels("linux-1", []string{"kind:linux"}, agent.NewClient(httpSrv.URL, "t"))
	go func() { _ = linuxAgent.Run(ctx) }()

	// k8s-only job
	k8sJob := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: k8s-only
spec:
  agentSelector:
    - kind:kubernetes
  steps:
    - name: build
      run: echo k8s-step
`
	applyBody, _ := json.Marshal(api.ApplyJobRequest{YAML: k8sJob})
	req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/jobs", asReader(applyBody))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	trigBody, _ := json.Marshal(api.TriggerRunRequest{JobName: "k8s-only"})
	req, _ = http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/runs", asReader(trigBody))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	var run api.Run
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&run))
	resp.Body.Close()

	// Linux agent must NOT claim this job — stays Queued for 2s
	time.Sleep(2 * time.Second)
	r, err := pg.GetRun(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, api.RunQueued, r.Status, "linux agent should not claim a k8s-only run")
}

// TestPhase4_AnyAgentRun verifies that a run with no agentSelector is claimed by any agent.
func TestPhase4_AnyAgentRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("phase 4 is linux/mac only")
	}
	pg := store.NewTestPostgres(t)
	srv := controller.NewServer(controller.Config{Token: "t", AgentToken: "t"}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, "t"))
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go controller.RunScheduler(ctx, pg, 50*time.Millisecond)

	ag := agent.NewWithLabels("agent-any", []string{"kind:linux", "pool:default"}, agent.NewClient(httpSrv.URL, "t"))
	go func() { _ = ag.Run(ctx) }()

	yamlJob := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: any-agent-job
spec:
  steps:
    - name: run
      run: echo hello
`
	applyBody, _ := json.Marshal(api.ApplyJobRequest{YAML: yamlJob})
	req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/jobs", asReader(applyBody))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	trigBody, _ := json.Marshal(api.TriggerRunRequest{JobName: "any-agent-job"})
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
}

// TestPhase4_LabeledAgentClaimsLabeledRun verifies a k8s-labeled agent claims a k8s-only run.
func TestPhase4_LabeledAgentClaimsLabeledRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("phase 4 is linux/mac only")
	}
	pg := store.NewTestPostgres(t)
	srv := controller.NewServer(controller.Config{Token: "t", AgentToken: "t"}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, "t"))
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go controller.RunScheduler(ctx, pg, 50*time.Millisecond)

	// k8s-labeled agent (simulated, not a real k8s agent — just uses the same label)
	k8sAgent := agent.NewWithLabels("k8s-sim-1", []string{"kind:kubernetes", "pool:build"}, agent.NewClient(httpSrv.URL, "t"))
	go func() { _ = k8sAgent.Run(ctx) }()

	k8sJob := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: k8s-targeted
spec:
  agentSelector:
    - kind:kubernetes
  steps:
    - name: build
      run: echo k8s-run
`
	applyBody, _ := json.Marshal(api.ApplyJobRequest{YAML: k8sJob})
	req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/jobs", asReader(applyBody))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	trigBody, _ := json.Marshal(api.TriggerRunRequest{JobName: "k8s-targeted"})
	req, _ = http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/runs", asReader(trigBody))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	var run api.Run
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&run))
	resp.Body.Close()

	// k8s agent SHOULD claim and run this
	require.Eventually(t, func() bool {
		r, err := pg.GetRun(ctx, run.ID)
		return err == nil && r.Status == api.RunSucceeded
	}, 15*time.Second, 100*time.Millisecond, "k8s-labeled agent should claim and succeed k8s-only run")
}
