package cli

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
)

// An older server may still return a credential-bearing repoURL; the CLI must
// never print it or write it into an exported manifest (which gets committed).
const leakyRepoURL = "https://user:s3cr3tpw@github.com/acme/pipelines"

func TestAppSourceListAndGet_NeverPrintCredentials(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			m := api.AppSourceMeta{Name: "p", RepoURL: leakyRepoURL, TargetRevision: "main"}
			if path == "/api/v1/appsources" {
				b, _ := json.Marshal([]api.AppSourceMeta{m})
				return http.StatusOK, b
			}
			b, _ := json.Marshal(m)
			return http.StatusOK, b
		},
	}
	for _, args := range [][]string{{"list"}, {"get", "p"}} {
		cmd, out := newTestAppSourceCmd(t, tr, "http://fake")
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if strings.Contains(out.String(), "s3cr3t") {
			t.Errorf("%v output leaks the credential:\n%s", args, out.String())
		}
		if !strings.Contains(out.String(), "https://github.com/acme/pipelines") {
			t.Errorf("%v output should show the sanitized URL:\n%s", args, out.String())
		}
	}
}

func TestExport_AppSourceNeverWritesCredentials(t *testing.T) {
	inner := exportFixtures(t, false)
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			if path == "/api/v1/appsources" {
				b, _ := json.Marshal([]api.AppSourceMeta{{Name: "src1", RepoURL: leakyRepoURL, TargetRevision: "main", Path: "jobs"}})
				return http.StatusOK, b
			}
			return inner.responseFor(path)
		},
	}
	dir := filepath.Join(t.TempDir(), "out")
	cmd, _ := newTestExportCmd(tr)
	cmd.SetArgs([]string{"-o", dir})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "appsources", "src1.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "s3cr3t") {
		t.Errorf("exported AppSource leaks the credential:\n%s", b)
	}
	if !strings.Contains(string(b), "https://github.com/acme/pipelines") {
		t.Errorf("exported AppSource should keep the sanitized URL:\n%s", b)
	}
}
