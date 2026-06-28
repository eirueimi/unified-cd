package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/unified-cd/unified-cd/internal/api"
)

func newTestRunCmd(t *testing.T, tr *captureTransport, serverURL string) (*cobra.Command, *strings.Builder) {
	t.Helper()
	cfg := Config{Server: serverURL, Token: "tok"}
	cmd := newRunCmdWithClient(func() (Config, error) { return cfg, nil }, &http.Client{Transport: tr})
	var out strings.Builder
	cmd.SetOut(&out)
	return cmd, &out
}

func TestRunTrigger_PrintsRunID(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			b, _ := json.Marshal(api.Run{ID: "run-123", JobName: "hello"})
			return http.StatusOK, b
		},
	}
	cmd, out := newTestRunCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"trigger", "hello"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), "run-123") {
		t.Errorf("unexpected output: %s", out.String())
	}
	if tr.records[0].path != "/api/v1/runs" {
		t.Errorf("unexpected path: %s", tr.records[0].path)
	}
}

func TestRunList_RequiresJobFlag(t *testing.T) {
	tr := &captureTransport{responseFor: func(path string) (int, []byte) { return http.StatusOK, []byte("[]") }}
	cmd, _ := newTestRunCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"list"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when --job is omitted")
	}
}

func TestRunList_PrintsTabSeparatedRows(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			runs := []api.Run{
				{ID: "run-1", JobName: "hello", Status: api.RunSucceeded, CreatedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), TriggeredBy: "api"},
			}
			b, _ := json.Marshal(runs)
			return http.StatusOK, b
		},
	}
	cmd, out := newTestRunCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"list", "--job", "hello"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), "run-1") || !strings.Contains(out.String(), "Succeeded") {
		t.Errorf("unexpected output: %s", out.String())
	}
	if !strings.Contains(tr.records[0].path, "jobName=hello") {
		t.Errorf("unexpected query: %s", tr.records[0].path)
	}
}

func TestRunList_EmptyShowsMessage(t *testing.T) {
	tr := &captureTransport{responseFor: func(path string) (int, []byte) { return http.StatusOK, []byte("[]") }}
	cmd, out := newTestRunCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"list", "--job", "hello"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), "(no runs)") {
		t.Errorf("expected empty message, got: %s", out.String())
	}
}

func TestRunDelete_Success(t *testing.T) {
	tr := &captureTransport{responseFor: func(path string) (int, []byte) { return http.StatusNoContent, nil }}
	cmd, out := newTestRunCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"delete", "run-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if tr.records[0].path != "/api/v1/runs/run-1" {
		t.Errorf("unexpected path: %s", tr.records[0].path)
	}
	if !strings.Contains(out.String(), "run-1") {
		t.Errorf("unexpected output: %s", out.String())
	}
}

func TestRunDelete_ConflictPropagatesMessage(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			return http.StatusConflict, []byte("run run-1 is still Running; only terminal runs can be deleted")
		},
	}
	cmd, _ := newTestRunCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"delete", "run-1"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "still Running") {
		t.Fatalf("expected conflict error, got: %v", err)
	}
}
