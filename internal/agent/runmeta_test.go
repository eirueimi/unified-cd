package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fullClaim() api.ClaimResponse {
	jst := time.FixedZone("JST", 9*3600)
	sched := time.Date(2026, 11, 26, 8, 0, 0, 0, jst)
	return api.ClaimResponse{
		RunID:   "run-1",
		JobName: "build",
		Params:  map[string]string{"b": "2", "a": "1"},
		Run: &api.ClaimRunMeta{
			CreatedAt:      time.Date(2026, 11, 26, 9, 0, 0, 0, jst),
			TriggeredBy:    "alice",
			Trigger:        "schedule",
			ScheduledAt:    &sched,
			ReplayOf:       "run-0",
			Parent:         &api.CalledBy{ParentRunID: "p-1", ParentJobName: "parent", StepName: "call-it"},
			DisplayName:    "nightly",
			URL:            "https://cd.example.com/runs/run-1",
			JobLabels:      map[string]string{"team": "core", "env": "ci"},
			JobAnnotations: map[string]string{"note": "x"},
		},
	}
}

func TestRunMetaEnv_Full(t *testing.T) {
	started := time.Date(2026, 11, 26, 0, 1, 0, 0, time.UTC)
	m := newRunMeta(fullClaim(), "agent-7", started)
	got := m.env(dsl.CurrentStepData{Name: "compile", Attempt: 1, MaxAttempts: 3}, nil)
	assert.ElementsMatch(t, []string{
		"UNIFIED_RUN_ID=run-1",
		"UNIFIED_RUN_URL=https://cd.example.com/runs/run-1",
		"UNIFIED_RUN_CREATED_AT=2026-11-26T00:00:00Z",
		"UNIFIED_RUN_STARTED_AT=2026-11-26T00:01:00Z",
		"UNIFIED_RUN_TRIGGERED_BY=alice",
		"UNIFIED_RUN_TRIGGER=schedule",
		"UNIFIED_RUN_SCHEDULED_AT=2026-11-25T23:00:00Z",
		"UNIFIED_RUN_PARENT_ID=p-1",
		"UNIFIED_RUN_PARENT_JOB=parent",
		"UNIFIED_RUN_PARENT_STEP=call-it",
		"UNIFIED_RUN_REPLAY_OF=run-0",
		"UNIFIED_RUN_DISPLAY_NAME=nightly",
		`UNIFIED_RUN_PARAMS_JSON={"a":"1","b":"2"}`,
		"UNIFIED_RUN_AGENT_ID=agent-7",
		"UNIFIED_JOB_NAME=build",
		`UNIFIED_JOB_LABELS_JSON={"env":"ci","team":"core"}`,
		`UNIFIED_JOB_ANNOTATIONS_JSON={"note":"x"}`,
		"UNIFIED_STEP_NAME=compile",
		"UNIFIED_STEP_ATTEMPT=1",
		"UNIFIED_STEP_MAX_ATTEMPTS=3",
	}, got)

	// Attempt 2 adds the previous exit code; finally adds the frozen outcome.
	fv := &finalView{Status: "Failed", Duration: 95 * time.Second,
		Failures: []dsl.StepFailure{{Name: "compile", Stage: 1, ExitCode: 2}}}
	got = m.env(dsl.CurrentStepData{Name: "compile", Attempt: 2, MaxAttempts: 3, LastExitCode: 7}, fv)
	assert.Contains(t, got, "UNIFIED_STEP_LAST_EXIT_CODE=7")
	assert.Contains(t, got, "UNIFIED_RUN_STATUS=Failed")
	assert.Contains(t, got, "UNIFIED_RUN_DURATION_SECONDS=95")
	var haveFailures bool
	for _, e := range got {
		if strings.HasPrefix(e, "UNIFIED_RUN_FAILURES_JSON=") {
			haveFailures = true
			assert.Contains(t, e, `"name":"compile"`)
		}
	}
	assert.True(t, haveFailures)

	// Post hooks pass a zero step: no UNIFIED_STEP_* entries.
	for _, e := range m.env(dsl.CurrentStepData{}, nil) {
		assert.False(t, strings.HasPrefix(e, "UNIFIED_STEP_"), e)
	}
}

