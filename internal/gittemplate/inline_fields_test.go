package gittemplate

import (
	"reflect"
	"testing"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// inlineFieldPolicy records, for every field of dsl.StepEntry / dsl.Step, what
// uses: inlining is expected to do with it. It exists because renameInnerEntry
// once built its result as a fresh literal listing the fields worth keeping:
// approval:, retry:, matrix: and foreach: were added to the DSL afterwards,
// were never added to that list, and were therefore dropped silently — an
// approval gate declared in a shared template inlined as a step with no action
// at all, so the run sailed past the gate and reported success.
//
// The tests below fail if a field is added to dsl.StepEntry (or dsl.Step)
// without a policy recorded here, so the drop can never happen again without
// somebody making an explicit decision and writing down why.
type inlineFieldPolicy int

const (
	// fieldPreserved: inlining must carry the value through unchanged.
	fieldPreserved inlineFieldPolicy = iota

	// fieldTransformed: inlining deliberately rewrites the value (step-name
	// prefixing, if: combination, .Params./.Steps. reference rewriting). The
	// generic check only asserts the field did not come out zero; the exact
	// rewrite is asserted by name at the end of the test.
	fieldTransformed

	// fieldRejected: the field cannot appear on a template step reaching
	// renameInnerEntry at all — it returns an error instead of a step. The
	// fixture must leave it zero, and the test asserts the rejection.
	fieldRejected

	// fieldNotApplicable: only meaningful on the other branch of
	// renameInnerEntry (Parallel marks the parallel: branch, so it is by
	// definition zero on a concrete step, and dsl.Step has no such field).
	fieldNotApplicable
)

// stepEntryInlinePolicy covers every field of dsl.StepEntry. dsl.Step has the
// same fields minus Parallel, so both branches share this map.
//
// Read the fieldTransformed and fieldRejected entries as the deliberate
// exceptions: everything else must survive inlining byte-for-byte.
var stepEntryInlinePolicy = map[string]inlineFieldPolicy{
	// Rewritten by design.
	"Name":             fieldTransformed, // prefixed with the uses step's name
	"If":               fieldTransformed, // combined with the uses step's own if:
	"Run":              fieldTransformed, // .Params./.Steps. refs rewritten
	"Env":              fieldTransformed, // values ref-rewritten
	"Outputs":          fieldTransformed, // values ref-rewritten
	"Cache":            fieldTransformed, // path/key/restoreKeys ref-rewritten
	"UploadArtifact":   fieldTransformed, // name/path ref-rewritten
	"DownloadArtifact": fieldTransformed, // name/destDir ref-rewritten
	"Post":             fieldTransformed, // run/env ref-rewritten

	// Rejected outright — inlining fails rather than producing a step.
	"Uses": fieldRejected, // nested uses must already be resolved (resolveSteps)
	// step-level runsIn: was removed in favour of container: (2026-07-08 job
	// isolation); a template still carrying one is an error, not a drop.
	"RunsIn": fieldRejected,

	// Parallel: selects the other branch of renameInnerEntry.
	"Parallel": fieldNotApplicable,

	// Everything else must survive untouched.
	"Call":            fieldPreserved, // with: values intentionally not rewritten in v1
	"Approval":        fieldPreserved,
	"Retry":           fieldPreserved,
	"Matrix":          fieldPreserved,
	"Foreach":         fieldPreserved,
	"ContinueOnError": fieldPreserved,
	"TimeoutMinutes":  fieldPreserved,
	"Shell":           fieldPreserved, // a template-level spec.shell is stamped later, by stampShell
	// Container is preserved when the step declares one; a step that declares
	// none inherits the uses: step's container: instead (asserted separately in
	// inline_scope_test.go). The fixture declares one.
	"Container": fieldPreserved,
	// Scope tags are not user-authored: renameInnerEntry stamps them in scope
	// mode. Outside scope mode they must carry through, because a nested
	// scope-mode uses: inside this template has already stamped its own steps
	// and dropping the tags would silently strip that isolation.
	"ScopeID":    fieldPreserved,
	"ScopeImage": fieldPreserved,
}

// fullyPopulatedTemplateStep is a template step with every inlinable field set
// to a distinctive non-zero value. It is deliberately NOT a valid DSL step (it
// declares several mutually exclusive actions at once) — renameInnerEntry is a
// pure transform that runs after validation, and one maximal step exercises
// field carriage far more sharply than a set of individually valid ones.
func fullyPopulatedTemplateStep() dsl.StepEntry {
	return dsl.StepEntry{
		Name:    "gate",
		If:      "success()",
		Run:     "deploy {{ .Params.image }}",
		Env:     map[string]string{"IMAGE": "{{ .Params.image }}"},
		Outputs: map[string]string{"digest": "{{ .Params.image }}-digest"},
		Cache: &dsl.CacheStep{
			Path:        "{{ .Params.image }}/cache",
			Key:         "k-{{ .Params.image }}",
			RestoreKeys: []string{"k-{{ .Params.image }}-", "k-"},
			TTLDays:     7,
		},
		UploadArtifact:   &dsl.UploadArtifactStep{Name: "bin", Path: "./out/{{ .Params.image }}"},
		DownloadArtifact: &dsl.DownloadArtifactStep{Name: "bin", DestDir: "./in/{{ .Params.image }}", RunID: "abc123"},
		Post:             &dsl.PostStep{Run: "cleanup {{ .Params.image }}", Env: map[string]string{"K": "{{ .Params.image }}"}, Shell: []string{"sh", "-c"}},
		Call:             &dsl.CallStep{Job: "child-job", With: map[string]any{"k": "v"}},
		Approval:         &dsl.ApprovalStep{Message: "ship to production?", TimeoutMinutes: 30},
		Retry:            &dsl.RetrySpec{Attempts: 3, Backoff: "5s"},
		Foreach:          &dsl.ForeachDef{Key: "env", Source: dsl.ForeachSource{Literal: []string{"dev", "prod"}}},
		Matrix: &dsl.MatrixDef{
			Dimensions: []dsl.MatrixDimension{{Name: "os", Source: dsl.ForeachSource{Literal: []string{"linux", "darwin"}}}},
			Exclude:    []map[string]string{{"os": "darwin"}},
		},
		ContinueOnError: true,
		Container:       "builder",
		ScopeID:         "scope:nested",
		ScopeImage:      "golang:1.22",
		TimeoutMinutes:  12.5,
		Shell:           []string{"bash", "-lc"},
	}
}

// checkPolicyCoverage asserts every field of typ has a recorded policy, and
// that the fixture populates exactly the fields the policy expects it to.
func checkPolicyCoverage(t *testing.T, typ reflect.Type, fixture reflect.Value) {
	t.Helper()
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		policy, ok := stepEntryInlinePolicy[name]
		require.Truef(t, ok,
			"%s.%s has no recorded uses:-inlining policy: add one to stepEntryInlinePolicy and populate it in "+
				"fullyPopulatedTemplateStep, so renameInnerEntry cannot silently drop it the way approval:/retry:/"+
				"matrix:/foreach: were dropped", typ.Name(), name)
		switch policy {
		case fieldPreserved, fieldTransformed:
			require.Falsef(t, fixture.Field(i).IsZero(),
				"fixture leaves %s.%s zero — set it in fullyPopulatedTemplateStep so the preservation check can see it",
				typ.Name(), name)
		default:
			require.Truef(t, fixture.Field(i).IsZero(),
				"%s.%s is rejected/not-applicable for a concrete inlined step, so the fixture must leave it zero",
				typ.Name(), name)
		}
	}
}

