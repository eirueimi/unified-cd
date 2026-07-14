package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAPI_ReplayRun_UsesSnapshotSpecNotLatestJob verifies replay creates a new
// run from the ORIGINAL run's stored spec snapshot + params, ignoring the job's
// current (re-applied) spec.
func TestAPI_ReplayRun_UsesSnapshotSpecNotLatestJob(t *testing.T) {
	s, pg := newTestServer(t)
	ctx := context.Background()

	// The job's CURRENT spec (B) differs from the run's snapshot (A).
	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{"steps":[{"name":"new","run":"echo latest"}]}`))
	require.NoError(t, err)
	specA := []byte(`{"agentSelector":["kind:linux"],"steps":[{"name":"old","run":"echo snapshot"}]}`)
	orig, err := pg.CreateRun(ctx, "j", map[string]string{"env": "prod"}, specA, []string{"kind:linux"}, nil, "api")
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+orig.ID+"/replay", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var newRun api.Run
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &newRun))
	assert.NotEqual(t, orig.ID, newRun.ID)
	assert.Equal(t, "j", newRun.JobName)
	assert.Equal(t, map[string]string{"env": "prod"}, newRun.Params)

	// The replayed run carries the SNAPSHOT spec (A), not the job's current (B).
	newSpec, err := pg.GetRunSpec(ctx, newRun.ID)
	require.NoError(t, err)
	assert.JSONEq(t, string(specA), string(newSpec))

	got, err := pg.GetRun(ctx, newRun.ID)
	require.NoError(t, err)
	assert.Equal(t, "replay:"+orig.ID, got.TriggeredBy)
}

func TestAPI_ReplayRun_NotFound(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs/00000000-0000-0000-0000-000000000000/replay", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}
