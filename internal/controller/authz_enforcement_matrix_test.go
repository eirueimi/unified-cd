package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// matrixPolicy gives: g-a developer on team-a; g-b developer on team-b;
// g-r job.read on team-a and developer on team-b (a caller who holds every
// action somewhere but can only read team-a).
const matrixPolicy = `
folders:
  team-a:
    groups:
      "g-a": developer
      "g-r": [job.read]
  team-b:
    groups:
      "g-b": developer
      "g-r": developer
`

var matrixJobs = []string{"team-a/x", "team-a/sub/x", "team-b/x", "team-ab/x", "x"}

type matrixFixture struct {
	runs map[string]string // job -> id of its terminal run
}

// seedMatrix (re)creates the jobs, one terminal run each, and the schedule
// "sch" -> team-a/x. Handlers under test may delete objects, so the table
// tests seed again before every request.
func seedMatrix(t *testing.T, st store.Store) matrixFixture {
	t.Helper()
	ctx := context.Background()
	f := matrixFixture{runs: map[string]string{}}
	for _, j := range matrixJobs {
		_, err := st.UpsertJob(ctx, j, "unified-cd/v1", []byte(`{"steps":[]}`))
		require.NoError(t, err)
		run, err := st.CreateRun(ctx, j, nil, []byte(`{"steps":[]}`), nil, nil, "", "")
		require.NoError(t, err)
		_, err = st.FinishRun(ctx, run.ID, api.RunSucceeded)
		require.NoError(t, err)
		f.runs[j] = run.ID
	}
	_, err := st.UpsertSchedule(ctx, "sch", "0 3 * * *", "team-a/x", nil)
	require.NoError(t, err)
	return f
}

type matrixRequest struct {
	method, path string
	body         []byte
}

// requestFor builds the request that exercises rule against job (and, for run
// routes, that job's run).
func requestFor(rule routeRule, job, runID string) matrixRequest {
	path := rule.Pattern
	runRoute := rule.Scope == scopeJobRunID
	repl := []string{"{stepIndex}", "0", "{agentId}", "x", "{name}", "x", "*", job}
	if runRoute {
		repl = append(repl, "{id}", runID, "{runID}", runID)
	} else {
		repl = append(repl, "{id}", "x")
	}
	if rule.Scope == scopeSchedulePath {
		repl = append(repl, "{name}", "sch")
	}
	path = strings.NewReplacer(repl...).Replace(path)
	if rule.Scope == scopeJobQuery {
		path += "?jobName=" + url.QueryEscape(job)
	}
	req := matrixRequest{method: rule.Method, path: path}
	if rule.Method == http.MethodPost || rule.Method == http.MethodPut {
		req.body = []byte(`{}`)
		if strings.Contains(rule.Pattern, "/approvals/") {
			req.body = []byte(`{"decision":"approve"}`)
		}
	}
	return req
}

// doTimed sends the request through the full router with a context deadline,
// so a streaming handler (the SSE route streams without checking that the run
// exists) cannot hang the test. A stream that started counts as admitted: its
// recorded status is 200.
func doTimed(t *testing.T, s *Server, req matrixRequest, token string) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r := httptest.NewRequestWithContext(ctx, req.method, req.path, strings.NewReader(string(req.body)))
	if req.body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", bearerPrefix+token)
	}
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, r)
	return rec
}

func humanRows() []routeRule {
	var rows []routeRule
	for _, r := range routeTable {
		if isHumanRow(r) {
			rows = append(rows, r)
		}
	}
	return rows
}

func rowName(r routeRule) string { return r.Method + " " + r.Pattern }

func isFolderRow(r routeRule) bool {
	switch r.Scope {
	case scopeJobPath, scopeJobQuery, scopeJobRunID:
		return true
	}
	return false
}

func permNames(ps []authz.Permission) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, string(p))
	}
	return out
}

func allExcept(excluded ...authz.Permission) []authz.Permission {
	var out []authz.Permission
outer:
	for _, p := range authz.All() {
		for _, e := range excluded {
			if p == e {
				continue outer
			}
		}
		out = append(out, p)
	}
	return out
}

