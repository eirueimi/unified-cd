package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/eirueimi/unified-cd/internal/api"
)

// captureTransport records requests and returns dummy responses without making real network connections.
type captureTransport struct {
	records []struct {
		method        string
		path          string
		body          []byte
		authorization string
	}
	responseFor func(path string) (int, []byte)
}

func (t *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var b []byte
	if req.Body != nil {
		b, _ = io.ReadAll(req.Body)
	}
	t.records = append(t.records, struct {
		method        string
		path          string
		body          []byte
		authorization string
	}{method: req.Method, path: req.URL.RequestURI(), body: b, authorization: req.Header.Get("Authorization")})

	status, respBody := t.responseFor(req.URL.RequestURI())
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(bytes.NewReader(respBody)),
		Header:     make(http.Header),
	}, nil
}

func newTestApplyCmd(t *testing.T, tr *captureTransport, serverURL string) (*cobra.Command, *strings.Builder) {
	t.Helper()
	cfg := Config{Server: serverURL, Token: "tok"}
	cmd := newApplyCmdWithClient(func() (Config, error) { return cfg, nil }, &http.Client{Transport: tr})
	var out strings.Builder
	cmd.SetOut(&out)
	return cmd, &out
}

func TestApplyMultiDocument(t *testing.T) {
	callIdx := 0
	names := []string{"hello", "world"}
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			name := names[callIdx]
			callIdx++
			b, _ := json.Marshal(api.Job{Name: name})
			return http.StatusOK, b
		},
	}

	yaml := `apiVersion: unified-cd/v1
kind: Job
metadata:
  name: hello
spec:
  steps:
    - name: greet
      run: echo hi
---
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: world
spec:
  steps:
    - name: greet
      run: echo world
`
	f, err := os.CreateTemp(t.TempDir(), "multi-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(yaml)
	f.Close()

	cmd, out := newTestApplyCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"-f", f.Name()})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if len(tr.records) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(tr.records))
	}
	for _, rec := range tr.records {
		if rec.path != "/api/v1/jobs" {
			t.Errorf("unexpected path %s", rec.path)
		}
	}
	if !strings.Contains(out.String(), "hello") || !strings.Contains(out.String(), "world") {
		t.Errorf("unexpected output: %s", out.String())
	}
}

func TestApplySingleDocument(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			b, _ := json.Marshal(api.Job{Name: "solo"})
			return http.StatusOK, b
		},
	}

	content := `apiVersion: unified-cd/v1
kind: Job
metadata:
  name: solo
spec:
  steps:
    - name: run
      run: echo solo
`
	tmp := filepath.Join(t.TempDir(), "solo.yaml")
	os.WriteFile(tmp, []byte(content), 0o600)

	cmd, out := newTestApplyCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"-f", tmp})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), "solo") {
		t.Errorf("unexpected output: %s", out.String())
	}
	if len(tr.records) != 1 {
		t.Fatalf("expected 1 request, got %d", len(tr.records))
	}
}

func TestApplyEmptyFile(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "empty.yaml")
	os.WriteFile(tmp, []byte(""), 0o600)

	cfg := Config{Server: "http://fake", Token: "tok"}
	cmd := newApplyCmdWithClient(func() (Config, error) { return cfg, nil }, http.DefaultClient)
	cmd.SetArgs([]string{"-f", tmp})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "no YAML documents") {
		t.Errorf("expected 'no YAML documents' error, got: %v", err)
	}
}

func TestApplyScheduleDocument(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			b, _ := json.Marshal(api.ScheduleMeta{Name: "nightly-build", Cron: "0 2 * * *", JobName: "build"})
			return http.StatusOK, b
		},
	}

	content := `apiVersion: unified-cd/v1
kind: Schedule
metadata:
  name: nightly-build
spec:
  cron: "0 2 * * *"
  job: build
  params:
    env: prod
`
	tmp := filepath.Join(t.TempDir(), "schedule.yaml")
	os.WriteFile(tmp, []byte(content), 0o600)

	cmd, out := newTestApplyCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"-f", tmp})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(tr.records) != 1 {
		t.Fatalf("expected 1 request, got %d", len(tr.records))
	}
	if tr.records[0].path != "/api/v1/schedules/" {
		t.Errorf("unexpected path %s", tr.records[0].path)
	}
	var req api.ApplyScheduleRequest
	if err := json.Unmarshal(tr.records[0].body, &req); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if !strings.Contains(req.YAML, "nightly-build") {
		t.Errorf("expected request YAML to contain schedule content, got: %s", req.YAML)
	}
	if !strings.Contains(out.String(), "nightly-build") {
		t.Errorf("unexpected output: %s", out.String())
	}
}

func TestApplyDryRun_ValidJobMakesNoHTTPCall(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			t.Fatal("--dry-run must not make an HTTP request")
			return 0, nil
		},
	}

	content := `apiVersion: unified-cd/v1
kind: Job
metadata:
  name: solo
spec:
  steps:
    - name: run
      run: echo solo
`
	tmp := filepath.Join(t.TempDir(), "solo.yaml")
	os.WriteFile(tmp, []byte(content), 0o600)

	cmd, out := newTestApplyCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"-f", tmp, "--dry-run"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), `"solo"`) || !strings.Contains(out.String(), "valid") {
		t.Errorf("unexpected output: %s", out.String())
	}
	if len(tr.records) != 0 {
		t.Errorf("expected no HTTP requests, got %d", len(tr.records))
	}
}

func TestApplyDryRun_InvalidYAMLErrorsWithoutHTTPCall(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			t.Fatal("--dry-run must not make an HTTP request")
			return 0, nil
		},
	}

	// metadata.name is required by dsl.Job.Validate; omitting it makes this
	// document invalid.
	content := `apiVersion: unified-cd/v1
kind: Job
metadata:
  name: ""
spec:
  steps:
    - name: run
      run: echo solo
`
	tmp := filepath.Join(t.TempDir(), "invalid.yaml")
	os.WriteFile(tmp, []byte(content), 0o600)

	cmd, _ := newTestApplyCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"-f", tmp, "--dry-run"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "metadata.name") {
		t.Fatalf("expected metadata.name validation error, got: %v", err)
	}
	if len(tr.records) != 0 {
		t.Errorf("expected no HTTP requests, got %d", len(tr.records))
	}
}

func TestApplyMultiDocumentStopsOnError(t *testing.T) {
	callCount := 0
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			callCount++
			return http.StatusInternalServerError, []byte("internal error")
		},
	}

	yaml := `apiVersion: unified-cd/v1
kind: Job
metadata:
  name: first
spec:
  steps: []
---
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: second
spec:
  steps: []
`
	tmp := filepath.Join(t.TempDir(), "multi.yaml")
	os.WriteFile(tmp, []byte(yaml), 0o600)

	cmd, _ := newTestApplyCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"-f", tmp})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error on server failure")
	}
	if callCount != 1 {
		t.Errorf("expected 1 call before abort, got %d", callCount)
	}
}
