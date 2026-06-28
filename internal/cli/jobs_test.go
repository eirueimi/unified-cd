package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/unified-cd/unified-cd/internal/api"
)

func newTestJobsCmd(t *testing.T, tr *captureTransport, serverURL string) (*cobra.Command, *strings.Builder) {
	t.Helper()
	cfg := Config{Server: serverURL, Token: "tok"}
	cmd := newJobsCmdWithClient(func() (Config, error) { return cfg, nil }, &http.Client{Transport: tr})
	var out strings.Builder
	cmd.SetOut(&out)
	return cmd, &out
}

func TestJobsList_PrintsTabSeparatedRows(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			jobs := []api.Job{{Name: "hello"}, {Name: "world"}}
			b, _ := json.Marshal(jobs)
			return http.StatusOK, b
		},
	}
	cmd, out := newTestJobsCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"list"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), "hello") || !strings.Contains(out.String(), "world") {
		t.Errorf("unexpected output: %s", out.String())
	}
}

func TestJobsList_EmptyShowsMessage(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) { return http.StatusOK, []byte("[]") },
	}
	cmd, out := newTestJobsCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"list"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), "(no jobs)") {
		t.Errorf("expected empty message, got: %s", out.String())
	}
}

func TestJobsDelete_Success(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) { return http.StatusNoContent, nil },
	}
	cmd, out := newTestJobsCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"delete", "hello"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(tr.records) != 1 || tr.records[0].path != "/api/v1/jobs/hello" {
		t.Fatalf("unexpected requests: %+v", tr.records)
	}
	if !strings.Contains(out.String(), "hello") {
		t.Errorf("unexpected output: %s", out.String())
	}
}

func TestJobsDelete_ServerError(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) { return http.StatusInternalServerError, []byte("boom") },
	}
	cmd, _ := newTestJobsCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"delete", "hello"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected error containing 'boom', got: %v", err)
	}
}
