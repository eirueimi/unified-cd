package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/metrics"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newCustomMetricsServer is newTestServer with custom metrics configured the way
// cmd/controller does it: the store hook, the emitter and the gauge collector.
func newCustomMetricsServer(t *testing.T, enabled bool) (*Server, *store.Postgres, *metrics.Metrics) {
	t.Helper()
	s, st := newTestServer(t)
	pg := st.(*store.Postgres)
	m := metrics.New()
	s.SetMetrics(m)
	require.NoError(t, s.ConfigureCustomMetrics(metrics.CustomConfig{Enabled: enabled, MaxSeriesPerMetric: 100, TTL: 24 * time.Hour}, pg))
	pg.SetRunFinishedHook(s.OnRunFinished)
	return s, pg, m
}

func metricsJobYAML(name, specBody string) string {
	return "apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: " + name + "\nspec:\n" + specBody
}

func applyMetricJobYAML(t *testing.T, s *Server, yaml string) (int, string) {
	t.Helper()
	b, _ := json.Marshal(api.ApplyJobRequest{YAML: yaml})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func mustApplyJob(t *testing.T, s *Server, yaml string) {
	t.Helper()
	code, body := applyMetricJobYAML(t, s, yaml)
	require.Equal(t, http.StatusOK, code, body)
}

func scrapeMetrics(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	return rec.Body.String()
}

func customLines(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, "unifiedcd_job_") {
			out = append(out, l)
		}
	}
	return out
}

func createMetricRun(t *testing.T, pg *store.Postgres, job string) *api.Run {
	t.Helper()
	j, err := pg.GetJob(t.Context(), job)
	require.NoError(t, err)
	run, err := pg.CreateRun(t.Context(), job, nil, j.Spec, nil, nil, "", "", store.RunOrigin{})
	require.NoError(t, err)
	return run
}

func agentPostJSON(t *testing.T, s *Server, token, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec
}