// TestRenameInnerEntryPreservesEveryStepEntryField is the drift guard for the
// concrete-step branch: every dsl.StepEntry field that goes into inlining
// non-zero must come out non-zero, and every field not explicitly marked as
// transformed must come out identical.
func TestRenameInnerEntryPreservesEveryStepEntryField(t *testing.T) {
	inner := fullyPopulatedTemplateStep()
	typ := reflect.TypeOf(dsl.StepEntry{})
	inV := reflect.ValueOf(inner)
	checkPolicyCoverage(t, typ, inV)

	out, err := renameInnerEntry("deploy", map[string]bool{"gate": true}, "always()", false, "", "", "", inner)
	require.NoError(t, err)
	outV := reflect.ValueOf(out)

	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		switch stepEntryInlinePolicy[name] {
		case fieldPreserved:
			assert.Equalf(t, inV.Field(i).Interface(), outV.Field(i).Interface(),
				"dsl.StepEntry.%s must survive uses: inlining unchanged", name)
		case fieldTransformed:
			assert.Falsef(t, outV.Field(i).IsZero(),
				"dsl.StepEntry.%s came out of uses: inlining zero", name)
		}
	}

	// The transformed fields, spelled out: each is rewritten, none is lost.
	assert.Equal(t, "deploy__gate", out.Name)
	assert.Equal(t, "(always()) && (success())", out.If)
	assert.Equal(t, "deploy {{ .Steps.deploy__inputs.Outputs.image }}", out.Run)
	assert.Equal(t, "{{ .Steps.deploy__inputs.Outputs.image }}", out.Env["IMAGE"])
	assert.Equal(t, "{{ .Steps.deploy__inputs.Outputs.image }}-digest", out.Outputs["digest"])
	assert.Equal(t, "{{ .Steps.deploy__inputs.Outputs.image }}/cache", out.Cache.Path)
	assert.Equal(t, "k-{{ .Steps.deploy__inputs.Outputs.image }}", out.Cache.Key)
	assert.Equal(t, []string{"k-{{ .Steps.deploy__inputs.Outputs.image }}-", "k-"}, out.Cache.RestoreKeys)
	assert.Equal(t, 7, out.Cache.TTLDays, "a sub-struct field with no refs must survive the rewrite")
	assert.Equal(t, "./out/{{ .Steps.deploy__inputs.Outputs.image }}", out.UploadArtifact.Path)
	assert.Equal(t, "./in/{{ .Steps.deploy__inputs.Outputs.image }}", out.DownloadArtifact.DestDir)
	assert.Equal(t, "abc123", out.DownloadArtifact.RunID, "downloadArtifact.runId must survive the rewrite")
	assert.Equal(t, "cleanup {{ .Steps.deploy__inputs.Outputs.image }}", out.Post.Run)
	assert.Equal(t, "{{ .Steps.deploy__inputs.Outputs.image }}", out.Post.Env["K"])
	assert.Equal(t, []string{"sh", "-c"}, out.Post.Shell, "post.shell must survive the rewrite")

	// The rejected fields, spelled out: each is an error, not a silent drop.
	withUses := fullyPopulatedTemplateStep()
	withUses.Uses = &dsl.UsesStep{Job: "git://example.com/repo//tpl.yaml@v1"}
	_, err = renameInnerEntry("deploy", map[string]bool{"gate": true}, "", false, "", "", "", withUses)
	require.Error(t, err, "an unresolved nested uses: must be rejected, not dropped")

	withRunsIn := fullyPopulatedTemplateStep()
	withRunsIn.RunsIn = &dsl.RunsIn{Image: "golang:1.22"}
	_, err = renameInnerEntry("deploy", map[string]bool{"gate": true}, "", false, "", "", "", withRunsIn)
	require.Error(t, err, "step-level runsIn: must be rejected, not dropped")
}

