package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAPI_GetJobSchedulability_Satisfiable verifies the endpoint loads the
// job's stored spec, evaluates it against registered agents, and reports a
// satisfiable schedule when a capable agent is online.
func TestAPI_GetJobSchedulability_Satisfiable(t *testing.T) {
	s, pg := newTestServer(t)
	require.NoError(t, pg.UpsertAgent(t.Context(), "ag1", "host1", "linux", "dev", []string{"kind:docker"}, []string{"native", "container"}, nil))
	specJSON := []byte(`{"native":true,"steps":[{"name":"build","run":"echo hi"}]}`)
	_, err := pg.UpsertJob(t.Context(), "build", "unified-cd/v1", specJSON)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/build/schedulability", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got Schedulability
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.True(t, got.Satisfiable)
	assert.Equal(t, []string{"native"}, got.RequiredCaps)
}

// TestAPI_GetJobSchedulability_Unsatisfiable verifies the endpoint reports an
// unsatisfiable schedule (with a reason) when no online agent covers the
// job's required capability.
func TestAPI_GetJobSchedulability_Unsatisfiable(t *testing.T) {
	s, pg := newTestServer(t)
	require.NoError(t, pg.UpsertAgent(t.Context(), "ag1", "host1", "linux", "dev", []string{"kind:k8s"}, []string{"pod", "container"}, nil))
	specJSON := []byte(`{"native":true,"steps":[{"name":"build","run":"echo hi"}]}`)
	_, err := pg.UpsertJob(t.Context(), "build", "unified-cd/v1", specJSON)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/build/schedulability", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got Schedulability
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.False(t, got.Satisfiable)
	assert.Contains(t, got.Reason, "native")
}

// TestAPI_GetJobSchedulability_NotFound verifies a missing job yields 404.
func TestAPI_GetJobSchedulability_NotFound(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/missing/schedulability", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestAPI_GetJobSchedulability_HierarchicalName verifies the wildcard-suffix
// dispatch correctly separates a hierarchical job name (containing "/") from
// the "/schedulability" discriminator.
func TestAPI_GetJobSchedulability_HierarchicalName(t *testing.T) {
	s, pg := newTestServer(t)
	require.NoError(t, pg.UpsertAgent(t.Context(), "ag1", "host1", "linux", "dev", []string{"kind:docker"}, []string{"native", "container"}, nil))
	_, err := pg.UpsertJob(t.Context(), "team-a/build", "unified-cd/v1", []byte(`{"native":true}`))
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/team-a/build/schedulability", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got Schedulability
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.True(t, got.Satisfiable)
}
