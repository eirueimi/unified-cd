package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuditList_PrintsTable(t *testing.T) {
	occurred := time.Date(2026, 7, 5, 10, 30, 0, 0, time.UTC)
	list := []api.AuditLog{
		{ID: 2, OccurredAt: occurred, Actor: "bob", Method: "DELETE", Path: "/api/v1/jobs/foo", Action: "job.delete", Resource: "foo", Status: 204},
		{ID: 1, OccurredAt: occurred.Add(-time.Minute), Actor: "alice", Method: "POST", Path: "/api/v1/jobs", Action: "job.apply", Resource: "foo", Status: 200},
	}
	body, _ := json.Marshal(list)

	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			assert.Contains(t, path, "/api/v1/audit")
			return http.StatusOK, body
		},
	}
	cfg := Config{Server: "http://test", Token: "tok"}
	cmd := newAuditCmdWithClient(func() (Config, error) { return cfg, nil }, &http.Client{Transport: tr})
	cmd.SetArgs([]string{"list"})
	var out strings.Builder
	cmd.SetOut(&out)
	require.NoError(t, cmd.Execute())

	got := out.String()
	assert.Contains(t, got, "bob")
	assert.Contains(t, got, "job.delete")
	assert.Contains(t, got, "foo")
	assert.Contains(t, got, "204")
	assert.Contains(t, got, "alice")
	assert.Contains(t, got, "job.apply")
}

func TestAuditList_Empty(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			return http.StatusOK, []byte("[]")
		},
	}
	cfg := Config{Server: "http://test", Token: "tok"}
	cmd := newAuditCmdWithClient(func() (Config, error) { return cfg, nil }, &http.Client{Transport: tr})
	cmd.SetArgs([]string{"list"})
	var out strings.Builder
	cmd.SetOut(&out)
	require.NoError(t, cmd.Execute())
	assert.Contains(t, out.String(), "no audit log entries")
}

func TestAuditList_ServerError(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			return http.StatusForbidden, []byte("forbidden")
		},
	}
	cfg := Config{Server: "http://test", Token: "tok"}
	cmd := newAuditCmdWithClient(func() (Config, error) { return cfg, nil }, &http.Client{Transport: tr})
	cmd.SetArgs([]string{"list"})
	var out strings.Builder
	cmd.SetOut(&out)
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forbidden")
}

func TestAuditList_LimitFlag(t *testing.T) {
	var gotPath string
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			gotPath = path
			return http.StatusOK, []byte("[]")
		},
	}
	cfg := Config{Server: "http://test", Token: "tok"}
	cmd := newAuditCmdWithClient(func() (Config, error) { return cfg, nil }, &http.Client{Transport: tr})
	cmd.SetArgs([]string{"list", "--limit", "5"})
	var out strings.Builder
	cmd.SetOut(&out)
	require.NoError(t, cmd.Execute())
	assert.Contains(t, gotPath, "limit=5")
}
