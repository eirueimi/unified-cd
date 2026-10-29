package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const auditDecisionPolicy = `
global:
  groups:
    "g-sc": [schedule.configure]
folders:
  team-a:
    groups:
      "my-org:team-a": developer
  team-b:
    groups:
      "g-b": developer
`

func auditSeedRun(t *testing.T, st store.Store, job string) string {
	t.Helper()
	ctx := context.Background()
	_, err := st.UpsertJob(ctx, job, "unified-cd/v1", []byte(`{"steps":[]}`))
	require.NoError(t, err)
	run, err := st.CreateRun(ctx, job, nil, []byte(`{"steps":[]}`), nil, nil, "api", "")
	require.NoError(t, err)
	_, err = st.FinishRun(ctx, run.ID, api.RunSucceeded)
	require.NoError(t, err)
	return run.ID
}

func auditRows(t *testing.T, st store.Store) []api.AuditLog {
	t.Helper()
	logs, err := st.ListAuditLogs(context.Background(), 50, 0)
	require.NoError(t, err)
	return logs
}

func TestAudit_ReplayRecorded(t *testing.T) {
	s, pg := newTestServer(t)
	id := auditSeedRun(t, pg, "j")

	rec := doAs(t, s, http.MethodPost, "/api/v1/runs/"+id+"/replay", "secret", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	logs := auditRows(t, pg)
	require.Len(t, logs, 1)
	assert.Equal(t, "run.replay", logs[0].Action)
	assert.Equal(t, id, logs[0].Resource)
	assert.Equal(t, "test-bootstrap", logs[0].Actor)
	assert.Equal(t, "POST", logs[0].Method)
	assert.Equal(t, "/api/v1/runs/"+id+"/replay", logs[0].Path)
	assert.Equal(t, 200, logs[0].Status)
	assert.Equal(t, "run.replay", logs[0].Permission)
	assert.Equal(t, "j", logs[0].Scope)
}

func TestAudit_AllowedWriteRecordsGrant(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{}, auditDecisionPolicy)
	_, err := pg.UpsertJob(t.Context(), "team-a/x", "unified-cd/v1", []byte(`{"steps":[{"name":"s","run":"echo hi"}]}`))
	require.NoError(t, err)

	rec := bodyPost(t, s, "/api/v1/runs", bearer("dev@example.com", "my-org:team-a"), api.TriggerRunRequest{JobName: "team-a/x"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	logs := auditRows(t, pg)
	require.Len(t, logs, 1)
	assert.Equal(t, "run.trigger", logs[0].Action)
	assert.Equal(t, "run.trigger", logs[0].Permission)
	assert.Equal(t, "team-a/x", logs[0].Scope)
	assert.Equal(t, `folder:team-a (groups["my-org:team-a"])`, logs[0].GrantedBy)
	assert.Equal(t, 200, logs[0].Status)
}

func TestAudit_LegacyRoleGrantedBy(t *testing.T) {
	s, pg := newTestServer(t)
	s.cfg.Token = "secret" // break-glass requires the configured bootstrap token
	makeRolePAT(t, pg, "dev-token", "developer")
	id := auditSeedRun(t, pg, "j")

	rec := doAs(t, s, http.MethodPost, "/api/v1/runs/"+id+"/cancel", "dev-token", nil)
	require.NotEqual(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = doAs(t, s, http.MethodPost, "/api/v1/runs/"+id+"/cancel", "secret", nil)
	require.NotEqual(t, http.StatusForbidden, rec.Code, rec.Body.String())

	logs := auditRows(t, pg) // newest first
	require.Len(t, logs, 2)
	assert.Equal(t, "test-bootstrap", logs[0].Actor)
	assert.Equal(t, "break-glass", logs[0].GrantedBy)
	assert.Equal(t, "developer-user", logs[1].Actor)
	assert.Equal(t, "role:developer (pat)", logs[1].GrantedBy)
	assert.Equal(t, "run.cancel", logs[1].Permission)
	assert.Equal(t, "j", logs[1].Scope)
}

func TestAudit_DeniedWriteRecorded(t *testing.T) {
	s, pg := newTestServer(t)
	makeRolePAT(t, pg, "view-token", "viewer")

	rec := doAs(t, s, http.MethodPost, "/api/v1/jobs", "view-token", []byte(`{}`))
	require.Equal(t, http.StatusForbidden, rec.Code)

	logs := auditRows(t, pg)
	require.Len(t, logs, 1)
	assert.Equal(t, "job.apply", logs[0].Action)
	assert.Equal(t, "viewer-user", logs[0].Actor)
	assert.Equal(t, 403, logs[0].Status)
	assert.Equal(t, "job.configure", logs[0].Permission)
	assert.Empty(t, logs[0].Scope)
	assert.Empty(t, logs[0].GrantedBy)
}

// entryStore captures whole audit entries.
type entryStore struct{ entries []store.AuditEntry }

func (f *entryStore) InsertAuditLog(_ context.Context, e store.AuditEntry) error {
	f.entries = append(f.entries, e)
	return nil
}

// A write on a route absent from auditActionTable is not recorded when it is
// allowed, and is recorded the way a denied read is when authorization refuses
// it (no such human route exists yet, so the middleware is driven directly).
// A refusal by the pre-check on a job-scoped route happens before any job is
// known, so the scope is empty.
func TestAudit_PreCheckDenialOnJobRouteHasEmptyScope(t *testing.T) {
	s, pg := newTestServer(t)
	makeRolePAT(t, pg, "view-token", "viewer")

	rec := doAs(t, s, http.MethodPost, "/api/v1/runs", "view-token", []byte(`{"jobName":"j"}`))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	logs := auditRows(t, pg)
	require.Len(t, logs, 1)
	assert.Equal(t, "run.trigger", logs[0].Action)
	assert.Equal(t, "run.trigger", logs[0].Permission)
	assert.Empty(t, logs[0].Scope)
	assert.Empty(t, logs[0].GrantedBy)
}

func TestAudit_DeniedUnclassifiedWriteRecorded(t *testing.T) {
	st := &entryStore{}
	router := chi.NewRouter()
	router.Use(auditLogMiddleware(st))
	router.Post("/api/v1/unlisted/deny", func(w http.ResponseWriter, r *http.Request) {
		deny(w, r, authz.JobRead, "team-b/x", http.StatusForbidden, "no")
	})
	router.Post("/api/v1/unlisted/allow", func(w http.ResponseWriter, r *http.Request) {
		recordDecision(r, authzDecision{Permission: authz.JobRead, Scope: "x", GrantedBy: "role:admin (pat)"})
		w.WriteHeader(http.StatusOK)
	})

	for _, path := range []string{"/api/v1/unlisted/deny", "/api/v1/unlisted/allow"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"value":"secret-xyz"}`)))
	}

	require.Len(t, st.entries, 1)
	e := st.entries[0]
	assert.Equal(t, "job.read", e.Action)
	assert.Equal(t, "team-b/x", e.Resource)
	assert.Equal(t, "job.read", e.Permission)
	assert.Equal(t, "team-b/x", e.Scope)
	assert.Equal(t, 403, e.Status)
	assert.Equal(t, "POST", e.Method)
	assert.Equal(t, "/api/v1/unlisted/deny", e.Path)
	assert.Empty(t, e.GrantedBy)
}

func TestAudit_DeniedReadRecordedEvenAs404(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{}, auditDecisionPolicy)
	teamB := auditSeedRun(t, pg, "team-b/x")
	tok := bearer("dev@example.com", "my-org:team-a")

	rec := doAs(t, s, http.MethodGet, "/api/v1/runs/"+teamB+"/logs", tok, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	rec = doAs(t, s, http.MethodGet, "/api/v1/runs/"+teamB+"/events", tok, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)

	logs := auditRows(t, pg)
	require.Len(t, logs, 2)
	paths := map[string]bool{}
	for _, l := range logs {
		assert.Equal(t, "job.read", l.Action)
		assert.Equal(t, "job.read", l.Permission)
		assert.Equal(t, "team-b/x", l.Resource)
		assert.Equal(t, "team-b/x", l.Scope)
		assert.Equal(t, 404, l.Status)
		assert.Equal(t, "GET", l.Method)
		assert.Empty(t, l.GrantedBy)
		paths[l.Path] = true
	}
	assert.True(t, paths["/api/v1/runs/"+teamB+"/logs"])
	assert.True(t, paths["/api/v1/runs/"+teamB+"/events"])
}

// A hidden answer is recorded with the status the client actually saw, which
// is not always 404: a delete of a schedule whose target the caller cannot
// read is answered 204.
func TestAudit_HiddenAnswerRecordsActualStatus(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{}, auditDecisionPolicy)
	_, err := pg.UpsertJob(t.Context(), "team-a/x", "unified-cd/v1", []byte(`{"steps":[]}`))
	require.NoError(t, err)
	_, err = pg.UpsertSchedule(t.Context(), "sch", "0 3 * * *", "team-a/x", nil)
	require.NoError(t, err)

	rec := doAs(t, s, http.MethodDelete, "/api/v1/schedules/sch", bearer("sc@example.com", "g-sc"), nil)
	require.Equal(t, http.StatusNoContent, rec.Code)

	logs := auditRows(t, pg)
	require.Len(t, logs, 1)
	assert.Equal(t, "schedule.delete", logs[0].Action)
	assert.Equal(t, "schedule.configure", logs[0].Permission)
	assert.Equal(t, "team-a/x", logs[0].Scope)
	assert.Equal(t, 204, logs[0].Status)
	assert.Empty(t, logs[0].GrantedBy)
	_, err = pg.GetSchedule(t.Context(), "sch")
	require.NoError(t, err, "the hidden delete must not have touched the schedule")
}

func TestAudit_AllowedReadNotRecorded(t *testing.T) {
	s, pg := newTestServer(t)
	makeRolePAT(t, pg, "view-token", "viewer")

	rec := doAs(t, s, http.MethodGet, "/api/v1/jobs", "view-token", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, auditRows(t, pg))
}

// A refused secret write is recorded by name only (never the request body),
// and none of the new decision columns can carry a secret value.
func TestAudit_SecretBodyNeverRecorded(t *testing.T) {
	s, pg := newTestServerWithKM(t)
	makeRolePAT(t, pg, "dev-token", "developer")
	const value = "super-secret-value-xyz"

	body, _ := json.Marshal(api.SetSecretRequest{Name: "AWS_KEY", Value: value})
	rec := doAs(t, s, http.MethodPost, "/api/v1/secrets", "dev-token", body)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	logs := auditRows(t, pg)
	require.Len(t, logs, 1)
	assert.Equal(t, "secret.set", logs[0].Action)
	assert.Equal(t, "AWS_KEY", logs[0].Resource)
	assert.Equal(t, 403, logs[0].Status)
	assert.NotEmpty(t, logs[0].Permission)
	raw, err := json.Marshal(logs[0])
	require.NoError(t, err)
	assert.False(t, strings.Contains(string(raw), value), "audit row leaked the secret value: %s", raw)

	// An allowed write carries the decision columns and still only the name.
	makeRolePAT(t, pg, "admin-token", "admin")
	rec = doAs(t, s, http.MethodPost, "/api/v1/secrets", "admin-token", body)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	logs = auditRows(t, pg)
	require.Len(t, logs, 2)
	assert.Equal(t, "AWS_KEY", logs[0].Resource)
	assert.Equal(t, 204, logs[0].Status)
	assert.NotEmpty(t, logs[0].Permission)
	assert.NotEmpty(t, logs[0].GrantedBy)
	raw, err = json.Marshal(logs[0])
	require.NoError(t, err)
	assert.False(t, strings.Contains(string(raw), value), "audit row leaked the secret value: %s", raw)
}
