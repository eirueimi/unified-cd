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

func TestAPI_ListAuditLogs_AdminOnly(t *testing.T) {
	s, pg := newTestServer(t)
	require.NoError(t, pg.InsertAuditLog(context.Background(), "alice", "POST", "/api/v1/jobs", "job.apply", "j1", 200))
	require.NoError(t, pg.InsertAuditLog(context.Background(), "bob", "DELETE", "/api/v1/jobs/j1", "job.delete", "j1", 204))

	// admin (bootstrap token) -> 200 with entries, newest first.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/audit", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var list []api.AuditLog
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list, 2)
	assert.Equal(t, "bob", list[0].Actor)
	assert.Equal(t, "alice", list[1].Actor)
}

func TestAPI_ListAuditLogs_ForbiddenForNonAdmin(t *testing.T) {
	s, pg := newTestServer(t)
	_, err := pg.CreatePAT(context.Background(), "dev", HashToken("dev-token"), "developer", nil)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/audit", nil)
	req.Header.Set("Authorization", "Bearer dev-token")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestAPI_ListAuditLogs_Pagination(t *testing.T) {
	s, pg := newTestServer(t)
	for i := 0; i < 5; i++ {
		require.NoError(t, pg.InsertAuditLog(context.Background(), "alice", "POST", "/api/v1/jobs", "job.apply", "j", 200))
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/audit?limit=2&offset=1", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var list []api.AuditLog
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	assert.Len(t, list, 2)
}

func TestAPI_ListAuditLogs_DefaultLimit(t *testing.T) {
	s, pg := newTestServer(t)
	for i := 0; i < 3; i++ {
		require.NoError(t, pg.InsertAuditLog(context.Background(), "alice", "POST", "/api/v1/jobs", "job.apply", "j", 200))
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/audit", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var list []api.AuditLog
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	assert.Len(t, list, 3)
}
