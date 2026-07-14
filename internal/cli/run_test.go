package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

// TestRunTriggerCmd_WaitOutputEndToEnd drives `run trigger --wait --output
// url`: POST creates the run, GET polls it to Succeeded, then GET
// .../outputs is fetched and the requested key's value is printed.
func TestRunTriggerCmd_WaitOutputEndToEnd(t *testing.T) {
	var terminal atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/runs":
			fmt.Fprint(w, `{"id":"r1","status":"Pending"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runs/r1/outputs":
			fmt.Fprint(w, `{"runId":"r1","outputs":{"url":"https://x"}}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/runs/r1"):
			if terminal.Swap(true) {
				fmt.Fprint(w, `{"id":"r1","status":"Succeeded"}`)
			} else {
				fmt.Fprint(w, `{"id":"r1","status":"Running"}`)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	cfg := Config{Server: srv.URL, Token: "tok"}
	cmd := newRunTriggerCmd(func() (Config, error) { return cfg, nil }, srv.Client())
	// Note: --output implies --wait, so --wait is intentionally omitted here.
	cmd.SetArgs([]string{"my-job", "--output", "url"})
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	err := cmd.Execute()
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	// stdout must carry ONLY the captured output value(s) so that
	// URL=$(unified-cli run trigger ... --output url) captures exactly the value.
	if got := out.String(); got != "https://x\n" {
		t.Errorf("stdout should be only the output value, got %q", got)
	}
	// The run id must NOT pollute stdout; it goes to stderr instead.
	if strings.Contains(out.String(), "r1") {
		t.Errorf("run id must not appear on stdout, got %q", out.String())
	}
	if !strings.Contains(errb.String(), "r1") {
		t.Errorf("run id should be printed to stderr, got %q", errb.String())
	}
}

// TestRunTriggerCmd_WaitOutput_MissingKeyErrors verifies that requesting an
// --output key the run did not report returns an error.
func TestRunTriggerCmd_WaitOutput_MissingKeyErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/runs":
			fmt.Fprint(w, `{"id":"r1","status":"Succeeded"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runs/r1/outputs":
			fmt.Fprint(w, `{"runId":"r1","outputs":{}}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/runs/r1"):
			fmt.Fprint(w, `{"id":"r1","status":"Succeeded"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	cfg := Config{Server: srv.URL, Token: "tok"}
	cmd := newRunTriggerCmd(func() (Config, error) { return cfg, nil }, srv.Client())
	cmd.SetArgs([]string{"my-job", "--output", "missing"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), `no output "missing"`) {
		t.Fatalf("expected missing-output error, got: %v", err)
	}
}

// TestRunTriggerCmd_WaitOutput_DoesNotPrintOnFailure verifies that outputs
// are not fetched/printed when the run does not succeed (waitForRun returns
// an *ExitError).
func TestRunTriggerCmd_WaitOutput_DoesNotPrintOnFailure(t *testing.T) {
	outputsHit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/runs":
			fmt.Fprint(w, `{"id":"r1","status":"Pending"}`)
		case r.URL.Path == "/api/v1/runs/r1/outputs":
			outputsHit = true
			fmt.Fprint(w, `{"runId":"r1","outputs":{"url":"https://x"}}`)
		default: // GET /api/v1/runs/r1
			fmt.Fprint(w, `{"id":"r1","status":"Failed"}`)
		}
	}))
	defer srv.Close()
	cfg := Config{Server: srv.URL, Token: "tok"}
	cmd := newRunTriggerCmd(func() (Config, error) { return cfg, nil }, srv.Client())
	cmd.SetArgs([]string{"my-job", "--output", "url"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	var ee *ExitError
	if err == nil {
		t.Fatal("expected an error for a failed run")
	} else if !errors.As(err, &ee) {
		t.Fatalf("expected *ExitError, got: %v", err)
	} else if ee.Code != 1 {
		t.Errorf("expected exit code 1, got %d", ee.Code)
	}
	if outputsHit {
		t.Error("outputs endpoint must not be hit when the run did not succeed")
	}
}

// TestRunTriggerCmd_ParamFileMergesWithParamOverride verifies --param-file
// loads key=value lines (skipping blanks/comments) and that --param
// overrides on key conflict.
func TestRunTriggerCmd_ParamFileMergesWithParamOverride(t *testing.T) {
	content := "# comment\n\nenv=staging\nregion=us-east-1\n"
	tmp := filepath.Join(t.TempDir(), "params.env")
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			b, _ := json.Marshal(api.Run{ID: "run-1", JobName: "hello"})
			return http.StatusOK, b
		},
	}
	cmd, _ := newTestRunCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"trigger", "hello", "--param-file", tmp, "--param", "env=prod"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	var req api.TriggerRunRequest
	if err := json.Unmarshal(tr.records[0].body, &req); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if req.Params["env"] != "prod" {
		t.Errorf("expected --param to override param-file, got env=%q", req.Params["env"])
	}
	if req.Params["region"] != "us-east-1" {
		t.Errorf("expected param-file value to be present, got region=%q", req.Params["region"])
	}
}

// TestRunTriggerCmd_ParamFile_BadLineErrors verifies a param-file line
// without "=" is rejected.
func TestRunTriggerCmd_ParamFile_BadLineErrors(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "params.env")
	if err := os.WriteFile(tmp, []byte("not-a-kv-line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tr := &captureTransport{responseFor: func(path string) (int, []byte) { return http.StatusOK, []byte("{}") }}
	cmd, _ := newTestRunCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"trigger", "hello", "--param-file", tmp})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "expected key=value") {
		t.Fatalf("expected param-file parse error, got: %v", err)
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