// matrixServer builds the server used by the table tests: group "all" is a
// global admin, so an owner PAT with a permission-list ceiling holds exactly
// that list.
func matrixServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	return newAuthzTestServer(t, authz.Legacy{RoleMap: map[string]string{"all": "admin"}}, matrixPolicy)
}

func ceilingToken(t *testing.T, st store.Store, token string, perms []authz.Permission) string {
	t.Helper()
	mustOwnedPAT(t, st, token, "", permNames(perms), []string{"all"})
	return token
}

// hiddenAnswer is the body a hidden job or run gets on rule's route.
func hiddenAnswer(rule routeRule, runID string) string {
	if rule.Scope != scopeJobRunID {
		return store.JobNotFoundError().Error() + "\n"
	}
	rec := httptest.NewRecorder()
	missingRunAnswer(rule.Pattern, runID)(rec)
	return rec.Body.String()
}

// isHidden reports whether rec is the answer given for a job or run the caller
// cannot read. The log archive route answers an existing run that has no
// archive with the very same text, so an admitted request there cannot be told
// apart by its body.
func isHidden(rule routeRule, rec *httptest.ResponseRecorder, runID string) bool {
	return rec.Code == http.StatusNotFound && rec.Body.String() == hiddenAnswer(rule, runID)
}

func isAdmitted(rule routeRule, rec *httptest.ResponseRecorder, runID string) bool {
	if rec.Code == http.StatusForbidden {
		return false
	}
	if rule.Pattern == "/api/v1/runs/{id}/logs/archive" {
		return true
	}
	return !isHidden(rule, rec, runID)
}

func TestEveryHumanRouteRefusesCallerWithoutItsPermission(t *testing.T) {
	s, st := matrixServer(t)
	for i, rule := range humanRows() {
		t.Run(rowName(rule), func(t *testing.T) {
			excluded := []authz.Permission{rule.Perm}
			if rule.AltPerm != "" {
				excluded = append(excluded, rule.AltPerm)
			}
			token := ceilingToken(t, st, fmt.Sprintf("not-%s-%d", rule.Perm, i), allExcept(excluded...))
			f := seedMatrix(t, st)
			rec := doTimed(t, s, requestFor(rule, "team-a/x", f.runs["team-a/x"]), token)
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			assert.Equal(t, missingPerm(rule.Perm)+"\n", rec.Body.String())
		})
	}
}

func TestEveryHumanRouteAdmitsCallerWithItsPermission(t *testing.T) {
	s, st := matrixServer(t)
	for i, rule := range humanRows() {
		perms := []authz.Permission{rule.Perm}
		if rule.AltPerm != "" {
			perms = append(perms, rule.AltPerm)
		}
		for _, held := range perms {
			t.Run(rowName(rule)+" holding "+string(held), func(t *testing.T) {
				grant := []authz.Permission{held}
				if authz.FolderScopable(held) && held != authz.JobRead {
					grant = append(grant, authz.JobRead) // section 5.7 makes job.read a precondition
				}
				token := ceilingToken(t, st, fmt.Sprintf("only-%s-%d", held, i), grant)
				f := seedMatrix(t, st)
				rec := doTimed(t, s, requestFor(rule, "team-a/x", f.runs["team-a/x"]), token)
				assert.True(t, isAdmitted(rule, rec, f.runs["team-a/x"]), "%d: %q", rec.Code, rec.Body.String())
			})
		}
	}
}

