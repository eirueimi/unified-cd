package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bodyPolicy: g-a is a developer on team-a, g-b a developer on team-b, g-ra
// can only read team-a, g-agent holds agent.read globally and reads team-a.
const bodyPolicy = `
global:
  groups:
    "g-agent": [agent.read]
folders:
  team-a:
    groups:
      "g-a": developer
      "g-ra": [job.read]
      "g-agent": [job.read]
  team-b:
    groups:
      "g-b": developer
`

func bodyServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	return newAuthzTestServer(t, authz.Legacy{}, bodyPolicy)
}

func bodyJobYAML(name, path string) string {
	ann := ""
	if path != "" {
		ann = fmt.Sprintf("\n  annotations:\n    path: %q", path)
	}
	return fmt.Sprintf("apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: %s%s\nspec:\n  steps:\n    - name: s\n      run: echo hi\n", name, ann)
}

func bodyPost(t *testing.T, s *Server, path, token string, v any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return doAs(t, s, http.MethodPost, path, token, b)
}

func bodySeedJob(t *testing.T, st store.Store, name string) {
	t.Helper()
	_, err := st.UpsertJob(context.Background(), name, "unified-cd/v1", []byte(`{"steps":[]}`))
	require.NoError(t, err)
}

func TestApplyJob_ChecksStoredQualifiedName(t *testing.T) {
	s, st := bodyServer(t)
	tok := bearer("a@example.com", "g-a")

	rec := bodyPost(t, s, "/api/v1/jobs", tok, api.ApplyJobRequest{YAML: bodyJobYAML("build", "team-a//sub")})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	// Whatever normalisation the stored name has, authorization ran on it.
	_, err := st.GetJob(context.Background(), "team-a//sub/build")
	require.NoError(t, err)

	rec = bodyPost(t, s, "/api/v1/jobs", tok, api.ApplyJobRequest{YAML: bodyJobYAML("build", "/team-b/")})
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), `forbidden: missing permission "job.configure" on job "team-b/build"`)

	rec = bodyPost(t, s, "/api/v1/jobs", tok, api.ApplyJobRequest{YAML: bodyJobYAML("build", "")})
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), `forbidden: missing permission "job.configure" on job "build"`)
}

func seedManagedTeamB(t *testing.T, st store.Store, spec string) {
	t.Helper()
	ctx := context.Background()
	_, err := st.UpsertAppSource(ctx, "pipelines", []byte(spec))
	require.NoError(t, err)
	require.NoError(t, st.UpdateAppSourceSyncState(ctx, "pipelines", "sha", time.Now(),
		[]store.ResourceRef{{Kind: "Job", Name: "team-b/x"}}))
}

const managedSpec = `{"repoURL":"https://example.com/secret-repo.git","targetRevision":"main","path":"jobs"}`

