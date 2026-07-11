package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlannedSteps(t *testing.T) {
	const y = `apiVersion: unified-cd/v1
kind: Job
metadata:
  name: j
spec:
  steps:
    - name: checkout
      run: echo hi
    - name: restore-cache
      cache:
        path: p
        key: k
    - name: build
      matrix:
        os: [linux, windows]
      run: echo build
    - name: upload
      uploadArtifact:
        name: a
        path: p
  finally:
    - name: notify
      run: echo done
`
	job, err := dsl.Parse(strings.NewReader(y))
	require.NoError(t, err)
	ps := plannedSteps(job.Spec)

	require.Len(t, ps, 5) // matrix counts as ONE planned entry
	// index/stageIndex are position-based across steps then finally (shared counter)
	assert.Equal(t, "checkout", ps[0].Name)
	assert.Equal(t, "run", ps[0].Kind)
	assert.Equal(t, "main", ps[0].Section)
	assert.Equal(t, 0, ps[0].StageIndex)
	assert.Equal(t, "Pending", ps[0].Status)

	assert.Equal(t, "restore-cache", ps[1].Name)
	assert.Equal(t, "cache", ps[1].Kind)
	assert.Equal(t, 1, ps[1].StageIndex)

	assert.Equal(t, "build", ps[2].Name)
	assert.Equal(t, "run", ps[2].Kind)
	assert.True(t, ps[2].Matrix)
	assert.Equal(t, 2, ps[2].StageIndex)

	assert.Equal(t, "upload", ps[3].Name)
	assert.Equal(t, "uploadArtifact", ps[3].Kind)
	assert.Equal(t, 3, ps[3].StageIndex)

	// finally: section=finally, stageIndex restarts at 0, stepIndex continues
	assert.Equal(t, "notify", ps[4].Name)
	assert.Equal(t, "finally", ps[4].Section)
	assert.Equal(t, 4, ps[4].Index)
	assert.Equal(t, 0, ps[4].StageIndex)
}

func TestMergedRunSteps(t *testing.T) {
	const y = `apiVersion: unified-cd/v1
kind: Job
metadata:
  name: j
spec:
  steps:
    - name: a
      run: echo a
    - name: b
      cache:
        path: p
        key: k
    - name: c
      run: echo c
`
	job, err := dsl.Parse(strings.NewReader(y))
	require.NoError(t, err)

	// only step 0 (a) reported so far
	reported := []api.StepReport{{Index: 0, StageIndex: 0, Name: "a", Status: "Succeeded"}}
	m := mergedRunSteps(reported, job.Spec)

	require.Len(t, m, 3)
	assert.Equal(t, "Succeeded", m[0].Status) // reported wins
	assert.Equal(t, "run", m[0].Kind)         // kind attached from planned
	assert.Equal(t, "Pending", m[1].Status)   // b not reported → pending
	assert.Equal(t, "cache", m[1].Kind)
	assert.Equal(t, "Pending", m[2].Status) // c not reported → pending
}

// TestAPI_GetRunSteps_MergesPlanned verifies that GET /runs/{id}/steps overlays
// reported step statuses onto the full planned step list from the run's stored
// spec, so steps the agent hasn't reported yet still show up as Pending. Uses
// a real Postgres store (via newTestServer/NewTestPostgres) like the other
// controller tests in api_runs_test.go (e.g. TestAPI_GetRun).
func TestAPI_GetRunSteps_MergesPlanned(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	s, pg := newTestServer(t)
	specJSON := []byte(`{"steps":[{"name":"a","run":"echo a"},{"name":"b","cache":{"path":"p","key":"k"}}]}`)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", specJSON)
	run, err := pg.CreateRun(t.Context(), "j", nil, specJSON, nil, nil, "api")
	require.NoError(t, err)

	// Only step 0 ("a") has been reported so far.
	require.NoError(t, pg.UpsertStepReport(t.Context(), run.ID, 0, 0, "a", "", "Succeeded", nil, nil, nil, "", ""))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+run.ID+"/steps", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var got []api.StepReport
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got, 2)
	assert.Equal(t, "a", got[0].Name)
	assert.Equal(t, "Succeeded", got[0].Status)
	assert.Equal(t, "b", got[1].Name)
	assert.Equal(t, "Pending", got[1].Status)
	assert.Equal(t, "cache", got[1].Kind)
}
