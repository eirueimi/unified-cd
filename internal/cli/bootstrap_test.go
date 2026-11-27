package cli

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runBootstrap(t *testing.T, tr *captureTransport, args ...string) (string, error) {
	t.Helper()
	cfg := Config{Server: "http://fake", Token: "should-not-be-sent"}
	cmd := newBootstrapCmdWithClient(func() (Config, error) { return cfg, nil }, &http.Client{Transport: tr})
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func bootstrapOK() *captureTransport {
	return &captureTransport{responseFor: func(string) (int, []byte) {
		b, _ := json.Marshal(api.CreatePATResponse{ID: "1", Name: "bootstrap", Token: "exc_pat"})
		return http.StatusCreated, b
	}}
}

func TestBootstrapCommand(t *testing.T) {
	tr := bootstrapOK()
	out, err := runBootstrap(t, tr, "--initial-token", "uci_abc", "--name", "first")
	require.NoError(t, err)
	assert.Equal(t, "exc_pat\n", out)
	require.Len(t, tr.records, 1)
	assert.Equal(t, http.MethodPost, tr.records[0].method)
	assert.Equal(t, "/api/v1/bootstrap", tr.records[0].path)
	assert.Empty(t, tr.records[0].authorization)
	assert.JSONEq(t, `{"initialToken":"uci_abc","name":"first"}`, string(tr.records[0].body))
}

func TestBootstrapCommandFileAndEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "initial-admin-token")
	require.NoError(t, os.WriteFile(path, []byte("uci_file\n"), 0o600))
	tr := bootstrapOK()
	_, err := runBootstrap(t, tr, "--initial-token-file", path)
	require.NoError(t, err)
	assert.JSONEq(t, `{"initialToken":"uci_file","name":""}`, string(tr.records[0].body))

	t.Setenv("UNIFIED_INITIAL_ADMIN_TOKEN", "uci_env")
	tr = bootstrapOK()
	_, err = runBootstrap(t, tr)
	require.NoError(t, err)
	assert.JSONEq(t, `{"initialToken":"uci_env","name":""}`, string(tr.records[0].body))
}

func TestBootstrapCommandServerError(t *testing.T) {
	tr := &captureTransport{responseFor: func(string) (int, []byte) {
		return http.StatusUnauthorized, []byte("invalid or already used initial token\n")
	}}
	out, err := runBootstrap(t, tr, "--initial-token", "uci_bad")
	require.EqualError(t, err, "server error: invalid or already used initial token")
	assert.Empty(t, out)
}

func TestBootstrapCommandRequiresValue(t *testing.T) {
	t.Setenv("UNIFIED_INITIAL_ADMIN_TOKEN", "")
	_, err := runBootstrap(t, bootstrapOK())
	require.Error(t, err)
}