func TestFolderGrantMatrix(t *testing.T) {
	s, st := matrixServer(t)
	devA := bearer("dev-a@example.com", "g-a")
	reader := bearer("reader@example.com", "g-r")

	for _, rule := range humanRows() {
		if !isFolderRow(rule) {
			continue
		}
		t.Run(rowName(rule), func(t *testing.T) {
			for _, job := range matrixJobs {
				f := seedMatrix(t, st)
				req := requestFor(rule, job, f.runs[job])
				rec := doTimed(t, s, req, devA)
				if job == "team-a/x" || job == "team-a/sub/x" {
					assert.True(t, isAdmitted(rule, rec, f.runs[job]), "%s: %d %q", job, rec.Code, rec.Body.String())
					continue
				}
				assert.True(t, isHidden(rule, rec, f.runs[job]), "%s: %d %q", job, rec.Code, rec.Body.String())
			}

			// A caller that can read team-a, and holds every action on team-b,
			// is told exactly what it is missing on team-a.
			if rule.Perm == authz.JobRead {
				return
			}
			f := seedMatrix(t, st)
			rec := doTimed(t, s, requestFor(rule, "team-a/x", f.runs["team-a/x"]), reader)
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			assert.Equal(t, missingPermOnJob(rule.Perm, "team-a/x")+"\n", rec.Body.String())
		})
	}
}

// normalizeID replaces id in body so two answers about different run IDs can
// be compared structurally.
func normalizeID(body, id string) string { return strings.ReplaceAll(body, id, "<id>") }

func TestAuthorize_HiddenRunIndistinguishableFromMissing(t *testing.T) {
	s, st := matrixServer(t)
	teamA := bearer("dev-a@example.com", "g-a")
	admin := ceilingToken(t, st, "admin-tok", authz.All())
	const missingID = "11111111-1111-1111-1111-111111111111"

	for _, rule := range humanRows() {
		if rule.Scope != scopeJobRunID || rule.Method != http.MethodGet || rule.Perm != authz.JobRead {
			continue
		}
		t.Run(rowName(rule), func(t *testing.T) {
			f := seedMatrix(t, st)
			hiddenID := f.runs["team-b/x"]

			ids := []string{hiddenID, missingID, "not-a-uuid"}
			var normalized []string
			for _, id := range ids {
				rec := doTimed(t, s, requestFor(rule, "", id), teamA)
				assert.Equal(t, http.StatusNotFound, rec.Code, "%s: %s", id, rec.Body.String())
				if rule.Pattern == "/api/v1/runs/{id}" {
					assert.Equal(t, store.RunNotFoundError(id).Error()+"\n", rec.Body.String())
				}
				normalized = append(normalized, normalizeID(rec.Body.String(), id))
			}
			assert.Equal(t, normalized[0], normalized[1])
			assert.Equal(t, normalized[0], normalized[2])

			// Whatever the real handler writes for a truly missing run is what a
			// hidden run answers. Handlers that do not look the run up (an empty
			// log is a valid answer) have no missing-run answer to compare with.
			adminMissing := doTimed(t, s, requestFor(rule, "", missingID), admin)
			if adminMissing.Code != http.StatusNotFound {
				t.Logf("handler answers %d for a missing run (no 404 to compare)", adminMissing.Code)
				return
			}
			hidden := doTimed(t, s, requestFor(rule, "", hiddenID), teamA)
			assert.Equal(t, adminMissing.Code, hidden.Code)
			assert.Equal(t, normalizeID(adminMissing.Body.String(), missingID), normalizeID(hidden.Body.String(), hiddenID))
		})
	}
}

func TestAuthorize_HiddenJobIndistinguishableFromMissing(t *testing.T) {
	s, st := matrixServer(t)
	teamA := bearer("dev-a@example.com", "g-a")
	admin := ceilingToken(t, st, "admin-tok", authz.All())
	seedMatrix(t, st)

	for _, path := range []string{"/api/v1/jobs/team-b/x", "/api/v1/jobs/team-b/x/yaml", "/api/v1/jobs/team-b/x/schedulability"} {
		t.Run(path, func(t *testing.T) {
			hidden := doTimed(t, s, matrixRequest{method: http.MethodGet, path: path}, teamA)
			missing := doTimed(t, s, matrixRequest{method: http.MethodGet, path: strings.Replace(path, "team-b/x", "team-b/nope", 1)}, admin)
			assert.Equal(t, http.StatusNotFound, missing.Code)
			assert.Equal(t, missing.Code, hidden.Code)
			assert.Equal(t, missing.Body.String(), hidden.Body.String())
		})
	}
}

