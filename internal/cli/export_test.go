package cli

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/spf13/cobra"
)

// realJobSpecJSON parses jobYAML with the real DSL parser and marshals its Spec
// with encoding/json, reproducing the CAPITALIZED-field JSON that the store
// actually holds (json.Marshal on a yaml-tag-only struct uses Go field names).
// Fixtures must use this instead of hand-written lowercase JSON so tests catch
// key-casing bugs in export (see spec_yaml.go's specJSONToYAML precedent).
func realJobSpecJSON(t *testing.T, jobYAML string) []byte {
	t.Helper()
	job, err := dsl.Parse(strings.NewReader(jobYAML))
	if err != nil {
		t.Fatalf("parse fixture job yaml: %v", err)
	}
	b, err := json.Marshal(job.Spec)
	if err != nil {
		t.Fatalf("marshal fixture job spec: %v", err)
	}
	return b
}

// realWebhookSpecJSON is the WebhookReceiver analog of realJobSpecJSON.
func realWebhookSpecJSON(t *testing.T, whYAML string) []byte {
	t.Helper()
	wr, err := dsl.ParseWebhookReceiver(strings.NewReader(whYAML))
	if err != nil {
		t.Fatalf("parse fixture webhookreceiver yaml: %v", err)
	}
	b, err := json.Marshal(wr.Spec)
	if err != nil {
		t.Fatalf("marshal fixture webhookreceiver spec: %v", err)
	}
	return b
}

// exportFixtures returns a captureTransport serving a small consistent dataset.
// managed controls whether AppSource src1 reports Job team-a/build as managed.
func exportFixtures(t *testing.T, managed bool) *captureTransport {
	t.Helper()
	jobSpec := realJobSpecJSON(t, `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: build
spec:
  steps:
    - name: greet
      run: echo hi
`)
	whSpec := realWebhookSpecJSON(t, `
apiVersion: unified-cd/v1
kind: WebhookReceiver
metadata:
  name: gh
spec:
  trigger:
    job: hello
  auth:
    type: none
`)
	return &captureTransport{
		responseFor: func(path string) (int, []byte) {
			switch path {
			case "/api/v1/appsources":
				m := api.AppSourceMeta{Name: "src1", RepoURL: "https://x/y.git", TargetRevision: "main", Path: "jobs"}
				if managed {
					m.ManagedResources = []api.ResourceRef{{Kind: "Job", Name: "team-a/build"}}
				}
				b, _ := json.Marshal([]api.AppSourceMeta{m})
				return http.StatusOK, b
			case "/api/v1/jobs":
				b, _ := json.Marshal([]api.Job{
					{Name: "team-a/build", Path: "team-a", Leaf: "build", APIVersion: "unified-cd/v1", Spec: jobSpec},
					{Name: "hello", Path: "", Leaf: "hello", APIVersion: "unified-cd/v1", Spec: jobSpec},
				})
				return http.StatusOK, b
			case "/api/v1/schedules":
				b, _ := json.Marshal([]api.ScheduleMeta{{Name: "nightly", Cron: "0 3 * * *", JobName: "hello", Params: map[string]string{"env": "prod"}}})
				return http.StatusOK, b
			case "/api/v1/webhooks":
				b, _ := json.Marshal([]api.WebhookReceiverMeta{{Name: "gh", Spec: whSpec}})
				return http.StatusOK, b
			case "/api/v1/gitcredentials":
				b, _ := json.Marshal([]api.GitCredentialMeta{{Name: "github", Host: "github.com", CredType: "token", SecretRef: "gh-token"}})
				return http.StatusOK, b
			}
			return http.StatusNotFound, []byte("not found")
		},
	}
}

func newTestExportCmd(tr *captureTransport) (*cobra.Command, *strings.Builder) {
	cfg := Config{Server: "http://fake", Token: "tok"}
	cmd := newExportCmdWithClient(func() (Config, error) { return cfg, nil }, &http.Client{Transport: tr})
	var out strings.Builder
	cmd.SetOut(&out)
	return cmd, &out
}

func TestExport_WritesAllKinds(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	cmd, out := newTestExportCmd(exportFixtures(t, false))
	cmd.SetArgs([]string{"-o", dir})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	for _, f := range []string{
		"team-a/build.yaml", "hello.yaml",
		"schedules/nightly.yaml", "webhookreceivers/gh.yaml",
		"gitcredentials/github.yaml", "appsources/src1.yaml",
	} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f))); err != nil {
			t.Errorf("expected file %s: %v", f, err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(dir, "team-a", "build.yaml"))
	s := string(b)
	for _, want := range []string{"apiVersion: unified-cd/v1", "kind: Job", "name: build", "run: echo hi"} {
		if !strings.Contains(s, want) {
			t.Errorf("job yaml missing %q:\n%s", want, s)
		}
	}
	sched, _ := os.ReadFile(filepath.Join(dir, "schedules", "nightly.yaml"))
	for _, want := range []string{"kind: Schedule", "cron:", "job: hello", "env: prod"} {
		if !strings.Contains(string(sched), want) {
			t.Errorf("schedule yaml missing %q:\n%s", want, string(sched))
		}
	}
	gc, _ := os.ReadFile(filepath.Join(dir, "gitcredentials", "github.yaml"))
	for _, want := range []string{"kind: GitCredential", "host: github.com", "type: token", "secretRef: gh-token"} {
		if !strings.Contains(string(gc), want) {
			t.Errorf("gitcredential yaml missing %q:\n%s", want, string(gc))
		}
	}
	if !strings.Contains(out.String(), "exported 6 resources (0 skipped as managed); secrets are not exported") {
		t.Errorf("unexpected summary: %s", out.String())
	}
}

func TestExport_UnmanagedOnlySkipsManaged(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	cmd, out := newTestExportCmd(exportFixtures(t, true))
	cmd.SetArgs([]string{"-o", dir, "--unmanaged-only"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "team-a", "build.yaml")); !os.IsNotExist(err) {
		t.Errorf("managed job must be skipped, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "hello.yaml")); err != nil {
		t.Errorf("unmanaged job must be exported: %v", err)
	}
	if !strings.Contains(out.String(), "exported 5 resources (1 skipped as managed)") {
		t.Errorf("unexpected summary: %s", out.String())
	}
}

func TestExport_RefusesNonEmptyDirWithoutForce(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "existing.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd, _ := newTestExportCmd(exportFixtures(t, false))
	cmd.SetArgs([]string{"-o", dir})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("expected not-empty error, got %v", err)
	}

	// --force なら書ける
	cmd2, _ := newTestExportCmd(exportFixtures(t, false))
	cmd2.SetArgs([]string{"-o", dir, "--force"})
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("with --force: %v", err)
	}
}

func TestExport_RejectsReservedDirCollision(t *testing.T) {
	jobSpec := realJobSpecJSON(t, `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: evil
spec:
  steps:
    - name: s
      run: echo
`)
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			switch path {
			case "/api/v1/appsources":
				return http.StatusOK, []byte(`[]`)
			case "/api/v1/jobs":
				b, _ := json.Marshal([]api.Job{{Name: "schedules/evil", Path: "schedules", Leaf: "evil", APIVersion: "unified-cd/v1", Spec: jobSpec}})
				return http.StatusOK, b
			}
			return http.StatusOK, []byte(`[]`)
		},
	}
	cmd, _ := newTestExportCmd(tr)
	cmd.SetArgs([]string{"-o", filepath.Join(t.TempDir(), "out")})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("expected reserved-dir error, got %v", err)
	}
}