func reportStepHTTP(t *testing.T, s *Server, token string, req api.StepReportRequest) {
	t.Helper()
	rec := agentPostJSON(t, s, token, "/api/v1/agents/a1/steps", req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

func finishRunHTTP(t *testing.T, s *Server, token, runID, status string) {
	t.Helper()
	rec := agentPostJSON(t, s, token, "/api/v1/agents/a1/runs/"+runID+"/finish", map[string]string{"status": status})
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

func TestApplyJob_MetricConflict(t *testing.T) {
	s, _, _ := newCustomMetricsServer(t, true)
	mustApplyJob(t, s, metricsJobYAML("a", `  metrics:
    - name: x
      help: h
      counter: {}
      labels: [{key: a, value: v}]
  steps:
    - name: s
      run: echo
`))

	code, body := applyMetricJobYAML(t, s, metricsJobYAML("b", `  metrics:
    - name: x
      help: h
      counter: {}
      labels: [{key: b, value: v}]
  steps:
    - name: s
      run: echo
`))
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Contains(t, body, `metric "x" conflicts with job "a": labels differ`)

	// A gauge x_total exposes the same family as counter x.
	code, body = applyMetricJobYAML(t, s, metricsJobYAML("c", `  metrics:
    - name: x_total
      help: h
      gauge: {value: '1'}
      labels: [{key: a, value: v}]
  steps:
    - name: s
      run: echo
`))
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Contains(t, body, `metric "x_total" conflicts with job "a": family differ`)

	// Re-applying the owner with a new shape is fine: only other jobs conflict.
	mustApplyJob(t, s, metricsJobYAML("a", `  metrics:
    - name: x
      help: h
      counter: {}
      labels: [{key: z, value: v}]
  steps:
    - name: s
      run: echo
`))
}

func TestRunMetrics_CounterOnFinish(t *testing.T) {
	s, pg, m := newCustomMetricsServer(t, true)
	mustApplyJob(t, s, metricsJobYAML("mjob", `  metrics:
    - name: runs
      help: Runs of mjob.
      counter: {}
      labels: [{key: status, value: '{{ .Run.Status }}'}]
  steps:
    - name: s
      run: echo
`))
	run := createMetricRun(t, pg, "mjob")
	claimRunForTest(t, pg, "a1", run.ID)
	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)

	finishRunHTTP(t, s, token, run.ID, "Succeeded")
	assert.Contains(t, scrapeMetrics(t, m), `unifiedcd_job_runs_total{status="Succeeded"} 1`)

	// A second finish is a CAS miss and must not count again.
	rec := agentPostJSON(t, s, token, "/api/v1/agents/a1/runs/"+run.ID+"/finish", map[string]string{"status": "Succeeded"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, scrapeMetrics(t, m), `unifiedcd_job_runs_total{status="Succeeded"} 1`)
}

func TestRunMetrics_CancelledWhileQueued(t *testing.T) {
	s, pg, m := newCustomMetricsServer(t, true)
	mustApplyJob(t, s, metricsJobYAML("mjob", `  metrics:
    - name: runs
      help: Runs of mjob.
      counter: {}
      labels: [{key: status, value: '{{ .Run.Status }}'}]
    - name: dur
      help: Duration.
      gauge: {value: '{{ .Run.Duration }}'}
    - name: live
      help: Live.
      gauge: {value: '{{ .Run.Duration }}', realtime: true}
  steps:
    - name: s
      run: echo
`))
	run := createMetricRun(t, pg, "mjob")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+run.ID+"/cancel", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	text := scrapeMetrics(t, m)
	assert.Contains(t, text, `unifiedcd_job_runs_total{status="Cancelled"} 1`)
	assert.Contains(t, text, "unifiedcd_job_dur 0\n", "never claimed: duration 0")
	rows, err := pg.ListMetricRealtime(t.Context())
	require.NoError(t, err)
	assert.Empty(t, rows)
	assert.NotContains(t, text, "unifiedcd_job_live")
}

func TestStepReport_DuplicateTerminalEmitsOnce(t *testing.T) {
	s, pg, m := newCustomMetricsServer(t, true)
	mustApplyJob(t, s, metricsJobYAML("mjob", `  steps:
    - name: build
      run: exit 1
      metrics:
        - name: step_failures
          help: Failed steps.
          counter: {}
          when: '{{ eq .Step.Status "Failed" }}'
          labels: [{key: step, value: '{{ .Step.Name }}'}]
`))
	run := createMetricRun(t, pg, "mjob")
	claimRunForTest(t, pg, "a1", run.ID)
	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)

	r := api.StepReportRequest{RunID: run.ID, StepIndex: 0, StepName: "build", Status: "Failed", ExitCode: 1, StartedAt: time.Now().Add(-time.Second), EndedAt: time.Now()}
	reportStepHTTP(t, s, token, r)
	reportStepHTTP(t, s, token, r)

	assert.Contains(t, scrapeMetrics(t, m), `unifiedcd_job_step_failures_total{step="build"} 1`)
}

func TestStepMetrics_MatrixPerCombination(t *testing.T) {
	s, pg, m := newCustomMetricsServer(t, true)
	mustApplyJob(t, s, metricsJobYAML("mjob", `  steps:
    - name: build
      matrix:
        os: [linux, windows]
      run: echo
      metrics:
        - name: built
          help: Built.
          counter: {value: '{{ .Outputs.n }}'}
          labels: [{key: os, value: '{{ .Step.MatrixKey }}'}]
`))
	run := createMetricRun(t, pg, "mjob")
	claimRunForTest(t, pg, "a1", run.ID)
	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)

	for variant, n := range map[string]string{"linux": "3", "windows": "5"} {
		rec := agentPostJSON(t, s, token, "/api/v1/agents/a1/runs/"+run.ID+"/steps/0/outputs?variant="+variant, api.SetOutputsRequest{Outputs: map[string]string{"n": n}})
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		reportStepHTTP(t, s, token, api.StepReportRequest{RunID: run.ID, StepIndex: 0, StepName: "build", Variant: variant, Status: "Succeeded", StartedAt: time.Now().Add(-time.Second), EndedAt: time.Now()})
	}

	text := scrapeMetrics(t, m)
	assert.Contains(t, text, `unifiedcd_job_built_total{os="linux"} 3`)
	assert.Contains(t, text, `unifiedcd_job_built_total{os="windows"} 5`)
}

func TestRealtime_RowsLifecycle(t *testing.T) {
	s, pg, m := newCustomMetricsServer(t, true)
	mustApplyJob(t, s, metricsJobYAML("mjob", `  metrics:
    - name: run_live
      help: Live run.
      gauge: {value: '{{ .Run.Duration }}', realtime: true}
  steps:
    - name: build
      run: echo
      metrics:
        - name: step_live
          help: Live step.
          gauge: {value: '{{ .Step.Duration }}', realtime: true}
          labels: [{key: step, value: '{{ .Step.Name }}'}]
`))
	run := createMetricRun(t, pg, "mjob")
	_, err := pg.TransitionPendingToQueued(t.Context(), 10)
	require.NoError(t, err)
	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)

	rec := agentPostJSON(t, s, token, "/api/v1/agents/a1/claim?timeout=2s", api.ClaimRequest{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var claim api.ClaimResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &claim))
	require.Equal(t, run.ID, claim.RunID)

	keys := func() []string {
		rows, err := pg.ListMetricRealtime(t.Context())
		require.NoError(t, err)
		var out []string
		for _, r := range rows {
			out = append(out, r.StepKey+"|"+r.Name)
		}
		return out
	}
	assert.Equal(t, []string{"|unifiedcd_job_run_live"}, keys(), "claim writes the run-level row")
	assert.Contains(t, scrapeMetrics(t, m), "unifiedcd_job_run_live ")

	reportStepHTTP(t, s, token, api.StepReportRequest{RunID: run.ID, StepIndex: 0, StageIndex: 0, StepName: "build", Status: "Running", StartedAt: time.Now()})
	assert.ElementsMatch(t, []string{"|unifiedcd_job_run_live", "0/0/|unifiedcd_job_step_live"}, keys())
	assert.Contains(t, scrapeMetrics(t, m), `unifiedcd_job_step_live{step="build"} `)

	reportStepHTTP(t, s, token, api.StepReportRequest{RunID: run.ID, StepIndex: 0, StageIndex: 0, StepName: "build", Status: "Succeeded", StartedAt: time.Now().Add(-time.Second), EndedAt: time.Now()})
	assert.Equal(t, []string{"|unifiedcd_job_run_live"}, keys(), "the terminal step report removes the step row")

	finishRunHTTP(t, s, token, run.ID, "Succeeded")
	assert.Empty(t, keys(), "the run finish removes the run row")
}

