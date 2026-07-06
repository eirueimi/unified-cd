package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