// stepFromEntry copies every same-named field of a dsl.StepEntry fixture onto a
// dsl.Step, so the parallel: branch's fixture cannot drift out of sync with the
// concrete branch's.
func stepFromEntry(t *testing.T, e dsl.StepEntry) dsl.Step {
	t.Helper()
	var s dsl.Step
	sv := reflect.ValueOf(&s).Elem()
	ev := reflect.ValueOf(e)
	for i := 0; i < ev.NumField(); i++ {
		name := ev.Type().Field(i).Name
		f := sv.FieldByName(name)
		if !f.IsValid() {
			continue // Parallel has no dsl.Step counterpart
		}
		require.Equalf(t, ev.Field(i).Type(), f.Type(),
			"dsl.StepEntry.%s and dsl.Step.%s have diverged in type", name, name)
		f.Set(ev.Field(i))
	}
	return s
}

// TestRenameInnerEntryPreservesEveryParallelStepField is the same drift guard
// for the parallel: branch, which has always copied the whole struct. It is
// here so the two branches stay provably symmetric.
func TestRenameInnerEntryPreservesEveryParallelStepField(t *testing.T) {
	inner := stepFromEntry(t, fullyPopulatedTemplateStep())
	typ := reflect.TypeOf(dsl.Step{})
	inV := reflect.ValueOf(inner)
	checkPolicyCoverage(t, typ, inV)

	entry, err := renameInnerEntry("deploy", map[string]bool{"gate": true}, "always()", false, "", "", "",
		dsl.StepEntry{Parallel: []dsl.Step{inner}})
	require.NoError(t, err)
	require.Len(t, entry.Parallel, 1)
	outV := reflect.ValueOf(entry.Parallel[0])

	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		switch stepEntryInlinePolicy[name] {
		case fieldPreserved:
			assert.Equalf(t, inV.Field(i).Interface(), outV.Field(i).Interface(),
				"dsl.Step.%s must survive uses: inlining unchanged", name)
		case fieldTransformed:
			assert.Falsef(t, outV.Field(i).IsZero(),
				"dsl.Step.%s came out of uses: inlining zero", name)
		}
	}
	assert.Equal(t, "deploy__gate", entry.Parallel[0].Name)
	assert.Equal(t, "(always()) && (success())", entry.Parallel[0].If)
}