func TestAuthorize_JobPathUsesHandlerNameDerivation(t *testing.T) {
	s, st := matrixServer(t)
	seedMatrix(t, st)
	reader := bearer("dev-a@example.com", "g-a")

	get := func(path string) *httptest.ResponseRecorder {
		return doTimed(t, s, matrixRequest{method: http.MethodGet, path: path}, reader)
	}
	rec := get("/api/v1/jobs/team-b/x/yaml")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	rec = get("/api/v1/jobs/team-b%2Fx")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	rec = get("/api/v1/jobs/team-a/x/schedulability")
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = get("/api/v1/jobs/team-a/x/yaml")
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = get("/api/v1/jobs/team-a/sub/x")
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doTimed(t, s, matrixRequest{method: http.MethodDelete, path: "/api/v1/jobs/team-b/x"}, reader)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	_, err := st.GetJob(context.Background(), "team-b/x")
	assert.NoError(t, err, "a hidden job must not be deleted")
}

func TestAuthorize_EmptyGrantSetMessages(t *testing.T) {
	s, st := newAuthzTestServer(t, authz.Legacy{}, teamAPolicy)
	// An owner whose claims match nothing: admitted with empty grants.
	mustOwnedPAT(t, st, "nobody", "", nil, []string{"unmapped"})
	for _, path := range []string{"/api/v1/jobs", "/api/v1/secrets"} {
		rec := doAs(t, s, http.MethodGet, path, "nobody", nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, path)
		assert.Equal(t, "forbidden: token owner has no permissions\n", rec.Body.String(), path)
	}

	// A session whose claims match nothing.
	addSession(t, st, "sess-nobody", "sub-n", "n@example.com", "", []string{"unmapped"})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "sess-nobody"})
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "forbidden: no permissions\n", rec.Body.String())
}

func TestAuthorize_GlobalOnlyMessage(t *testing.T) {
	s, st := matrixServer(t)
	_, err := st.CreatePAT(context.Background(), "dev", HashToken("dev-pat"), "developer", nil)
	require.NoError(t, err)
	rec := doAs(t, s, http.MethodPost, "/api/v1/secrets", "dev-pat", []byte(`{}`))
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "forbidden: missing permission \"secret.write\"\n", rec.Body.String())
}

