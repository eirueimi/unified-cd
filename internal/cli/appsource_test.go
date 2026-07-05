package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/spf13/cobra"
)

func newTestAppSourceCmd(t *testing.T, tr *captureTransport, serverURL string) (*cobra.Command, *strings.Builder) {
	t.Helper()
	cfg := Config{Server: serverURL, Token: "tok"}
	cmd := newAppSourceCmdWithClient(func() (Config, error) { return cfg, nil }, &http.Client{Transport: tr})
	var out strings.Builder
	cmd.SetOut(&out)
	return cmd, &out
}

func TestAppSourceSync(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) { return http.StatusNoContent, nil },
	}
	cmd, out := newTestAppSourceCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"sync", "my-pipelines"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(tr.records) != 1 || tr.records[0].path != "/api/v1/appsources/my-pipelines/sync" {
		t.Fatalf("unexpected requests: %+v", tr.records)
	}
	if tr.records[0].method != http.MethodPost {
		t.Errorf("expected POST, got %s", tr.records[0].method)
	}
	if !strings.Contains(out.String(), "my-pipelines") {
		t.Errorf("unexpected output: %s", out.String())
	}
}

func TestAppSourceSync_ServerErrorPropagatesMessage(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) { return http.StatusInternalServerError, []byte("sync boom") },
	}
	cmd, _ := newTestAppSourceCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"sync", "my-pipelines"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "sync boom") {
		t.Fatalf("expected error containing 'sync boom', got: %v", err)
	}
	if len(tr.records) != 1 || tr.records[0].method != http.MethodPost {
		t.Fatalf("unexpected requests: %+v", tr.records)
	}
}

func TestAppSourceList(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			b, _ := json.Marshal([]api.AppSourceMeta{
				{Name: "my-pipelines", RepoURL: "https://github.com/acme/pipelines", TargetRevision: "main", LastCommit: "abc123"},
			})
			return http.StatusOK, b
		},
	}
	cmd, out := newTestAppSourceCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"list"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(tr.records) != 1 || tr.records[0].path != "/api/v1/appsources" {
		t.Fatalf("unexpected requests: %+v", tr.records)
	}
	if !strings.Contains(out.String(), "my-pipelines") {
		t.Errorf("unexpected output: %s", out.String())
	}
}

func TestAppSourceGet(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			b, _ := json.Marshal(api.AppSourceMeta{Name: "my-pipelines", RepoURL: "https://github.com/acme/pipelines", TargetRevision: "main"})
			return http.StatusOK, b
		},
	}
	cmd, out := newTestAppSourceCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"get", "my-pipelines"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(tr.records) != 1 || tr.records[0].path != "/api/v1/appsources/my-pipelines" {
		t.Fatalf("unexpected requests: %+v", tr.records)
	}
	if !strings.Contains(out.String(), "github.com/acme/pipelines") {
		t.Errorf("unexpected output: %s", out.String())
	}
}

func TestAppSourceDelete(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) { return http.StatusNoContent, nil },
	}
	cmd, out := newTestAppSourceCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"delete", "my-pipelines"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(tr.records) != 1 || tr.records[0].path != "/api/v1/appsources/my-pipelines" {
		t.Fatalf("unexpected requests: %+v", tr.records)
	}
	if tr.records[0].method != http.MethodDelete {
		t.Errorf("expected DELETE, got %s", tr.records[0].method)
	}
	if !strings.Contains(out.String(), "my-pipelines") {
		t.Errorf("unexpected output: %s", out.String())
	}
}
