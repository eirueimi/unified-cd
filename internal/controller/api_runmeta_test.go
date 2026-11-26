package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const runMetaJobYAML = `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: build
  labels:
    team: core
  annotations:
    owner: ci
spec:
  steps:
    - name: s
      run: echo x
`

func applyRunMetaJob(t *testing.T, s *Server) {
	t.Helper()
	b, _ := json.Marshal(api.ApplyJobRequest{YAML: runMetaJobYAML})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func replayRun(t *testing.T, s *Server, id string) api.Run {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+id+"/replay", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var run api.Run
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &run))
	return run
}

func TestCreateRun_TriggerPerSource(t *testing.T) {
	s, pg := newTestServer(t)
	ctx := t.Context()
	applyRunMetaJob(t, s)

	// Manual trigger.
	b, _ := json.Marshal(api.TriggerRunRequest{JobName: "build"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var manual api.Run
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &manual))
	got, err := pg.GetRun(ctx, manual.ID)
	require.NoError(t, err)
	assert.Equal(t, api.TriggerAPI, got.Trigger)
	assert.Nil(t, got.ScheduledAt)

	// Webhook.
	hookSpec, _ := json.Marshal(map[string]any{
		"trigger": map[string]any{"job": "build"},
		"auth":    map[string]any{"type": "none", "allowUnauthenticated": true},
	})
	_, err = pg.UpsertWebhookReceiver(ctx, "meta-hook", hookSpec)
	require.NoError(t, err)
	hreq := httptest.NewRequest(http.MethodPost, "/webhook/meta-hook", bytes.NewReader([]byte(`{}`)))
	hreq.Header.Set("Content-Type", "application/json")
	hrec := httptest.NewRecorder()
	s.Router().ServeHTTP(hrec, hreq)
	require.Equal(t, http.StatusOK, hrec.Code, hrec.Body.String())
	var hook api.Run
	require.NoError(t, json.Unmarshal(hrec.Body.Bytes(), &hook))
	got, err = pg.GetRun(ctx, hook.ID)
	require.NoError(t, err)
	assert.Equal(t, api.TriggerWebhook, got.Trigger)

	// Call child.
	child, status, msg := s.createRunFromJob(ctx, "build", nil, "agent:a1", api.TriggerCall)
	require.Zero(t, status, msg)
	got, err = pg.GetRun(ctx, child.ID)
	require.NoError(t, err)
	assert.Equal(t, api.TriggerCall, got.Trigger)

	// Schedule: ScheduledAt is the cron fire time the scheduler computed.
	now := time.Date(2026, 7, 29, 12, 3, 0, 0, time.UTC)
	_, err = pg.UpsertSchedule(ctx, "meta-sched", "*/5 * * * *", "build", nil)
	require.NoError(t, err)
	require.NoError(t, pg.UpdateScheduleLastFiredAt(ctx, "meta-sched", now.Add(-4*time.Minute)))
	wantFire, err := dsl.NextCronTime("*/5 * * * *", now.Add(-4*time.Minute))
	require.NoError(t, err)
	checkAndFireSchedules(ctx, pg, now)
	runs, err := pg.ListRunsByJob(ctx, "build", 50)
	require.NoError(t, err)
	var schedID string
	for _, r := range runs {
		if r.TriggeredBy == "schedule:meta-sched" {
			schedID = r.ID
		}
	}
	require.NotEmpty(t, schedID, "schedule run not created")
	got, err = pg.GetRun(ctx, schedID)
	require.NoError(t, err)
	assert.Equal(t, api.TriggerSchedule, got.Trigger)
	require.NotNil(t, got.ScheduledAt)
	assert.True(t, wantFire.Equal(*got.ScheduledAt), "want %s got %s", wantFire, *got.ScheduledAt)

	// Replay.
	replay := replayRun(t, s, manual.ID)
	got, err = pg.GetRun(ctx, replay.ID)
	require.NoError(t, err)
	assert.Equal(t, api.TriggerReplay, got.Trigger)
}

func TestCreateRun_CopiesJobLabels(t *testing.T) {
	s, pg := newTestServer(t)
	applyRunMetaJob(t, s)
	run, status, msg := s.createRunFromJob(t.Context(), "build", nil, "api", api.TriggerAPI)
	require.Zero(t, status, msg)
	got, err := pg.GetRun(t.Context(), run.ID)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"team": "core"}, got.JobLabels)
	assert.Equal(t, "ci", got.JobAnnotations["owner"])
}

func TestReplay_DropsScheduleAndParent(t *testing.T) {
	s, pg := newTestServer(t)
	ctx := t.Context()
	applyRunMetaJob(t, s)
	sched := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	spec := []byte(`{"steps":[{"name":"s","run":"echo x"}]}`)
	orig, err := pg.CreateRun(ctx, "build", nil, spec, nil, nil, "schedule:nightly", "",
		store.RunOrigin{Trigger: api.TriggerSchedule, JobLabels: map[string]string{"team": "core"}, ScheduledAt: &sched})
	require.NoError(t, err)

	replay := replayRun(t, s, orig.ID)
	got, err := pg.GetRun(ctx, replay.ID)
	require.NoError(t, err)
	assert.Equal(t, api.TriggerReplay, got.Trigger)
	assert.Nil(t, got.ScheduledAt)
	assert.Equal(t, map[string]string{"team": "core"}, got.JobLabels)

	meta := s.claimRunMeta(ctx, *got)
	assert.Equal(t, orig.ID, meta.ReplayOf)
	assert.Nil(t, meta.Parent)
	assert.Nil(t, meta.ScheduledAt)
}

func TestClaimRunMeta_Parent(t *testing.T) {
	s, pg := newTestServer(t)
	ctx := t.Context()
	applyRunMetaJob(t, s)
	parent, err := pg.CreateRun(ctx, "build", nil, []byte(`{}`), nil, nil, "api", "", store.RunOrigin{Trigger: api.TriggerAPI})
	require.NoError(t, err)
	child, status, msg := s.createRunFromJob(ctx, "build", nil, "agent:a1", api.TriggerCall)
	require.Zero(t, status, msg)
	require.NoError(t, dropFirstTerminal(pg.UpsertStepReport(ctx, parent.ID, 0, 0, "call-step", "", "Running", nil, nil, nil, child.ID, "build")))

	got, err := pg.GetRun(ctx, child.ID)
	require.NoError(t, err)
	meta := s.claimRunMeta(ctx, *got)
	require.NotNil(t, meta.Parent)
	assert.Equal(t, parent.ID, meta.Parent.ParentRunID)
	assert.Equal(t, "build", meta.Parent.ParentJobName)
	assert.Equal(t, "call-step", meta.Parent.StepName)
	assert.Equal(t, api.TriggerCall, meta.Trigger)
	assert.Empty(t, meta.ReplayOf)
}

func TestClaimRunMeta_URL(t *testing.T) {
	run := api.Run{ID: "11111111-1111-1111-1111-111111111111", Trigger: api.TriggerAPI}
	s, _ := newTestServerWithConfig(t, Config{ExternalURL: "https://cd.example.com/"})
	assert.Equal(t, "https://cd.example.com/ui/#/runs/11111111-1111-1111-1111-111111111111", s.claimRunMeta(t.Context(), run).URL)

	s2, _ := newTestServer(t)
	assert.Equal(t, "", s2.claimRunMeta(t.Context(), run).URL)
}
