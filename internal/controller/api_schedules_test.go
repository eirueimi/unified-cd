package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/unified-cd/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testScheduleYAML = `
apiVersion: unified-cd/v1
kind: Schedule
metadata:
  name: nightly-build
spec:
  cron: "0 2 * * *"
  job: build
  params:
    env: prod
`

func TestAPI_ApplySchedule(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "build", "unified-cd/v1", []byte(`{"steps":[{"name":"s","run":"echo x"}]}`))

	body, _ := json.Marshal(api.ApplyScheduleRequest{YAML: testScheduleYAML})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/schedules", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var meta api.ScheduleMeta
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &meta))
	assert.Equal(t, "nightly-build", meta.Name)
	assert.Equal(t, "0 2 * * *", meta.Cron)
	assert.Equal(t, "build", meta.JobName)
}

func TestAPI_ApplySchedule_InvalidCron(t *testing.T) {
	s, _ := newTestServer(t)
	const badYAML = `
apiVersion: unified-cd/v1
kind: Schedule
metadata:
  name: bad
spec:
  cron: "not-a-cron"
  job: build
`
	body, _ := json.Marshal(api.ApplyScheduleRequest{YAML: badYAML})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/schedules", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAPI_ApplySchedule_JobNotFound(t *testing.T) {
	s, _ := newTestServer(t)
	// "nonexistent-job" is not registered.
	body, _ := json.Marshal(api.ApplyScheduleRequest{YAML: testScheduleYAML}) // references "build"
	req := httptest.NewRequest(http.MethodPost, "/api/v1/schedules", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAPI_ListSchedules(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "build", "unified-cd/v1", []byte(`{}`))
	_, _ = pg.UpsertSchedule(t.Context(), "nightly-build", "0 2 * * *", "build", nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/schedules", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var list []api.ScheduleMeta
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	assert.Len(t, list, 1)
	assert.Equal(t, "nightly-build", list[0].Name)
}

func TestAPI_DeleteSchedule(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "build", "unified-cd/v1", []byte(`{}`))
	_, _ = pg.UpsertSchedule(t.Context(), "nightly-build", "0 2 * * *", "build", nil)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/schedules/nightly-build", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestAPI_DeleteSchedule_NotFound(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/schedules/nonexistent", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNoContent, rec.Code) // DELETE is idempotent — returns 204 even when not found.
}
