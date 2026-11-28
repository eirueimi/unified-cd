package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestAccountCmd(tr *captureTransport, serviceAlias bool) (*cobra.Command, *strings.Builder) {
	cfg := Config{Server: "http://fake", Token: "tok"}
	resolve := func() (Config, error) { return cfg, nil }
	client := &http.Client{Transport: tr}
	var cmd *cobra.Command
	if serviceAlias {
		cmd = newServiceAccountCmdWithClient(resolve, client)
	} else {
		cmd = newAccountCmdWithClient(resolve, client)
	}
	var out strings.Builder
	cmd.SetOut(&out)
	return cmd, &out
}

func accountsResponse(string) (int, []byte) {
	seen := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	b, _ := json.Marshal([]api.Account{
		{ID: "a1", Kind: "human", Name: "h@example.com", Email: "h@example.com", Disabled: true, LastSeenAt: &seen, PATCount: 2},
		{ID: "a2", Kind: "service", Name: "ci"},
	})
	return http.StatusOK, b
}

func TestAccountListPrintsColumns(t *testing.T) {
	tr := &captureTransport{responseFor: accountsResponse}
	cmd, out := newTestAccountCmd(tr, false)
	cmd.SetArgs([]string{"list", "--kind", "human"})
	require.NoError(t, cmd.Execute())
	require.Len(t, tr.records, 1)
	assert.Equal(t, http.MethodGet, tr.records[0].method)
	assert.Equal(t, "/api/v1/accounts?kind=human", tr.records[0].path)
	assert.Equal(t, "Bearer tok", tr.records[0].authorization)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	require.Len(t, lines, 3)
	assert.Equal(t, []string{"ID", "KIND", "NAME", "EMAIL", "DISABLED", "LAST", "SEEN"}, strings.Fields(lines[0]))
	assert.Contains(t, lines[1], "a1")
	assert.Contains(t, lines[1], "true")
	assert.Contains(t, lines[1], "2026-11-20")
	assert.Contains(t, lines[2], "ci")
	assert.Contains(t, lines[2], "false")
}

func TestAccountListRejectsBadKind(t *testing.T) {
	tr := &captureTransport{responseFor: accountsResponse}
	cmd, _ := newTestAccountCmd(tr, false)
	cmd.SetArgs([]string{"list", "--kind", "robot"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	require.Error(t, cmd.Execute())
	assert.Empty(t, tr.records)
}

func TestAccountDisableEnable(t *testing.T) {
	tr := &captureTransport{responseFor: func(string) (int, []byte) { return http.StatusNoContent, nil }}
	cmd, out := newTestAccountCmd(tr, false)
	cmd.SetArgs([]string{"disable", "a1"})
	require.NoError(t, cmd.Execute())
	cmd, _ = newTestAccountCmd(tr, false)
	cmd.SetArgs([]string{"enable", "a1"})
	require.NoError(t, cmd.Execute())
	require.Len(t, tr.records, 2)
	assert.Equal(t, http.MethodPost, tr.records[0].method)
	assert.Equal(t, "/api/v1/accounts/a1/disable", tr.records[0].path)
	assert.Equal(t, "/api/v1/accounts/a1/enable", tr.records[1].path)
	assert.Contains(t, out.String(), "disabled")
}

func TestAccountDisableServerError(t *testing.T) {
	const msg = "service accounts are enabled and disabled in serviceAccounts"
	tr := &captureTransport{responseFor: func(string) (int, []byte) { return http.StatusBadRequest, []byte(msg) }}
	cmd, _ := newTestAccountCmd(tr, false)
	cmd.SetArgs([]string{"disable", "a2"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.Execute()
	require.Error(t, err)
	assert.Equal(t, "server error: "+msg, err.Error())
}

func TestServiceAccountListIsAKindServiceAlias(t *testing.T) {
	tr := &captureTransport{responseFor: accountsResponse}
	cmd, out := newTestAccountCmd(tr, true)
	cmd.SetArgs([]string{"list"})
	require.NoError(t, cmd.Execute())
	require.Len(t, tr.records, 1)
	assert.Equal(t, "/api/v1/accounts?kind=service", tr.records[0].path)
	assert.Contains(t, out.String(), "KIND")
}

func TestTokenCreateAccountFlag(t *testing.T) {
	tr := &captureTransport{responseFor: func(string) (int, []byte) {
		b, _ := json.Marshal(api.CreatePATResponse{ID: "1", Name: "ci-token", Token: "exc_x", Role: "viewer"})
		return http.StatusOK, b
	}}
	cmd, _ := newTestTokenCmd(tr)
	cmd.SetArgs([]string{"create", "ci-token", "--account", "ci"})
	require.NoError(t, cmd.Execute())
	assert.JSONEq(t, `{"name":"ci-token","account":"ci"}`, string(tr.records[0].body))
}

func TestTokenListShowsAccountAndExpiry(t *testing.T) {
	exp := time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)
	tr := &captureTransport{responseFor: func(string) (int, []byte) {
		b, _ := json.Marshal([]api.PATMeta{
			{ID: "1", Name: "ci", Account: "sa:ci", Permissions: []string{"job.read"}, CreatedAt: time.Now(), ExpiresAt: &exp},
			{ID: "2", Name: "forever", Account: "h@example.com", Role: "viewer", CreatedAt: time.Now()},
		})
		return http.StatusOK, b
	}}
	cmd, out := newTestTokenCmd(tr)
	cmd.SetArgs([]string{"list"})
	require.NoError(t, cmd.Execute())
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	require.Len(t, lines, 3)
	header := strings.Fields(lines[0])
	assert.Contains(t, header, "ACCOUNT")
	assert.Contains(t, header, "EXPIRES")
	assert.Contains(t, lines[1], "sa:ci")
	assert.Contains(t, lines[1], "2027-02-01")
	assert.Contains(t, lines[2], "h@example.com")
	assert.Contains(t, lines[2], "never")
}
