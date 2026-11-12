package controller

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const releaseTokenSpec = `steps:
  - name: s
    run: echo {{ .Secrets.gh-app-release }}
`

func releaseJobAccess() *api.GitJobAccess {
	return &api.GitJobAccess{
		Folders:      []string{"team-a"},
		Repositories: []string{"acme/app"},
		Permissions:  map[string]string{"contents": "write", "statuses": "write"},
	}
}

// fetchedSecrets decodes a 200 secrets-fetch response.
func fetchedSecrets(t *testing.T, body []byte) map[string]string {
	t.Helper()
	var resp api.AgentFetchSecretsResponse
	require.NoError(t, json.Unmarshal(body, &resp))
	return resp.Secrets
}

// tokenIssueAudits returns the gitcredential.token_issue audit rows, newest first.
func tokenIssueAudits(t *testing.T, srv *Server) []api.AuditLog {
	t.Helper()
	rows, err := srv.store.ListAuditLogs(t.Context(), 100, 0)
	require.NoError(t, err)
	var out []api.AuditLog
	for _, r := range rows {
		if r.Action == "gitcredential.token_issue" {
			out = append(out, r)
		}
	}
	return out
}

func auditDetails(t *testing.T, row api.AuditLog) map[string]any {
	t.Helper()
	var d map[string]any
	require.NoError(t, json.Unmarshal(row.Details, &d))
	return d
}

func TestAgentSecretsFetch_GitHubAppJobToken(t *testing.T) {
	srv, _, agentID, runID := newSecretsFetchFixtureForJob(t, "team-a/build", releaseTokenSpec)
	env := fetchGitHub(t, srv)
	putJobAppCredential(t, srv, "gh-app-release", releaseJobAccess())

	rr := postFetchSecrets(t, srv, agentID, runID, []string{"gh-app-release"})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, "ghs_1", fetchedSecrets(t, rr.Body.Bytes())["gh-app-release"])

	reqs := env.fake.AccessTokenRequests()
	require.Len(t, reqs, 1)
	assert.Equal(t, []string{"app"}, reqs[0].Repositories)
	assert.Equal(t, map[string]string{"contents": "write", "statuses": "write"}, reqs[0].Permissions)

	rows := tokenIssueAudits(t, srv)
	require.Len(t, rows, 1)
	row := rows[0]
	assert.Equal(t, "agent:"+agentID, row.ActorID)
	assert.Equal(t, "gh-app-release", row.Resource)
	assert.Equal(t, http.StatusOK, row.Status)
	d := auditDetails(t, row)
	assert.Equal(t, runID, d["runId"])
	assert.Equal(t, "team-a/build", d["jobName"])
	assert.Equal(t, []any{"acme/app"}, d["repositories"])
	assert.Equal(t, map[string]any{"contents": "write", "statuses": "write"}, d["permissions"])
	assert.Equal(t, env.clock().Add(time.Hour).Format(time.RFC3339), d["expiresAt"])
	assert.NotContains(t, d, "reason")
	raw, err := json.Marshal(row)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "ghs_1")

	assert.Equal(t, 1.0, testutil.ToFloat64(env.metrics.GitCredentialTokenCounterForTest("gh-app-release", "job", "ok")))
}

func TestAgentSecretsFetch_ParamSelectedCredentialName(t *testing.T) {
	srv, pg, _, _ := newSecretsFetchFixtureForJob(t, "team-a/build", releaseTokenSpec)
	putJobAppCredential(t, srv, "gh-app-release", releaseJobAccess())

	runID := newClaimedRunForJob(t, pg, "param-agent", "team-a/deploy", `params:
  inputs:
    - name: token_secret
      type: string
steps:
  - name: deploy
    env:
      TOKEN: '{{ index .Secrets .Params.token_secret }}'
    run: "true"
`, map[string]string{"token_secret": "gh-app-release"})

	rr := postFetchSecrets(t, srv, "param-agent", runID, []string{"gh-app-release"})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, "ghs_1", fetchedSecrets(t, rr.Body.Bytes())["gh-app-release"])
}

