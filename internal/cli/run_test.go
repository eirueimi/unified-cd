package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/eirueimi/unified-cd/internal/api"
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

func TestRunCancel_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/runs/run-1/cancel" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("unexpected authorization header: %s", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	cfg := Config{Server: srv.URL, Token: "tok"}
	cmd := newRunCmdWithClient(func() (Config, error) { return cfg, nil }, http.DefaultClient)
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"cancel", "run-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), "run-1") || !strings.Contains(out.String(), "cancelled") {
		t.Errorf("unexpected output: %s", out.String())
	}
}

func TestRunListActive_PrintsRows(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			runs := []api.Run{
				{ID: "run-9", JobName: "deploy", Status: api.RunRunning, CreatedAt: time.Date(2026, 7, 1, 12, 30, 0, 0, time.UTC), TriggeredBy: "manual"},
			}
			b, _ := json.Marshal(runs)
			return http.StatusOK, b
		},
	}
	cmd, out := newTestRunCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"list-active"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if tr.records[0].path != "/api/v1/runs/active" {
		t.Errorf("unexpected path: %s", tr.records[0].path)
	}
	if !strings.Contains(out.String(), "run-9") || !strings.Contains(out.String(), "deploy") || !strings.Contains(out.String(), "Running") {
		t.Errorf("unexpected output: %s", out.String())
	}
}

func TestRunListActive_EmptyShowsMessage(t *testing.T) {
	tr := &captureTransport{responseFor: func(path string) (int, []byte) { return http.StatusOK, []byte("[]") }}
	cmd, out := newTestRunCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"list-active"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), "(no active runs)") {
		t.Errorf("expected empty message, got: %s", out.String())
	}
}

func TestRunOutputs_PrintsSortedKeyValues(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			b, _ := json.Marshal(api.RunOutputs{RunID: "run-1", Outputs: map[string]string{"zeta": "2", "alpha": "1"}})
			return http.StatusOK, b
		},
	}
	cmd, out := newTestRunCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"outputs", "run-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if tr.records[0].path != "/api/v1/runs/run-1/outputs" {
		t.Errorf("unexpected path: %s", tr.records[0].path)
	}
	if got, want := out.String(), "alpha=1\nzeta=2\n"; got != want {
		t.Errorf("unexpected output: %q (want %q)", got, want)
	}
}

func TestRunOutputs_RequiresRunID(t *testing.T) {
	tr := &captureTransport{responseFor: func(path string) (int, []byte) { return http.StatusOK, []byte("{}") }}
	cmd, _ := newTestRunCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"outputs"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when run-id is omitted")
	}
}

func TestRunOutputs_EmptyShowsMessage(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			b, _ := json.Marshal(api.RunOutputs{RunID: "run-1", Outputs: map[string]string{}})
			return http.StatusOK, b
		},
	}
	cmd, out := newTestRunCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"outputs", "run-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), "(no outputs)") {
		t.Errorf("expected empty message, got: %s", out.String())
	}
}

func TestRunShowYAML_PrintsRawBody(t *testing.T) {
	yamlBody := "apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: hello\n"
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) { return http.StatusOK, []byte(yamlBody) },
	}
	cmd, out := newTestRunCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"show-yaml", "run-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if tr.records[0].path != "/api/v1/runs/run-1/yaml" {
		t.Errorf("unexpected path: %s", tr.records[0].path)
	}
	if out.String() != yamlBody {
		t.Errorf("unexpected output: %q", out.String())
	}
}

func TestRunShowYAML_RequiresRunID(t *testing.T) {
	tr := &captureTransport{responseFor: func(path string) (int, []byte) { return http.StatusOK, nil }}
	cmd, _ := newTestRunCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"show-yaml"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when run-id is omitted")
	}
}

func TestRunApprovals_PrintsRows(t *testing.T) {
	decidedAt := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			list := []api.RunApproval{
				{RunID: "run-1", StepIndex: 2, StepName: "deploy-gate", Status: "Approved", DecidedBy: "alice", DecidedAt: &decidedAt},
				{RunID: "run-1", StepIndex: 4, StepName: "", Status: "Pending"},
			}
			b, _ := json.Marshal(list)
			return http.StatusOK, b
		},
	}
	cmd, out := newTestRunCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"approvals", "run-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if tr.records[0].path != "/api/v1/runs/run-1/approvals" {
		t.Errorf("unexpected path: %s", tr.records[0].path)
	}
	s := out.String()
	if !strings.Contains(s, "deploy-gate") || !strings.Contains(s, "Approved") || !strings.Contains(s, "by alice") {
		t.Errorf("unexpected output: %s", s)
	}
	if !strings.Contains(s, "step[4]") || !strings.Contains(s, "Pending") {
		t.Errorf("unexpected output for unnamed pending gate: %s", s)
	}
}

func TestRunApprovals_EmptyShowsMessage(t *testing.T) {
	tr := &captureTransport{responseFor: func(path string) (int, []byte) { return http.StatusOK, []byte("[]") }}
	cmd, out := newTestRunCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"approvals", "run-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), "(no approvals)") {
		t.Errorf("expected empty message, got: %s", out.String())
	}
}

func TestRunApprovals_RequiresRunID(t *testing.T) {
	tr := &captureTransport{responseFor: func(path string) (int, []byte) { return http.StatusOK, []byte("[]") }}
	cmd, _ := newTestRunCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"approvals"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when run-id is omitted")
	}
}

func TestRunCancel_ErrorPropagatesMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte("run run-1 is already terminal; cannot cancel"))
	}))
	defer srv.Close()

	cfg := Config{Server: srv.URL, Token: "tok"}
	cmd := newRunCmdWithClient(func() (Config, error) { return cfg, nil }, http.DefaultClient)
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"cancel", "run-1"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "already terminal") {
		t.Fatalf("expected conflict error, got: %v", err)
	}
}