func TestRunMetaEnv_NilClaimRun(t *testing.T) {
	c := api.ClaimResponse{RunID: "r", JobName: "j"}
	m := newRunMeta(c, "ag", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	got := m.env(dsl.CurrentStepData{Name: "s", Attempt: 1, MaxAttempts: 1}, nil)
	for _, want := range []string{
		"UNIFIED_RUN_ID=r", "UNIFIED_RUN_STARTED_AT=2026-01-02T03:04:05Z",
		"UNIFIED_RUN_PARAMS_JSON={}", "UNIFIED_RUN_AGENT_ID=ag", "UNIFIED_JOB_NAME=j",
		"UNIFIED_STEP_NAME=s", "UNIFIED_STEP_ATTEMPT=1", "UNIFIED_STEP_MAX_ATTEMPTS=1",
	} {
		assert.Contains(t, got, want)
	}
	for _, e := range got {
		for _, absent := range []string{"UNIFIED_RUN_URL=", "UNIFIED_RUN_TRIGGER=", "UNIFIED_RUN_CREATED_AT="} {
			assert.False(t, strings.HasPrefix(e, absent), e)
		}
	}
	r, j, a := m.templateData(dsl.CurrentStepData{}, nil)
	assert.Equal(t, "r", r.ID)
	assert.True(t, r.CreatedAt.IsZero())
	assert.Equal(t, "j", j.Name)
	assert.Equal(t, "ag", a.ID)
}

func TestRunMetaEnv_AbsentNotEmpty(t *testing.T) {
	c := api.ClaimResponse{RunID: "r", JobName: "j", Run: &api.ClaimRunMeta{Trigger: "manual"}}
	m := newRunMeta(c, "ag", time.Now())
	for _, e := range m.env(dsl.CurrentStepData{Name: "s", Attempt: 1, MaxAttempts: 1}, &finalView{Status: "Succeeded"}) {
		assert.False(t, strings.HasSuffix(e, "="), "empty value exported: %s", e)
	}
}

func TestFailuresJSON_Truncates(t *testing.T) {
	var f []dsl.StepFailure
	for i := 0; i < 5000; i++ {
		f = append(f, dsl.StepFailure{Name: "step", Stage: i, ExitCode: 1})
	}
	out := failuresJSON(f, 32768)
	assert.LessOrEqual(t, len(out), 32768)
	var arr []map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &arr), "truncated output must still be valid JSON")
	last := arr[len(arr)-1]
	trunc, ok := last["truncated"].(float64)
	require.True(t, ok, "last element must be the truncation marker")
	assert.Equal(t, 5000, len(arr)-1+int(trunc))

	assert.Equal(t, "[]", failuresJSON(nil, 100))
	assert.NotContains(t, failuresJSON(f[:2], 32768), "truncated")
}

func TestSetStepResult_AggregatesMatrixAndKeepsOutputs(t *testing.T) {
	s := &safeStepCtx{data: dsl.TemplateData{Steps: map[string]dsl.StepData{}}}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.setStepMatrixOutputs("m", "a", map[string]string{"o": "1"})
	s.setStepResult("m", dsl.StepData{Status: "Succeeded", StartedAt: t0.Add(time.Second), EndedAt: t0.Add(2 * time.Second)})
	s.setStepResult("m", dsl.StepData{Status: "Failed", ExitCode: 3, StartedAt: t0, EndedAt: t0.Add(5 * time.Second)})
	s.setStepResult("m", dsl.StepData{Status: "Succeeded", StartedAt: t0.Add(time.Second), EndedAt: t0.Add(time.Second)})
	got := s.snapshot().Steps["m"]
	assert.Equal(t, "Failed", got.Status)
	assert.Equal(t, 3, got.ExitCode)
	assert.Equal(t, t0, got.StartedAt)
	assert.Equal(t, t0.Add(5*time.Second), got.EndedAt)
	assert.NotNil(t, got.Outputs["o"])
}