func TestAgentSecretsFetch_GitHubAppFolderDenied(t *testing.T) {
	srv, _, agentID, runID := newSecretsFetchFixtureForJob(t, "team-b/x", releaseTokenSpec)
	env := fetchGitHub(t, srv)
	putJobAppCredential(t, srv, "gh-app-release", releaseJobAccess())

	rr := postFetchSecrets(t, srv, agentID, runID, []string{"gh-app-release"})
	require.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())
	assert.Equal(t, `gitcredential "gh-app-release": job "team-b/x" is not covered by jobAccess.folders`, strings.TrimSpace(rr.Body.String()))
	assert.Empty(t, env.fake.Paths(), "a denied hand-out never calls GitHub")

	rows := tokenIssueAudits(t, srv)
	require.Len(t, rows, 1)
	assert.Equal(t, http.StatusForbidden, rows[0].Status)
	assert.Equal(t, "agent:"+agentID, rows[0].ActorID)
	d := auditDetails(t, rows[0])
	assert.Equal(t, "folder", d["reason"])
	assert.Equal(t, "team-b/x", d["jobName"])
	assert.NotContains(t, d, "expiresAt")
	assert.Equal(t, 1.0, testutil.ToFloat64(env.metrics.GitCredentialTokenCounterForTest("gh-app-release", "job", "denied")))
}

func TestAgentSecretsFetch_RootLevelJobNeedsStar(t *testing.T) {
	srv, _, agentID, runID := newSecretsFetchFixtureForJob(t, "build", releaseTokenSpec)
	putJobAppCredential(t, srv, "gh-app-release", releaseJobAccess())

	rr := postFetchSecrets(t, srv, agentID, runID, []string{"gh-app-release"})
	require.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())

	ja := releaseJobAccess()
	ja.Folders = []string{"*"}
	putJobAppCredential(t, srv, "gh-app-release", ja)
	rr = postFetchSecrets(t, srv, agentID, runID, []string{"gh-app-release"})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, "ghs_1", fetchedSecrets(t, rr.Body.Bytes())["gh-app-release"])
}

func TestAgentSecretsFetch_CredentialAsOfFetchTime(t *testing.T) {
	srv, _, agentID, runID := newSecretsFetchFixtureForJob(t, "team-a/build", releaseTokenSpec)
	putJobAppCredential(t, srv, "gh-app-release", releaseJobAccess())

	// The run was created while team-a was allowed; the folder list changes before the fetch.
	ja := releaseJobAccess()
	ja.Folders = []string{"other"}
	putJobAppCredential(t, srv, "gh-app-release", ja)

	rr := postFetchSecrets(t, srv, agentID, runID, []string{"gh-app-release"})
	require.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())
	assert.Empty(t, fetchGitHub(t, srv).fake.Paths())
}

func TestAgentSecretsFetch_NarrowedJobAccessGetsNewToken(t *testing.T) {
	srv, pg, agentID, runID := newSecretsFetchFixtureForJob(t, "team-a/build", releaseTokenSpec)
	env := fetchGitHub(t, srv)
	ja := releaseJobAccess()
	ja.Permissions = map[string]string{"contents": "write"}
	putJobAppCredential(t, srv, "gh-app-release", ja)

	rr := postFetchSecrets(t, srv, agentID, runID, []string{"gh-app-release"})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, "ghs_1", fetchedSecrets(t, rr.Body.Bytes())["gh-app-release"])

	ja.Permissions = map[string]string{"contents": "read"}
	putJobAppCredential(t, srv, "gh-app-release", ja)
	run2 := newClaimedRunForJob(t, pg, "agent-2", "team-a/build", releaseTokenSpec, nil)
	rr = postFetchSecrets(t, srv, "agent-2", run2, []string{"gh-app-release"})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, "ghs_2", fetchedSecrets(t, rr.Body.Bytes())["gh-app-release"])

	reqs := env.fake.AccessTokenRequests()
	require.Len(t, reqs, 2)
	assert.Equal(t, map[string]string{"contents": "read"}, reqs[1].Permissions)
}

func TestAgentSecretsFetch_SharedTokenAcrossRuns(t *testing.T) {
	srv, pg, agentID, run1 := newSecretsFetchFixtureForJob(t, "team-a/build", releaseTokenSpec)
	env := fetchGitHub(t, srv)
	putJobAppCredential(t, srv, "gh-app-release", releaseJobAccess())

	rr := postFetchSecrets(t, srv, agentID, run1, []string{"gh-app-release"})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, "ghs_1", fetchedSecrets(t, rr.Body.Bytes())["gh-app-release"])

	run2 := newClaimedRunForJob(t, pg, "agent-2", "team-a/build", releaseTokenSpec, nil)
	env.advance(5 * time.Minute)
	rr = postFetchSecrets(t, srv, "agent-2", run2, []string{"gh-app-release"})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, "ghs_1", fetchedSecrets(t, rr.Body.Bytes())["gh-app-release"])
	assert.Len(t, env.fake.AccessTokenRequests(), 1, "the second run reuses the cached token")

	// 11 minutes after issue, 49 minutes are left: less than a job needs.
	env.advance(6 * time.Minute)
	rr = postFetchSecrets(t, srv, "agent-2", run2, []string{"gh-app-release"})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, "ghs_2", fetchedSecrets(t, rr.Body.Bytes())["gh-app-release"])
	assert.Len(t, env.fake.AccessTokenRequests(), 2)
}