// TestExpandUsesPreservesDeferredStepFields is the end-to-end regression test
// for the reported defect: a template declaring approval:, retry:, matrix: or
// foreach: on a step inlined those fields away. The approval case is the
// dangerous one — the step came out with run: "" and no approval, so the agent
// executed an empty script, reported Succeeded, and the human gate the template
// promised was gone with no error and no log line.
func TestExpandUsesPreservesDeferredStepFields(t *testing.T) {
	tpl := dsl.Spec{Steps: []dsl.StepEntry{
		{Name: "gate", Approval: &dsl.ApprovalStep{Message: "ship to production?", TimeoutMinutes: 45}},
		{Name: "flaky", Run: "./flaky.sh", Retry: &dsl.RetrySpec{Attempts: 4, Backoff: "10s"}},
		{Name: "fan", Run: "./build.sh", Matrix: &dsl.MatrixDef{
			Dimensions: []dsl.MatrixDimension{{Name: "os", Source: dsl.ForeachSource{Literal: []string{"linux", "darwin"}}}},
		}},
		{Name: "each", Run: "./deploy.sh", Foreach: &dsl.ForeachDef{
			Key: "env", Source: dsl.ForeachSource{Literal: []string{"dev", "prod"}},
		}},
	}}

	expanded, _, err := expandUsesStep("u", map[string]string{}, tpl, nil, "", "")
	require.NoError(t, err)

	byName := map[string]dsl.StepEntry{}
	for _, s := range expanded {
		byName[s.Name] = s
	}

	gate := byName["u__gate"]
	require.NotNil(t, gate.Approval, "an approval gate declared in a template must survive inlining")
	assert.Equal(t, "ship to production?", gate.Approval.Message)
	assert.Equal(t, 45.0, gate.Approval.TimeoutMinutes)
	assert.Empty(t, gate.Run, "the approval step must not degrade into an empty run: script")

	flaky := byName["u__flaky"]
	require.NotNil(t, flaky.Retry, "retry: declared in a template must survive inlining")
	assert.Equal(t, 4, flaky.Retry.Attempts)
	assert.Equal(t, "10s", flaky.Retry.Backoff)

	fan := byName["u__fan"]
	require.NotNil(t, fan.Matrix, "matrix: declared in a template must survive inlining")
	require.Len(t, fan.Matrix.Dimensions, 1)
	assert.Equal(t, []string{"linux", "darwin"}, fan.Matrix.Dimensions[0].Source.Literal)

	each := byName["u__each"]
	require.NotNil(t, each.Foreach, "foreach: declared in a template must survive inlining")
	assert.Equal(t, "env", each.Foreach.Key)
	assert.Equal(t, []string{"dev", "prod"}, each.Foreach.Source.Literal)
}

