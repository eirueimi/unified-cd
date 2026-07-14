package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunReplayCmd_PrintsNewRunID(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/replay") {
			gotPath = r.URL.Path
			fmt.Fprint(w, `{"id":"new-run-123","status":"Pending"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	cfg := Config{Server: srv.URL, Token: "tok"}
	cmd := newRunReplayCmd(func() (Config, error) { return cfg, nil }, srv.Client())
	cmd.SetArgs([]string{"orig-run"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	require.NoError(t, cmd.Execute())
	assert.Equal(t, "new-run-123\n", out.String())
	assert.Equal(t, "/api/v1/runs/orig-run/replay", gotPath)
}

func TestRunReplayCmd_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "run not found", http.StatusNotFound)
	}))
	defer srv.Close()
	cfg := Config{Server: srv.URL, Token: "tok"}
	cmd := newRunReplayCmd(func() (Config, error) { return cfg, nil }, srv.Client())
	cmd.SetArgs([]string{"missing"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "run not found")
}
