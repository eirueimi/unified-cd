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

func newTestTokenCmd(tr *captureTransport) (*cobra.Command, *strings.Builder) {
	cfg := Config{Server: "http://fake", Token: "tok"}
	cmd := newTokenCmdWithClient(func() (Config, error) { return cfg, nil }, &http.Client{Transport: tr})
	var out strings.Builder
	cmd.SetOut(&out)
	return cmd, &out
}

func TestTokenCreateSendsPermissions(t *testing.T) {
	tr := &captureTransport{responseFor: func(string) (int, []byte) {
		b, _ := json.Marshal(api.CreatePATResponse{ID: "1", Name: "ci", Token: "exc_x", Permissions: []string{"job.read", "run.trigger"}})
		return http.StatusOK, b
	}}
	cmd, out := newTestTokenCmd(tr)
	cmd.SetArgs([]string{"create", "ci", "--permission", "job.read", "--permission", "run.trigger"})
	require.NoError(t, cmd.Execute())
	require.Len(t, tr.records, 1)
	assert.Equal(t, http.MethodPost, tr.records[0].method)
	assert.Equal(t, "/api/v1/tokens", tr.records[0].path)
	assert.JSONEq(t, `{"name":"ci","permissions":["job.read","run.trigger"]}`, string(tr.records[0].body))
	assert.Contains(t, out.String(), "Permissions: job.read, run.trigger")
}

func TestTokenCreateServerErrorIsReturned(t *testing.T) {
	const msg = `cannot grant permission "secret.write": you do not hold it`
	tr := &captureTransport{responseFor: func(string) (int, []byte) {
		return http.StatusBadRequest, []byte(msg)
	}}
	cmd, _ := newTestTokenCmd(tr)
	cmd.SetArgs([]string{"create", "ci", "--permission", "secret.write"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.Execute()
	require.Error(t, err)
	assert.Equal(t, "server error: "+msg, err.Error())
}

func TestTokenCreateRoleOnlyOmitsPermissions(t *testing.T) {
	tr := &captureTransport{responseFor: func(string) (int, []byte) {
		b, _ := json.Marshal(api.CreatePATResponse{ID: "1", Name: "ci", Role: "viewer", Token: "exc_x"})
		return http.StatusOK, b
	}}
	cmd, out := newTestTokenCmd(tr)
	cmd.SetArgs([]string{"create", "ci", "--role", "viewer"})
	require.NoError(t, cmd.Execute())
	assert.JSONEq(t, `{"name":"ci","role":"viewer"}`, string(tr.records[0].body))
	assert.NotContains(t, out.String(), "Permissions:")
}

func TestTokenListAndDeleteMethodAndPath(t *testing.T) {
	tr := &captureTransport{responseFor: func(path string) (int, []byte) {
		if path == "/api/v1/tokens" {
			b, _ := json.Marshal([]api.PATMeta{{ID: "1", Name: "ci", Permissions: []string{"job.read"}, CreatedAt: time.Now()}})
			return http.StatusOK, b
		}
		return http.StatusNoContent, nil
	}}
	cmd, out := newTestTokenCmd(tr)
	cmd.SetArgs([]string{"list"})
	require.NoError(t, cmd.Execute())
	assert.Contains(t, out.String(), "job.read")

	cmd, _ = newTestTokenCmd(tr)
	cmd.SetArgs([]string{"delete", "abc"})
	require.NoError(t, cmd.Execute())

	require.Len(t, tr.records, 2)
	assert.Equal(t, http.MethodGet, tr.records[0].method)
	assert.Equal(t, "/api/v1/tokens", tr.records[0].path)
	assert.Equal(t, http.MethodDelete, tr.records[1].method)
	assert.Equal(t, "/api/v1/tokens/abc", tr.records[1].path)
	assert.Equal(t, "Bearer tok", tr.records[1].authorization)
}