func TestStepEnvPrefixes_IncludeMetadata(t *testing.T) {
	p := SynthesizedStepEnvPrefixes()
	assert.Contains(t, p, dsl.RunEnvPrefix)
	assert.Contains(t, p, dsl.JobEnvPrefix)
	assert.Contains(t, p, dsl.StepEnvPrefix)
}

func runStagesClaim(t *testing.T, name string, c api.ClaimResponse) string {
	t.Helper()
	agentID := "meta-agent-" + name
	h := &varsTestHarness{}
	srv := newVarsTestServer(t, agentID, h)
	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}
	c.Native = true
	c.RunID = "run-" + name
	if c.JobName == "" {
		c.JobName = name
	}
	a.executeRun(context.Background(), c, t.TempDir())
	return h.output()
}

func TestRetry_AttemptEnvAndTemplate(t *testing.T) {
	script := `echo "attempt=$UNIFIED_STEP_ATTEMPT max=$UNIFIED_STEP_MAX_ATTEMPTS last=[$UNIFIED_STEP_LAST_EXIT_CODE] tpl=[echo {{ .Step.Attempt }}]"
[ "$UNIFIED_STEP_ATTEMPT" = "2" ] || exit 7`
	out := runStagesClaim(t, "retry", api.ClaimResponse{Stages: []api.ClaimStage{{Step: &api.ClaimStep{
		Name: "flaky", Run: script, Retry: &dsl.RetrySpec{Attempts: 2},
	}}}})
	assert.Contains(t, out, "attempt=1 max=2 last=[] tpl=[echo 1]")
	assert.Contains(t, out, "attempt=2 max=2 last=[7] tpl=[echo 2]")
}

func TestStepsStatus_VisibleAfterFailure(t *testing.T) {
	out := runStagesClaim(t, "stepsstatus", api.ClaimResponse{Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "a", Run: "exit 4", ContinueOnError: true}},
		{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "b", Run: `echo "a=[{{ .Steps.a.Status }}] code=[{{ .Steps.a.ExitCode }}]"`}},
	}})
	assert.Contains(t, out, "a=[Failed] code=[4]")
}

func TestFinally_StatusAndFailures(t *testing.T) {
	show := `echo "status=[$UNIFIED_RUN_STATUS] failures=[$UNIFIED_RUN_FAILURES_JSON] dur=[$UNIFIED_RUN_DURATION_SECONDS] tpl=[{{ .Run.Status }}]"`
	out := runStagesClaim(t, "finalmeta", api.ClaimResponse{
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "main-show", Run: `echo "main-status=[$UNIFIED_RUN_STATUS] main-dur=[$UNIFIED_RUN_DURATION_SECONDS]"`}},
			{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "boom", Run: "exit 5"}},
		},
		Finally: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 2, StageIndex: 0, Name: "cleanup", Run: show}},
		},
	})
	assert.Contains(t, out, "status=[Failed]")
	assert.Contains(t, out, `"name":"boom"`)
	assert.Contains(t, out, `"exitCode":5`)
	assert.Contains(t, out, "tpl=[Failed]")
	assert.Contains(t, out, " dur=[0] ")
	assert.Contains(t, out, "main-status=[] main-dur=[]")
}

func TestStepEnv_BuiltinWinsOverReservedStepEnv(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	out := runStagesClaim(t, "builtinwins", api.ClaimResponse{Stages: []api.ClaimStage{{Step: &api.ClaimStep{
		Name: "show", Env: map[string]string{"UNIFIED_RUN_ID": "x"}, Run: `echo "id=[$UNIFIED_RUN_ID]"`,
	}}}})
	assert.Contains(t, out, "id=[run-builtinwins]")
	assert.Contains(t, buf.String(), "step env overrides reserved run metadata; built-in value wins")
	assert.Contains(t, buf.String(), "UNIFIED_RUN_ID")
}