func TestRealtime_RunFinishRemovesLingeringStepRows(t *testing.T) {
	s, pg, _ := newCustomMetricsServer(t, true)
	mustApplyJob(t, s, metricsJobYAML("mjob", `  steps:
    - name: build
      run: echo
      metrics:
        - name: step_live
          help: Live step.
          gauge: {value: '{{ .Step.Duration }}', realtime: true}
`))
	run := createMetricRun(t, pg, "mjob")
	claimRunForTest(t, pg, "a1", run.ID)
	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)
	reportStepHTTP(t, s, token, api.StepReportRequest{RunID: run.ID, StepIndex: 0, StepName: "build", Status: "Running", StartedAt: time.Now()})
	rows, err := pg.ListMetricRealtime(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 1)

	// A cancelled run never gets the step's terminal report.
	finishRunHTTP(t, s, token, run.ID, "Cancelled")
	rows, err = pg.ListMetricRealtime(t.Context())
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestCustomMetrics_Disabled(t *testing.T) {
	s, pg, m := newCustomMetricsServer(t, false)
	mustApplyJob(t, s, metricsJobYAML("mjob", `  metrics:
    - name: runs
      help: Runs.
      counter: {}
    - name: dur
      help: Duration.
      gauge: {value: '{{ .Run.Duration }}'}
    - name: live
      help: Live.
      gauge: {value: '{{ .Run.Duration }}', realtime: true}
  steps:
    - name: build
      run: echo
      metrics:
        - name: steps
          help: Steps.
          counter: {}
`))
	run := createMetricRun(t, pg, "mjob")
	_, err := pg.TransitionPendingToQueued(t.Context(), 10)
	require.NoError(t, err)
	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)
	rec := agentPostJSON(t, s, token, "/api/v1/agents/a1/claim?timeout=2s", api.ClaimRequest{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	reportStepHTTP(t, s, token, api.StepReportRequest{RunID: run.ID, StepIndex: 0, StepName: "build", Status: "Succeeded", StartedAt: time.Now().Add(-time.Second), EndedAt: time.Now()})
	finishRunHTTP(t, s, token, run.ID, "Succeeded")

	assert.Empty(t, customLines(scrapeMetrics(t, m)))
	gauges, err := pg.ListMetricGauges(t.Context(), time.Time{})
	require.NoError(t, err)
	assert.Empty(t, gauges)
	rows, err := pg.ListMetricRealtime(t.Context())
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestResolveClaimStep_MatchesClaimNumbering(t *testing.T) {
	spec := dsl.Spec{
		Steps: []dsl.StepEntry{
			{Name: "checkout", Run: "echo"},
			{Parallel: []dsl.Step{{Name: "lint", Run: "echo"}, {Name: "unit", Run: "echo"}, {Name: "vet", Run: "echo"}}},
			{Name: "build", Run: "echo"},
			{Parallel: []dsl.Step{{Name: "scan", Run: "echo"}, {Name: "sign", Run: "echo"}}},
		},
		Finally: []dsl.StepEntry{
			{Name: "cleanup", Run: "echo"},
			{Parallel: []dsl.Step{{Name: "notify", Run: "echo"}, {Name: "archive", Run: "echo"}}},
			{Name: "report", Run: "echo"},
		},
	}
	specJSON, err := json.Marshal(spec)
	require.NoError(t, err)
	var parsed dsl.Spec
	require.NoError(t, json.Unmarshal(specJSON, &parsed))

	resp, err := buildClaimResponse(&store.ClaimedRun{Run: api.Run{ID: "r", JobName: "j"}, Spec: specJSON}, nil)
	require.NoError(t, err)

	seen := 0
	check := func(cs api.ClaimStep) {
		got, ok := resolveClaimStep(parsed, cs.StageIndex, cs.Index)
		require.True(t, ok, "step %q (stage %d, index %d) must resolve", cs.Name, cs.StageIndex, cs.Index)
		assert.Equal(t, cs.Name, got.Name, "stage %d index %d", cs.StageIndex, cs.Index)
		seen++
	}
	for _, stages := range [][]api.ClaimStage{resp.Stages, resp.Finally} {
		for _, st := range stages {
			if st.Step != nil {
				check(*st.Step)
			}
			for _, p := range st.Parallel {
				check(p)
			}
		}
	}
	assert.Equal(t, 11, seen)

	// Wrong coordinates do not resolve to a neighbour.
	_, ok := resolveClaimStep(parsed, 0, 99)
	assert.False(t, ok)
	_, ok = resolveClaimStep(parsed, 3, 0)
	assert.False(t, ok, "index 0 lives in stage 0")
}

const plainSteps = `  steps:
    - name: s
      run: echo
`

func TestCustomMetricsSweeper_CleansOldGaugesAndTerminalRealtimeRows(t *testing.T) {
	s, pg, _ := newCustomMetricsServer(t, true)
	g := store.MetricGauge{Name: "unifiedcd_job_g", Help: "h", Labels: map[string]string{}, Value: 1, UpdatedAt: time.Now()}
	require.NoError(t, pg.ApplyMetricGauge(t.Context(), g, "h1", "Set", 0))

	mustApplyJob(t, s, metricsJobYAML("plain", plainSteps))
	terminal := createMetricRun(t, pg, "plain")
	live := createMetricRun(t, pg, "plain")
	for _, id := range []string{terminal.ID, live.ID} {
		require.NoError(t, pg.PutMetricRealtime(t.Context(), store.MetricRealtime{RunID: id, Name: "unifiedcd_job_x", Help: "h", Labels: map[string]string{}, StartedAt: time.Now()}))
	}
	_, err := pg.FinishRun(t.Context(), terminal.ID, api.RunFailed)
	require.NoError(t, err)

	// A clock 48h ahead makes the gauge row older than the 24h TTL.
	_, _, err = cleanCustomMetricsAsLeader(t.Context(), pg, 24*time.Hour, time.Now().Add(48*time.Hour))
	require.NoError(t, err)
	gauges, err := pg.ListMetricGauges(t.Context(), time.Time{})
	require.NoError(t, err)
	assert.Empty(t, gauges, "old gauge row removed")
	rows, err := pg.ListMetricRealtime(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 1, "only the live run keeps its realtime row")
	assert.Equal(t, live.ID, rows[0].RunID)
}

// countingSpecStore counts GetRunSpec calls.
type countingSpecStore struct {
	store.Store
	specCalls int
}

func (c *countingSpecStore) GetRunSpec(ctx context.Context, id string) ([]byte, error) {
	c.specCalls++
	return c.Store.GetRunSpec(ctx, id)
}

// A run of a job without metrics costs no spec load at finish or step report.
func TestRunMetrics_MetricFreeRunLoadsNoSpec(t *testing.T) {
	s, pg, _ := newCustomMetricsServer(t, true)
	mustApplyJob(t, s, metricsJobYAML("plain", plainSteps))
	run := createMetricRun(t, pg, "plain")
	_, err := pg.TransitionPendingToQueued(t.Context(), 10)
	require.NoError(t, err)
	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)
	rec := agentPostJSON(t, s, token, "/api/v1/agents/a1/claim?timeout=2s", api.ClaimRequest{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	counting := &countingSpecStore{Store: pg}
	s.custom.st = counting
	reportStepHTTP(t, s, token, api.StepReportRequest{RunID: run.ID, StepIndex: 0, StepName: "s", Status: "Succeeded", StartedAt: time.Now().Add(-time.Second), EndedAt: time.Now()})
	s.OnRunFinished(t.Context(), counting, run.ID, api.RunSucceeded)
	assert.Zero(t, counting.specCalls)
}

// A run the store fails itself (here: an unexpandable concurrency template while
// queueing) still emits its run-level metrics, exactly once.
func TestRunMetrics_StoreInternalFinishEmitsOnce(t *testing.T) {
	s, pg, m := newCustomMetricsServer(t, true)
	mustApplyJob(t, s, metricsJobYAML("mjob", `  metrics:
    - name: runs
      help: Runs of mjob.
      counter: {}
      labels: [{key: status, value: '{{ .Run.Status }}'}]
  steps:
    - name: s
      run: echo
`))
	j, err := pg.GetJob(t.Context(), "mjob")
	require.NoError(t, err)
	var spec map[string]any
	require.NoError(t, json.Unmarshal(j.Spec, &spec))
	spec["concurrency"] = map[string]any{"mutex": "deploy-{{ .Params.env"} // missing closing }}
	bad, err := json.Marshal(spec)
	require.NoError(t, err)
	_, err = pg.CreateRun(t.Context(), "mjob", map[string]string{"env": "prod"}, bad, nil, nil, "", "", store.RunOrigin{})
	require.NoError(t, err)

	_, err = pg.TransitionPendingToQueued(t.Context(), 10)
	require.NoError(t, err)
	assert.Contains(t, scrapeMetrics(t, m), `unifiedcd_job_runs_total{status="Failed"} 1`)
}
