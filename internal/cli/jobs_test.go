package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/eirueimi/unified-cd/internal/api"
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

func TestJobsGet_PrintsSummary(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			job := api.Job{
				ID:         "id-1",
				Name:       "build",
				APIVersion: "unified-cd/v1",
				Inputs: []api.InputDef{
					{Name: "image", Type: "string", Required: true, Description: "container image"},
					{Name: "tag", Type: "string", Default: "latest"},
				},
			}
			b, _ := json.Marshal(job)
			return http.StatusOK, b
		},
	}
	cmd, out := newTestJobsCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"get", "build"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if tr.records[0].path != "/api/v1/jobs/build" {
		t.Errorf("unexpected path: %s", tr.records[0].path)
	}
	s := out.String()
	if !strings.Contains(s, "build") || !strings.Contains(s, "image") || !strings.Contains(s, "required") || !strings.Contains(s, "default=latest") {
		t.Errorf("unexpected output: %s", s)
	}
}

func TestJobsGet_RequiresName(t *testing.T) {
	tr := &captureTransport{responseFor: func(path string) (int, []byte) { return http.StatusOK, []byte("{}") }}
	cmd, _ := newTestJobsCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"get"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when name is omitted")
	}
}

func TestJobsShowYAML_PrintsRawBody(t *testing.T) {
	yamlBody := "apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: build\n"
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) { return http.StatusOK, []byte(yamlBody) },
	}
	cmd, out := newTestJobsCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"show-yaml", "build"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if tr.records[0].path != "/api/v1/jobs/build/yaml" {
		t.Errorf("unexpected path: %s", tr.records[0].path)
	}
	if out.String() != yamlBody {
		t.Errorf("unexpected output: %q", out.String())
	}
}

func TestJobsShowYAML_NotFoundPropagatesMessage(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) { return http.StatusNotFound, []byte("job not found") },
	}
	cmd, _ := newTestJobsCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"show-yaml", "missing"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "job not found") {
		t.Fatalf("expected not-found error, got: %v", err)
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
