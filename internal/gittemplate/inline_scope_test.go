package gittemplate

import (
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/dsl"
)

func scopedTemplate() dsl.Spec {
	return dsl.Spec{Steps: []dsl.StepEntry{
		{Name: "compile", Run: "make build"},
		{Name: "save", UploadArtifact: &dsl.UploadArtifactStep{Name: "bin", Path: "./out"}},
	}}
}

func TestExpandUsesScopeTagsSteps(t *testing.T) {
	out, err := expandUsesStep("build", map[string]string{}, scopedTemplate(), &dsl.RunsIn{Image: "golang:1.22"})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	for _, s := range out {
		if s.Name == inputsStepName("build") {
			continue // the synthetic inputs step
		}
		if s.Name == "build" {
			continue // the synthetic capture step
		}
		if s.ScopeID != "scope:build" || s.ScopeImage != "golang:1.22" {
			t.Fatalf("step %q not scope-tagged: %+v", s.Name, s)
		}
		if s.RunsIn != nil {
			t.Fatalf("step %q should not carry RunsIn in scope mode: %+v", s.Name, s.RunsIn)
		}
	}
}

func TestExpandUsesNestedRunsInIsError(t *testing.T) {
	tpl := dsl.Spec{Steps: []dsl.StepEntry{
		{Name: "lint", Run: "golangci-lint run", RunsIn: &dsl.RunsIn{Image: "golangci/lint:latest"}},
	}}
	_, err := expandUsesStep("build", map[string]string{}, tpl, &dsl.RunsIn{Image: "golang:1.22"})
	if err == nil || !strings.Contains(err.Error(), "lint") {
		t.Fatalf("expected nested-runsIn error naming step, got %v", err)
	}
}

func TestExpandUsesContainerModeUnchanged(t *testing.T) {
	// uses-level runsIn.container is NOT scope mode: keep propagating RunsIn.
	out, err := expandUsesStep("build", map[string]string{}, scopedTemplate(), &dsl.RunsIn{Container: "builder"})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	for _, s := range out {
		if s.ScopeID != "" {
			t.Fatalf("container mode must not scope-tag: %+v", s)
		}
	}
}