func TestAuthorize_AgentBypassUnchanged(t *testing.T) {
	s, st := matrixServer(t)
	f := seedMatrix(t, st)
	token := issueAgentAccessForTest(t, st, "agent-a", nil, nil)
	runID := f.runs["team-a/x"]

	for _, path := range []string{
		"/api/v1/runs/" + runID,
		"/api/v1/runs/" + runID + "/outputs",
		"/api/v1/runs/" + runID + "/artifacts",
	} {
		rec := doTimed(t, s, matrixRequest{method: http.MethodGet, path: path}, token)
		assert.NotEqual(t, http.StatusForbidden, rec.Code, "%s: %s", path, rec.Body.String())
		assert.NotEqual(t, http.StatusUnauthorized, rec.Code, "%s: %s", path, rec.Body.String())
	}
	rec := doTimed(t, s, matrixRequest{method: http.MethodGet, path: "/api/v1/jobs"}, token)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAuthorize_ApprovalCheckedAgainstCurrentJobName(t *testing.T) {
	s, st := matrixServer(t)
	ctx := context.Background()
	_, err := st.UpsertJob(ctx, "x", "unified-cd/v1", []byte(`{"steps":[]}`))
	require.NoError(t, err)
	run, err := st.CreateRun(ctx, "x", nil, []byte(`{"steps":[]}`), nil, nil, "", "")
	require.NoError(t, err)
	require.NoError(t, st.RenameJob(ctx, "x", "team-a/x"))

	path := "/api/v1/runs/" + run.ID + "/approvals/0"
	body := []byte(`{"decision":"approve"}`)
	req := matrixRequest{method: http.MethodPost, path: path, body: body}

	rec := doTimed(t, s, req, bearer("dev-a@example.com", "g-a"))
	assert.NotEqual(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.NotEqual(t, store.RunNotFoundError(run.ID).Error()+"\n", rec.Body.String())

	rec = doTimed(t, s, req, bearer("dev-b@example.com", "g-b"))
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, store.RunNotFoundError(run.ID).Error()+"\n", rec.Body.String())
}

func TestAuthorize_AltPermAdmitsEitherPermission(t *testing.T) {
	s, st := matrixServer(t)
	for _, held := range []authz.Permission{authz.TokenCreate, authz.TokenManage} {
		token := ceilingToken(t, st, "alt-"+string(held), []authz.Permission{held})
		for _, req := range []matrixRequest{
			{method: http.MethodGet, path: "/api/v1/tokens"},
			{method: http.MethodDelete, path: "/api/v1/tokens/x"},
		} {
			rec := doTimed(t, s, req, token)
			assert.NotEqual(t, http.StatusForbidden, rec.Code, "%s holding %s: %s", req.path, held, rec.Body.String())
		}
	}
	// POST has no alternative.
	token := ceilingToken(t, st, "alt-only-manage", []authz.Permission{authz.TokenManage})
	rec := doTimed(t, s, matrixRequest{method: http.MethodPost, path: "/api/v1/tokens", body: []byte(`{}`)}, token)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestAuthorize_SchedulePath(t *testing.T) {
	s, st := matrixServer(t)
	seedMatrix(t, st)
	del := matrixRequest{method: http.MethodDelete, path: "/api/v1/schedules/sch"}

	// Developer on team-b only: the schedule targets team-a/x, which it cannot read.
	rec := doTimed(t, s, del, bearer("dev-b@example.com", "g-b"))
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, missingPermOnJob(authz.ScheduleConfigure, "team-a/x")+"\n", rec.Body.String())
	_, found, err := st.ScheduleJobName(context.Background(), "sch")
	require.NoError(t, err)
	assert.True(t, found, "a refused delete must leave the schedule")

	// Developer on team-a: allowed.
	rec = doTimed(t, s, del, bearer("dev-a@example.com", "g-a"))
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	// Deleting a schedule that no longer exists is idempotent for a folder caller.
	rec = doTimed(t, s, del, bearer("dev-a@example.com", "g-a"))
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

func TestDecisionSlot(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	recordDecision(r, authzDecision{Permission: authz.JobRead}) // no slot: no-op, must not panic

	r, slot := withDecisionSlot(r)
	recordDecision(r, authzDecision{Permission: authz.RunTrigger, Scope: "team-a/x", GrantedBy: "role:developer", Status: 0})
	assert.Equal(t, authzDecision{Permission: authz.RunTrigger, Scope: "team-a/x", GrantedBy: "role:developer"}, *slot)
}

func TestAuthorize_RecordsDecisions(t *testing.T) {
	s, st := matrixServer(t)
	f := seedMatrix(t, st)

	var slot *authzDecision
	capture := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r, slot = withDecisionSlot(r)
			next.ServeHTTP(w, r)
		})
	}
	router := chi.NewRouter()
	router.With(ServerAuth(s.store, s), capture, s.authorize).
		Post("/api/v1/runs/{id}/cancel", func(http.ResponseWriter, *http.Request) {})

	call := func(token string) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+f.runs["team-b/x"]+"/cancel", nil)
		req.Header.Set("Authorization", bearerPrefix+token)
		router.ServeHTTP(httptest.NewRecorder(), req)
	}
	call(bearer("dev-a@example.com", "g-a"))
	assert.Equal(t, authzDecision{Permission: authz.RunCancel, Scope: "team-b/x", Denied: true, Status: http.StatusNotFound}, *slot)

	call(bearer("dev-b@example.com", "g-b"))
	require.False(t, slot.Denied)
	assert.Equal(t, authz.RunCancel, slot.Permission)
	assert.Equal(t, "team-b/x", slot.Scope)
	assert.NotEmpty(t, slot.GrantedBy)
}
