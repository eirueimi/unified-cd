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
	out, _, err := expandUsesStep("build", map[string]string{}, scopedTemplate(), &dsl.RunsIn{Image: "golang:1.22"}, "", "")
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
	_, _, err := expandUsesStep("build", map[string]string{}, tpl, &dsl.RunsIn{Image: "golang:1.22"}, "", "")
	if err == nil || !strings.Contains(err.Error(), "lint") {
		t.Fatalf("expected nested-runsIn error naming step, got %v", err)
	}
}

// TestExpandUsesNestedScopeSurvivesOuterContainerDefaulting pins the one
// behaviour change nobody explicitly asked for but that fell out of the
// whole-struct copy: a template step already scope-tagged by an *inner*
// uses: runsIn.image (a nested "template calls template into an isolated
// scope" shape, already resolved by the time it reaches this outer
// expandUsesStep — nested uses: must be resolved before this function runs)
// must keep its ScopeID/ScopeImage when the *outer* uses: is itself
// container-mode, not scope-mode. The reflection fixture in
// inline_fields_test.go only proves the copy carries ScopeID at all; it
// never exercises two inlining passes, so it can't see whether the outer
// pass's container-defaulting (inline.go's `else if ns.Container == ""`)
// clobbers or otherwise disturbs tags that already came from an inner pass.
//
// Before this branch's fix, the concrete-step literal never carried
// ScopeID/ScopeImage from `inner` at all outside of the (local) scope-mode
// stamp, so this exact shape silently lost its isolation: the nested
// template's steps ran unscoped, sharing the caller's own workspace, and the
// image the inner runsIn.image named was never used.
func TestExpandUsesNestedScopeSurvivesOuterContainerDefaulting(t *testing.T) {
	// tpl stands in for a template whose "build" step was itself already
	// inlined from a nested `uses:` with runsIn.image — i.e. it already
	// carries ScopeID/ScopeImage and an empty Container, exactly as
	// renameInnerEntry's own scope-mode branch would have left it.
	tpl := dsl.Spec{Steps: []dsl.StepEntry{
		{Name: "build", Run: "./build.sh", ScopeID: "scope:inner", ScopeImage: "golang:1.22"},
	}}

	// The outer uses: step is container-mode (outerRunsIn nil, outerContainer
	// "builder"), not scope-mode — this is the branch that stamps
	// outerContainer onto any inlined step still missing its own container:.
	out, _, err := expandUsesStep("outer", map[string]string{}, tpl, nil, "builder", "")
	if err != nil {
		t.Fatalf("expand: %v", err)
	}

	var build *dsl.StepEntry
	for i := range out {
		if out[i].Name == "outer__build" {
			build = &out[i]
		}
	}
	if build == nil {
		t.Fatalf("inlined step outer__build missing from expansion: %+v", out)
	}
	// The nested scope tags must survive the outer (non-scope) inlining pass
	// untouched: this is the ScopeID/ScopeImage preservation the whole-struct
	// copy restores.
	if build.ScopeID != "scope:inner" || build.ScopeImage != "golang:1.22" {
		t.Fatalf("nested scope tags did not survive outer inlining: %+v", build)
	}
	// The outer pass's container-defaulting still applies independently — it
	// only looks at Container, never at ScopeID — so the step also picks up
	// the outer uses:'s container: alongside its (still governing) scope
	// tags. See orchestrator.go's isScopedStep case for why a step carrying
	// both is handled correctly at run time.
	if build.Container != "builder" {
		t.Fatalf("outer container-defaulting did not apply: %+v", build)
	}
}

func TestExpandUsesContainerModeUnchanged(t *testing.T) {
	// uses-level flat container: is NOT scope mode: keep propagating Container.
	out, _, err := expandUsesStep("build", map[string]string{}, scopedTemplate(), nil, "builder", "")
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	for _, s := range out {
		if s.ScopeID != "" {
			t.Fatalf("container mode must not scope-tag: %+v", s)
		}
	}
}

func TestExpandUsesScopeApprovalIsError(t *testing.T) {
	tpl := dsl.Spec{Steps: []dsl.StepEntry{
		{Name: "gate", Approval: &dsl.ApprovalStep{Message: "ok to proceed?"}},
	}}
	_, _, err := expandUsesStep("build", map[string]string{}, tpl, &dsl.RunsIn{Image: "golang:1.22"}, "", "")
	if err == nil || !strings.Contains(err.Error(), "gate") {
		t.Fatalf("expected approval-in-scope error naming step, got %v", err)
	}
}