func TestApplyJob_PermissionBeforeManagedGuard(t *testing.T) {
	s, st := bodyServer(t)
	seedManagedTeamB(t, st, managedSpec)
	body := api.ApplyJobRequest{YAML: bodyJobYAML("x", "team-b")}

	rec := bodyPost(t, s, "/api/v1/jobs", bearer("a@example.com", "g-a"), body)
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.NotContains(t, rec.Body.String(), "pipelines")
	assert.NotContains(t, rec.Body.String(), "secret-repo")

	rec = bodyPost(t, s, "/api/v1/jobs", bearer("b@example.com", "g-b"), body)
	require.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), `managed by AppSource "pipelines"`)

	_, err := st.UpsertAppSource(context.Background(), "pipelines",
		[]byte(`{"repoURL":"https://example.com/secret-repo.git","targetRevision":"main","path":"jobs","syncPolicy":{"allowManualOverride":true}}`))
	require.NoError(t, err)
	rec = bodyPost(t, s, "/api/v1/jobs", bearer("b@example.com", "g-b"), body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestDeleteJob_ManagedHiddenIs404WithoutRepoURL(t *testing.T) {
	s, st := bodyServer(t)
	bodySeedJob(t, st, "team-b/x")
	seedManagedTeamB(t, st, managedSpec)

	rec := doAs(t, s, http.MethodDelete, "/api/v1/jobs/team-b/x", bearer("a@example.com", "g-a"), nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.NotContains(t, rec.Body.String(), "pipelines")
	assert.NotContains(t, rec.Body.String(), "secret-repo")
}

func TestTriggerRun_HiddenJobLooksMissing(t *testing.T) {
	s, st := bodyServer(t)
	bodySeedJob(t, st, "team-b/x")
	tok := bearer("a@example.com", "g-a")

	hidden := bodyPost(t, s, "/api/v1/runs", tok, api.TriggerRunRequest{JobName: "team-b/x"})
	missing := bodyPost(t, s, "/api/v1/runs", tok, api.TriggerRunRequest{JobName: "nope"})
	require.Equal(t, http.StatusNotFound, hidden.Code)
	require.Equal(t, http.StatusNotFound, missing.Code)
	assert.Equal(t, "job not found: team-b/x\n", hidden.Body.String())
	assert.Equal(t, "job not found: nope\n", missing.Body.String())

	// A reader who holds run.trigger nowhere is refused before any job lookup.
	bodySeedJob(t, st, "team-a/x")
	rec := bodyPost(t, s, "/api/v1/runs", bearer("r@example.com", "g-ra"), api.TriggerRunRequest{JobName: "team-a/x"})
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), `forbidden: missing permission "run.trigger"`)

	rec = bodyPost(t, s, "/api/v1/runs", tok, api.TriggerRunRequest{JobName: "team-a/x"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func bodyScheduleYAML(name, job string) string {
	return fmt.Sprintf("apiVersion: unified-cd/v1\nkind: Schedule\nmetadata:\n  name: %s\nspec:\n  cron: \"0 3 * * *\"\n  job: %s\n", name, job)
}

func TestSchedule_RetargetNeedsBothFolders(t *testing.T) {
	s, st := bodyServer(t)
	bodySeedJob(t, st, "team-a/x")
	bodySeedJob(t, st, "team-b/x")
	_, err := st.UpsertSchedule(context.Background(), "sch", "0 3 * * *", "team-b/x", nil)
	require.NoError(t, err)
	body := api.ApplyScheduleRequest{YAML: bodyScheduleYAML("sch", "team-a/x")}

	rec := bodyPost(t, s, "/api/v1/schedules", bearer("a@example.com", "g-a"), body)
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), `forbidden: missing permission "schedule.configure" on job "team-b/x"`)

	rec = bodyPost(t, s, "/api/v1/schedules", bearer("ab@example.com", "g-a", "g-b"), body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	sc, err := st.GetSchedule(context.Background(), "sch")
	require.NoError(t, err)
	assert.Equal(t, "team-a/x", sc.JobName)

	// A new schedule on a job the caller cannot read looks like a missing job.
	rec = bodyPost(t, s, "/api/v1/schedules", bearer("a@example.com", "g-a"), api.ApplyScheduleRequest{YAML: bodyScheduleYAML("other", "team-b/x")})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "job not found: team-b/x\n", rec.Body.String())
}

func TestSchedule_DeleteChecksCurrentTarget(t *testing.T) {
	s, st := bodyServer(t)
	bodySeedJob(t, st, "team-b/x")
	_, err := st.UpsertSchedule(context.Background(), "sch", "0 3 * * *", "team-b/x", nil)
	require.NoError(t, err)
	tok := bearer("a@example.com", "g-a")

	rec := doAs(t, s, http.MethodDelete, "/api/v1/schedules/sch", tok, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code) // a hidden target answers like a missing schedule
	_, err = st.GetSchedule(context.Background(), "sch")
	require.NoError(t, err, "a schedule the caller cannot see must not be deleted")

	// A caller who reads the target but cannot configure schedules is refused.
	bodySeedJob(t, st, "team-a/x")
	_, err = st.UpsertSchedule(context.Background(), "sch-a", "0 3 * * *", "team-a/x", nil)
	require.NoError(t, err)
	rec = doAs(t, s, http.MethodDelete, "/api/v1/schedules/sch-a", bearer("r@example.com", "g-ra"), nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	rec = doAs(t, s, http.MethodDelete, "/api/v1/schedules/sch", bearer("b@example.com", "g-b"), nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	_, err = st.GetSchedule(context.Background(), "sch")
	require.Error(t, err)

	rec = doAs(t, s, http.MethodDelete, "/api/v1/schedules/missing", tok, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// seedTwoFolders creates team-a/x, team-a/sub/x and team-b/x.
func bodySeedFolders(t *testing.T, st store.Store) {
	t.Helper()
	for _, j := range []string{"team-a/x", "team-a/sub/x", "team-b/x"} {
		bodySeedJob(t, st, j)
	}
}

func TestFilter_ListJobs(t *testing.T) {
	s, st := bodyServer(t)
	bodySeedFolders(t, st)
	list := func(token string) []string {
		rec := doAs(t, s, http.MethodGet, "/api/v1/jobs", token, nil)
		require.Equal(t, http.StatusOK, rec.Code)
		require.NotEqual(t, "null\n", rec.Body.String())
		var jobs []api.Job
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &jobs))
		names := []string{}
		for _, j := range jobs {
			names = append(names, j.Name)
		}
		return names
	}
	assert.ElementsMatch(t, []string{"team-a/x", "team-a/sub/x"}, list(bearer("r@example.com", "g-ra")))
	assert.Equal(t, []string{"team-b/x"}, list(bearer("b@example.com", "g-b")))
}

func bodyRunNames(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotEqual(t, "null\n", rec.Body.String())
	var runs []api.Run
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &runs))
	names := []string{}
	for _, r := range runs {
		names = append(names, r.JobName)
	}
	return names
}

func TestFilter_ActiveRuns(t *testing.T) {
	s, st := bodyServer(t)
	bodySeedFolders(t, st)
	for _, j := range []string{"team-a/x", "team-b/x"} {
		_, err := st.CreateRun(context.Background(), j, nil, []byte(`{"steps":[]}`), nil, nil, "", "")
		require.NoError(t, err)
	}
	assert.Equal(t, []string{"team-a/x"}, bodyRunNames(t, doAs(t, s, http.MethodGet, "/api/v1/runs/active", bearer("r@example.com", "g-ra"), nil)))
	assert.Equal(t, []string{"team-b/x"}, bodyRunNames(t, doAs(t, s, http.MethodGet, "/api/v1/runs/active", bearer("b@example.com", "g-b"), nil)))
}

func TestFilter_AgentRuns(t *testing.T) {
	s, st := bodyServer(t)
	ctx := context.Background()
	bodySeedFolders(t, st)
	for _, j := range []string{"team-a/x", "team-b/x"} {
		_, err := st.CreateRun(ctx, j, nil, []byte(`{"steps":[]}`), nil, nil, "", "")
		require.NoError(t, err)
		_, err = st.TransitionPendingToQueued(ctx, 10)
		require.NoError(t, err)
		claimed, err := st.ClaimNextRun(ctx, "agent-1", nil, nil)
		require.NoError(t, err)
		require.NotNil(t, claimed)
	}
	// g-agent holds agent.read globally but reads only team-a.
	got := bodyRunNames(t, doAs(t, s, http.MethodGet, "/api/v1/agents/agent-1/runs", bearer("ag@example.com", "g-agent"), nil))
	assert.Equal(t, []string{"team-a/x"}, got)

	// Agent principals see every row.
	req := withAgentPrincipal(httptest.NewRequest(http.MethodGet, "/", nil), AgentPrincipal{})
	all := s.filterRuns(req, []api.Run{{JobName: "team-a/x"}, {JobName: "team-b/x"}})
	assert.Len(t, all, 2)
}

func TestFilter_Schedules(t *testing.T) {
	s, st := bodyServer(t)
	ctx := context.Background()
	bodySeedFolders(t, st)
	_, err := st.UpsertSchedule(ctx, "sa", "0 3 * * *", "team-a/x", nil)
	require.NoError(t, err)
	_, err = st.UpsertSchedule(ctx, "sb", "0 3 * * *", "team-b/x", nil)
	require.NoError(t, err)

	rec := doAs(t, s, http.MethodGet, "/api/v1/schedules", bearer("r@example.com", "g-ra"), nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var list []api.ScheduleMeta
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list, 1)
	assert.Equal(t, "sa", list[0].Name)
}

func TestRunsByJobQuery_Hidden404(t *testing.T) {
	s, st := bodyServer(t)
	bodySeedFolders(t, st)
	rec := doAs(t, s, http.MethodGet, "/api/v1/runs?jobName=team-b/x", bearer("r@example.com", "g-ra"), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	rec = doAs(t, s, http.MethodGet, "/api/v1/runs?jobName=team-a/x", bearer("r@example.com", "g-ra"), nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// filterTests maps every row-filtering route to the test that covers it.
var filterTests = map[string]func(*testing.T){
	"GET /api/v1/jobs":                  TestFilter_ListJobs,
	"GET /api/v1/runs/active":           TestFilter_ActiveRuns,
	"GET /api/v1/agents/{agentId}/runs": TestFilter_AgentRuns,
	"GET /api/v1/schedules":             TestFilter_Schedules,
}

func TestSchedule_RetargetAuditScopeIsNewTarget(t *testing.T) {
	s, st := bodyServer(t)
	bodySeedJob(t, st, "team-a/x")
	bodySeedJob(t, st, "team-b/x")
	_, err := st.UpsertSchedule(context.Background(), "sch", "0 3 * * *", "team-b/x", nil)
	require.NoError(t, err)

	rec := bodyPost(t, s, "/api/v1/schedules", bearer("ab@example.com", "g-a", "g-b"),
		api.ApplyScheduleRequest{YAML: bodyScheduleYAML("sch", "team-a/x")})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	logs, err := st.ListAuditLogs(context.Background(), 10, 0)
	require.NoError(t, err)
	require.Len(t, logs, 1)
	assert.Equal(t, "team-a/x", logs[0].Scope, "the audit scope must name the new target")
}
