package cli

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestAgentLifecycleCmd(t *testing.T, tr *captureTransport) (*cobra.Command, *strings.Builder) {
	t.Helper()
	cfg := Config{Server: "http://fake", Token: "admin-token"}
	cmd := newAgentCmdWithClient(func() (Config, error) { return cfg, nil }, &http.Client{Transport: tr})
	var out strings.Builder
	cmd.SetOut(&out)
	return cmd, &out
}

func TestAgentEnrollmentCreateDisplaysTokenOnce(t *testing.T) {
	const token = "uce_550e8400-e29b-41d4-a716-446655440000_abcdefghijklmnopqrstuvwxyz"
	tr := &captureTransport{responseFor: func(path string) (int, []byte) {
		b, _ := json.Marshal(api.CreateAgentEnrollmentResponse{ID: "enroll-1", AgentID: "vm-agent-01", Token: token, ExpiresAt: time.Now().Add(10 * time.Minute)})
		return http.StatusCreated, b
	}}
	cmd, out := newTestAgentLifecycleCmd(t, tr)
	cmd.SetArgs([]string{"enrollment", "create", "--agent-id", "vm-agent-01", "--label", "kind:linux", "--capability", "container"})
	require.NoError(t, cmd.Execute())
	require.Len(t, tr.records, 1)
	assert.Equal(t, http.MethodPost, tr.records[0].method)
	assert.Equal(t, "/api/v1/agent-enrollments", tr.records[0].path)
	assert.Equal(t, "Bearer admin-token", tr.records[0].authorization)
	assert.NotContains(t, tr.records[0].path, token)
	assert.Equal(t, 1, strings.Count(out.String(), token))
	var request api.CreateAgentEnrollmentRequest
	require.NoError(t, json.Unmarshal(tr.records[0].body, &request))
	assert.Equal(t, "vm-agent-01", request.AgentID)
	assert.Equal(t, "10m", request.ExpiresIn)
	assert.Equal(t, []string{"kind:linux"}, request.Labels)
	assert.Equal(t, []string{"container"}, request.Capabilities)
}

func TestAgentEnrollmentCreateOutputFileIsExclusiveAndPrivate(t *testing.T) {
	const token = "uce_550e8400-e29b-41d4-a716-446655440000_abcdefghijklmnopqrstuvwxyz"
	tr := &captureTransport{responseFor: func(path string) (int, []byte) {
		b, _ := json.Marshal(api.CreateAgentEnrollmentResponse{ID: "enroll-1", AgentID: "vm-agent-01", Token: token})
		return http.StatusCreated, b
	}}
	path := filepath.Join(t.TempDir(), "enrollment-token")
	cmd, out := newTestAgentLifecycleCmd(t, tr)
	cmd.SetArgs([]string{"enrollment", "create", "--agent-id", "vm-agent-01", "--output-file", path})
	require.NoError(t, cmd.Execute())
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, token+"\n", string(got))
	assert.NotContains(t, out.String(), token)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	cmd, _ = newTestAgentLifecycleCmd(t, tr)
	cmd.SetArgs([]string{"enrollment", "create", "--agent-id", "vm-agent-02", "--output-file", path})
	require.Error(t, cmd.Execute())
}

func TestAgentEnrollmentListAndRevokeDoNotExposeSecrets(t *testing.T) {
	tr := &captureTransport{responseFor: func(path string) (int, []byte) {
		switch path {
		case "/api/v1/agent-enrollments":
			b, _ := json.Marshal([]api.AgentEnrollmentMeta{{ID: "enroll-1", AgentID: "vm-agent-01", CreatedAt: time.Now()}})
			return http.StatusOK, b
		case "/api/v1/agent-enrollments/enroll-1":
			return http.StatusNoContent, nil
		default:
			return http.StatusNotFound, []byte("not found")
		}
	}}
	cmd, out := newTestAgentLifecycleCmd(t, tr)
	cmd.SetArgs([]string{"enrollment", "list"})
	require.NoError(t, cmd.Execute())
	assert.Contains(t, out.String(), "vm-agent-01")
	assert.NotContains(t, out.String(), "uce_")
	assert.NotContains(t, out.String(), "hash")

	cmd, _ = newTestAgentLifecycleCmd(t, tr)
	cmd.SetArgs([]string{"enrollment", "revoke", "enroll-1"})
	require.NoError(t, cmd.Execute())
	require.Len(t, tr.records, 2)
	assert.Equal(t, http.MethodDelete, tr.records[1].method)
	assert.Equal(t, "/api/v1/agent-enrollments/enroll-1", tr.records[1].path)
	assert.Equal(t, "Bearer admin-token", tr.records[1].authorization)
}

func TestAgentIdentityCommandsUseAdminAPI(t *testing.T) {
	tr := &captureTransport{responseFor: func(path string) (int, []byte) {
		if path == "/api/v1/agent-identities/vm-agent-01" {
			b, _ := json.Marshal(api.AgentIdentityMeta{ID: "identity-1", AgentID: "vm-agent-01", Status: "active", CreatedAt: time.Now()})
			return http.StatusOK, b
		}
		return http.StatusNoContent, nil
	}}
	for _, args := range [][]string{
		{"identity", "get", "vm-agent-01"},
		{"identity", "enable", "vm-agent-01"},
		{"identity", "disable", "vm-agent-01"},
		{"identity", "revoke-credentials", "vm-agent-01"},
	} {
		cmd, _ := newTestAgentLifecycleCmd(t, tr)
		cmd.SetArgs(args)
		require.NoError(t, cmd.Execute(), strings.Join(args, " "))
	}
	require.Len(t, tr.records, 4)
	assert.Equal(t, http.MethodGet, tr.records[0].method)
	assert.Equal(t, "/api/v1/agent-identities/vm-agent-01", tr.records[0].path)
	assert.Equal(t, http.MethodPost, tr.records[1].method)
	assert.Equal(t, "/api/v1/agent-identities/vm-agent-01/enable", tr.records[1].path)
	assert.Equal(t, "/api/v1/agent-identities/vm-agent-01/disable", tr.records[2].path)
	assert.Equal(t, "/api/v1/agent-identities/vm-agent-01/credentials/revoke", tr.records[3].path)
	for _, record := range tr.records {
		assert.Equal(t, "Bearer admin-token", record.authorization)
	}
}