func TestExpandUsesScopeCallIsError(t *testing.T) {
	tpl := dsl.Spec{Steps: []dsl.StepEntry{
		{Name: "delegate", Call: &dsl.CallStep{Job: "some-job"}},
	}}
	_, _, err := expandUsesStep("build", map[string]string{}, tpl, &dsl.RunsIn{Image: "golang:1.22"}, "", "")
	if err == nil || !strings.Contains(err.Error(), "delegate") {
		t.Fatalf("expected call-in-scope error naming step, got %v", err)
	}
}

func TestExpandUsesScopeParallelApprovalIsError(t *testing.T) {
	tpl := dsl.Spec{Steps: []dsl.StepEntry{
		{Parallel: []dsl.Step{
			{Name: "a", Run: "echo a"},
			{Name: "gate", Approval: &dsl.ApprovalStep{Message: "ok to proceed?"}},
		}},
	}}
	_, _, err := expandUsesStep("build", map[string]string{}, tpl, &dsl.RunsIn{Image: "golang:1.22"}, "", "")
	if err == nil || !strings.Contains(err.Error(), "gate") {
		t.Fatalf("expected approval-in-scope error naming step, got %v", err)
	}
}

func TestExpandUsesScopeParallelCallIsError(t *testing.T) {
	tpl := dsl.Spec{Steps: []dsl.StepEntry{
		{Parallel: []dsl.Step{
			{Name: "a", Run: "echo a"},
			{Name: "delegate", Call: &dsl.CallStep{Job: "some-job"}},
		}},
	}}
	_, _, err := expandUsesStep("build", map[string]string{}, tpl, &dsl.RunsIn{Image: "golang:1.22"}, "", "")
	if err == nil || !strings.Contains(err.Error(), "delegate") {
		t.Fatalf("expected call-in-scope error naming step, got %v", err)
	}
}

func TestExpandUsesContainerModeApprovalAndCallAllowed(t *testing.T) {
	// uses-level flat container: is NOT scope mode: approval/call must still work.
	tpl := dsl.Spec{Steps: []dsl.StepEntry{
		{Name: "gate", Approval: &dsl.ApprovalStep{Message: "ok to proceed?"}},
		{Name: "delegate", Call: &dsl.CallStep{Job: "some-job"}},
	}}
	out, _, err := expandUsesStep("build", map[string]string{}, tpl, nil, "builder", "")
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	assertApprovalAndCallSurvive(t, out)
}

func TestExpandUsesNoRunsInApprovalAndCallAllowed(t *testing.T) {
	// No uses-level runsIn/container at all: approval/call must still work.
	tpl := dsl.Spec{Steps: []dsl.StepEntry{
		{Name: "gate", Approval: &dsl.ApprovalStep{Message: "ok to proceed?"}},
		{Name: "delegate", Call: &dsl.CallStep{Job: "some-job"}},
	}}
	out, _, err := expandUsesStep("build", map[string]string{}, tpl, nil, "", "")
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	assertApprovalAndCallSurvive(t, out)
}

// assertApprovalAndCallSurvive checks the expansion actually kept the fields,
// not merely that it returned no error. "Allowed" used to mean only the latter,
// which is why the approval gate could be dropped silently: the step came out
// with no approval and no run:, so the agent executed an empty script and
// reported success while the human gate simply ceased to exist.
func assertApprovalAndCallSurvive(t *testing.T, out []dsl.StepEntry) {
	t.Helper()
	var gate, delegate *dsl.StepEntry
	for i := range out {
		switch out[i].Name {
		case "build__gate":
			gate = &out[i]
		case "build__delegate":
			delegate = &out[i]
		}
	}
	if gate == nil {
		t.Fatalf("inlined approval step build__gate missing from expansion: %+v", out)
	}
	if gate.Approval == nil {
		t.Fatalf("step %q lost its approval: %+v", gate.Name, gate)
	}
	if gate.Approval.Message != "ok to proceed?" {
		t.Fatalf("step %q approval message = %q, want %q", gate.Name, gate.Approval.Message, "ok to proceed?")
	}
	if gate.Run != "" {
		t.Fatalf("step %q must stay an approval gate, not degrade into run %q", gate.Name, gate.Run)
	}
	if delegate == nil {
		t.Fatalf("inlined call step build__delegate missing from expansion: %+v", out)
	}
	if delegate.Call == nil || delegate.Call.Job != "some-job" {
		t.Fatalf("step %q lost its call: %+v", delegate.Name, delegate)
	}
}
