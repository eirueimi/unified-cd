package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"gopkg.in/yaml.v3"
)

// TestExport_RoundTripQualifiedNames verifies that parsing exported files with
// the same rules the AppSource reconciler uses (dir-based qualification)
// reproduces the original qualified job names.
func TestExport_RoundTripQualifiedNames(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	cmd, _ := newTestExportCmd(exportFixtures(t, false))
	cmd.SetArgs([]string{"-o", dir})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	wantJobs := map[string]bool{"team-a/build": false, "hello": false}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.Contains(string(b), "kind: Job") {
			return nil
		}
		job, err := dsl.Parse(bytes.NewReader(b))
		if err != nil {
			t.Errorf("%s: exported job must re-parse: %v", path, err)
			return nil
		}
		rel, _ := filepath.Rel(dir, filepath.Dir(path))
		reldir := filepath.ToSlash(rel)
		if reldir == "." {
			reldir = ""
		}
		qualified := dsl.QualifyName(reldir, job.Metadata.Name)
		if _, ok := wantJobs[qualified]; !ok {
			t.Errorf("unexpected qualified name %q from %s", qualified, path)
			return nil
		}
		wantJobs[qualified] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	for name, seen := range wantJobs {
		if !seen {
			t.Errorf("job %q not reproduced by round-trip", name)
		}
	}
}

// TestExport_RoundTripAllKindsParse re-parses EVERY exported file with the
// strict (KnownFields(true)) parser matching its kind. This is the regression
// test for C1: dsl.Spec and dsl.WebhookReceiverSpec have yaml-only tags, so
// json.Marshal(job.Spec) (what the store persists) produces capitalized Go
// field names ("Steps", "Name", "Trigger", "Auth", ...). If export renders
// those capitalized keys back out as YAML, the strict reconciler parsers
// reject the file ("field Steps not found in type dsl.Spec"). Every exported
// file, regardless of kind, must parse cleanly with its matching dsl parser.
func TestExport_RoundTripAllKindsParse(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	cmd, _ := newTestExportCmd(exportFixtures(t, false))
	cmd.SetArgs([]string{"-o", dir})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	parsed := map[string]int{}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var probe struct {
			Kind string `yaml:"kind"`
		}
		if err := yaml.Unmarshal(b, &probe); err != nil {
			t.Errorf("%s: probe kind: %v", path, err)
			return nil
		}
		switch probe.Kind {
		case "Job":
			if _, err := dsl.Parse(bytes.NewReader(b)); err != nil {
				t.Errorf("%s: exported Job must re-parse with strict parser: %v", path, err)
			}
		case "Schedule":
			if _, err := dsl.ParseSchedule(bytes.NewReader(b)); err != nil {
				t.Errorf("%s: exported Schedule must re-parse with strict parser: %v", path, err)
			}
		case "WebhookReceiver":
			if _, err := dsl.ParseWebhookReceiver(bytes.NewReader(b)); err != nil {
				t.Errorf("%s: exported WebhookReceiver must re-parse with strict parser: %v", path, err)
			}
		case "GitCredential":
			if _, err := dsl.ParseGitCredential(bytes.NewReader(b)); err != nil {
				t.Errorf("%s: exported GitCredential must re-parse with strict parser: %v", path, err)
			}
		case "AppSource":
			if _, err := dsl.ParseAppSource(bytes.NewReader(b)); err != nil {
				t.Errorf("%s: exported AppSource must re-parse with strict parser: %v", path, err)
			}
		default:
			t.Errorf("%s: unrecognized kind %q", path, probe.Kind)
			return nil
		}
		parsed[probe.Kind]++
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	for _, kind := range []string{"Job", "Schedule", "WebhookReceiver", "GitCredential", "AppSource"} {
		if parsed[kind] == 0 {
			t.Errorf("expected at least one exported %s file, found none", kind)
		}
	}
}

// TestExport_RoundTripMatrixForeachJob pins the fidelity of matrix/foreach
// steps through export → strict re-parse. MatrixDef and ForeachSource have
// custom UnmarshalYAML (mapping form / sequence-or-string form); without
// matching MarshalYAML, the typed re-marshal emits their raw struct shape
// ("dimensions:", "literal:"), which the strict parser rejects — silently
// breaking export for any repo that uses matrix or foreach.
func TestExport_RoundTripMatrixForeachJob(t *testing.T) {
	const matrixJobYAML = `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: matrix-build
spec:
  steps:
    - name: build
      matrix:
        os: [linux, windows, darwin]
        arch: [amd64, arm64]
        exclude:
          - os: windows
            arch: arm64
      run: echo build
    - name: deploy
      foreach:
        key: env
        in: "{{ .Params.envs }}"
      run: echo deploy
`
	spec := realJobSpecJSON(t, matrixJobYAML)
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			switch path {
			case "/api/v1/jobs":
				b, _ := json.Marshal([]api.Job{
					{Name: "matrix-build", Path: "", Leaf: "matrix-build", APIVersion: "unified-cd/v1", Spec: spec},
				})
				return http.StatusOK, b
			}
			return http.StatusOK, []byte(`[]`)
		},
	}
	dir := filepath.Join(t.TempDir(), "out")
	cmd, _ := newTestExportCmd(tr)
	cmd.SetArgs([]string{"-o", dir})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(dir, "matrix-build.yaml"))
	if err != nil {
		t.Fatalf("read exported job: %v", err)
	}
	job, err := dsl.Parse(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("exported matrix job must re-parse with strict parser: %v\n--- exported yaml ---\n%s", err, b)
	}

	build := job.Spec.Steps[0]
	if build.Matrix == nil {
		t.Fatalf("matrix definition lost in round-trip:\n%s", b)
	}
	wantDims := []dsl.MatrixDimension{
		{Name: "os", Source: dsl.ForeachSource{Literal: []string{"linux", "windows", "darwin"}}},
		{Name: "arch", Source: dsl.ForeachSource{Literal: []string{"amd64", "arm64"}}},
	}
	if !reflect.DeepEqual(build.Matrix.Dimensions, wantDims) {
		t.Errorf("matrix dimensions = %+v, want %+v", build.Matrix.Dimensions, wantDims)
	}
	wantExclude := []map[string]string{{"os": "windows", "arch": "arm64"}}
	if !reflect.DeepEqual(build.Matrix.Exclude, wantExclude) {
		t.Errorf("matrix exclude = %+v, want %+v", build.Matrix.Exclude, wantExclude)
	}

	deploy := job.Spec.Steps[1]
	if deploy.Foreach == nil {
		t.Fatalf("foreach definition lost in round-trip:\n%s", b)
	}
	if deploy.Foreach.Key != "env" {
		t.Errorf("foreach key = %q, want %q", deploy.Foreach.Key, "env")
	}
	if deploy.Foreach.Source.Expr != "{{ .Params.envs }}" {
		t.Errorf("foreach expr = %q, want %q", deploy.Foreach.Source.Expr, "{{ .Params.envs }}")
	}
}