func TestAgentSecretsFetch_GitHubAppWithoutJobAccessNeverHandedOut(t *testing.T) {
	srv, _, agentID, runID := newSecretsFetchFixtureForJob(t, "team-a/build", `steps:
  - name: s
    run: echo {{ .Secrets.gh-sync }}
`)
	putJobAppCredential(t, srv, "gh-sync", nil)

	rr := postFetchSecrets(t, srv, agentID, runID, []string{"gh-sync"})
	require.Equal(t, http.StatusNotFound, rr.Code, rr.Body.String())
	assert.Equal(t, "secret gh-sync not found", strings.TrimSpace(rr.Body.String()))
	assert.Empty(t, fetchGitHub(t, srv).fake.Paths())
	assert.Empty(t, tokenIssueAudits(t, srv))
}

func TestAgentSecretsFetch_PrivateKeyNeverDelivered(t *testing.T) {
	srv, _, agentID, runID := newSecretsFetchFixtureForJob(t, "team-a/build", `steps:
  - name: s
    run: echo {{ .Secrets.github-app-key }}
`)
	putJobAppCredential(t, srv, "gh-app-release", releaseJobAccess())

	rr := postFetchSecrets(t, srv, agentID, runID, []string{"github-app-key"})
	require.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())
	assert.Equal(t, `secret "github-app-key" is the private key of GitHub App credential "gh-app-release" and is never delivered to jobs`, strings.TrimSpace(rr.Body.String()))
	assert.NotContains(t, rr.Body.String(), "BEGIN")
	assert.Empty(t, fetchGitHub(t, srv).fake.Paths())
}

func TestAgentSecretsFetch_GitHubIssueFailure(t *testing.T) {
	srv, _, agentID, runID := newSecretsFetchFixtureForJob(t, "team-a/build", releaseTokenSpec)
	env := fetchGitHub(t, srv)
	env.fake.IssueStatus = http.StatusInternalServerError
	env.fake.IssueMessage = "boom"
	putJobAppCredential(t, srv, "gh-app-release", releaseJobAccess())

	rr := postFetchSecrets(t, srv, agentID, runID, []string{"gh-app-release"})
	require.Equal(t, http.StatusBadGateway, rr.Code, rr.Body.String())
	body := rr.Body.String()
	assert.True(t, strings.HasPrefix(body, `gitcredential "gh-app-release": `), body)
	assert.Contains(t, body, "boom")
	jwt := env.fake.LastJWT()
	require.NotEmpty(t, jwt)
	assert.NotContains(t, body, jwt)
	assert.NotContains(t, body, "BEGIN")

	rows := tokenIssueAudits(t, srv)
	require.Len(t, rows, 1)
	assert.Equal(t, http.StatusBadGateway, rows[0].Status)
	d := auditDetails(t, rows[0])
	assert.Equal(t, "issue", d["reason"])
	assert.NotContains(t, d, "expiresAt")
	raw, err := json.Marshal(rows[0])
	require.NoError(t, err)
	assert.NotContains(t, string(raw), jwt)
	assert.Equal(t, 1.0, testutil.ToFloat64(env.metrics.GitCredentialTokenCounterForTest("gh-app-release", "job", "error")))
}

func TestAgentSecretsFetch_MixedSecretAndToken(t *testing.T) {
	srv, _, agentID, runID := newSecretsFetchFixtureForJob(t, "team-a/build", `steps:
  - name: s
    run: echo {{ .Secrets.NEEDED }} {{ .Secrets.gh-app-release }}
`)
	mustSetSecret(t, srv, "NEEDED", "stored-value")
	putJobAppCredential(t, srv, "gh-app-release", releaseJobAccess())

	rr := postFetchSecrets(t, srv, agentID, runID, []string{"NEEDED", "gh-app-release"})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	got := fetchedSecrets(t, rr.Body.Bytes())
	assert.Equal(t, map[string]string{"NEEDED": "stored-value", "gh-app-release": "ghs_1"}, got)
	assert.Len(t, tokenIssueAudits(t, srv), 1, "only the token is audited, not the stored secret")
}
