package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mustCreateRun creates a job named jobName (if not already present) and a Run
// for it, returning the new run's ID. Mirrors the store-package helper of the
// same name, adapted to the controller package's store.Store interface.
func mustCreateRun(t *testing.T, pg store.Store, jobName string) string {
	t.Helper()
	ctx := t.Context()

	_, err := pg.UpsertJob(ctx, jobName, "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)

	run, err := pg.CreateRun(ctx, jobName, nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	return run.ID
}

// getRunViaAPI issues GET /api/v1/runs/{id} against the server and decodes the response.
func getRunViaAPI(t *testing.T, s *Server, id string) api.Run {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+id, nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var run api.Run
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &run))
	return run
}

func TestGetRun_CalledBy(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	srv, pg := newTestServer(t) // use the existing controller test harness
	ctx := t.Context()

	parent := mustCreateRun(t, pg, "parent-job")
	child := mustCreateRun(t, pg, "child-job")
	require.NoError(t, pg.UpsertStepReport(ctx, parent, 0, 0, "call-child", "", "Succeeded", nil, nil, nil, child, "child-job"))

	// GET /runs/{child} → response.calledBy points at the parent
	run := getRunViaAPI(t, srv, child)
	require.NotNil(t, run.CalledBy)
	assert.Equal(t, parent, run.CalledBy.ParentRunID)
	assert.Equal(t, "parent-job", run.CalledBy.ParentJobName)

	// GET /runs/{parent} → no calledBy
	prun := getRunViaAPI(t, srv, parent)
	assert.Nil(t, prun.CalledBy)
}