// TestExpandUsesScopeModeKeepsRetryAndMatrix pins the scope-mode branch: it
// stamps ScopeID/ScopeImage and rejects approval:/call:/container:, but it must
// not eat the remaining fields. Matrix in particular is load-bearing there —
// the agent keys a scope environment on (ScopeID, MatrixKey), so a dropped
// matrix collapsed a multi-variant scope into a single un-expanded one.
func TestExpandUsesScopeModeKeepsRetryAndMatrix(t *testing.T) {
	tpl := dsl.Spec{Steps: []dsl.StepEntry{
		{Name: "build", Run: "./build.sh",
			Retry: &dsl.RetrySpec{Attempts: 2},
			Matrix: &dsl.MatrixDef{
				Dimensions: []dsl.MatrixDimension{{Name: "os", Source: dsl.ForeachSource{Literal: []string{"linux", "darwin"}}}},
			}},
	}}
	expanded, _, err := expandUsesStep("u", map[string]string{}, tpl, &dsl.RunsIn{Image: "golang:1.22"}, "", "")
	require.NoError(t, err)

	var build dsl.StepEntry
	for _, s := range expanded {
		if s.Name == "u__build" {
			build = s
		}
	}
	require.Equal(t, "u__build", build.Name)
	assert.Equal(t, "scope:u", build.ScopeID)
	assert.Equal(t, "golang:1.22", build.ScopeImage)
	require.NotNil(t, build.Retry)
	assert.Equal(t, 2, build.Retry.Attempts)
	require.NotNil(t, build.Matrix)
	require.Len(t, build.Matrix.Dimensions, 1)
}

// TestExpandUsesFinallyPreservesDeferredStepFields covers the second call site
// of renameInnerEntry: a template's own finally: steps are spliced into the
// caller's finally phase and must get the identical treatment. (approval: and
// post:/cache: are rejected in finally at parse time, so retry:/matrix: are the
// fields at stake here.)
func TestExpandUsesFinallyPreservesDeferredStepFields(t *testing.T) {
	tpl := dsl.Spec{
		Steps: []dsl.StepEntry{{Name: "build", Run: "./build.sh"}},
		Finally: []dsl.StepEntry{
			{Name: "cleanup", Run: "./cleanup.sh", Retry: &dsl.RetrySpec{Attempts: 3}, ContinueOnError: true},
		},
	}
	_, contrib, err := expandUsesStep("u", map[string]string{}, tpl, nil, "", "")
	require.NoError(t, err)
	require.Len(t, contrib.finally, 1)
	assert.Equal(t, "u__cleanup", contrib.finally[0].Name)
	require.NotNil(t, contrib.finally[0].Retry, "retry: on a template finally step must survive inlining")
	assert.Equal(t, 3, contrib.finally[0].Retry.Attempts)
	assert.True(t, contrib.finally[0].ContinueOnError)
}
