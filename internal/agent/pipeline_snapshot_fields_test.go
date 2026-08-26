package agent

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/dsl"
)

// templateDataFieldPolicy records what safeStepCtx.snapshot() does with one
// field of dsl.TemplateData.
//
// This exists because Vars was added to TemplateData for the vars feature and
// had to be added to snapshot()'s hand-written copy by hand to reach an if:
// condition or a step's template data at all — the same class of silent drop
// that once cost this codebase an approval gate when uses:-inlining rebuilt a
// struct as a fresh literal instead of copying it field-by-field (see
// internal/gittemplate/inline_fields_test.go, the pattern this test copies).
// A field added to TemplateData without a policy recorded here fails a test
// instead of silently reading zero from every step's {{ .X }} template and
// every if: condition for the rest of the run.
type templateDataFieldPolicy int

const (
	// fieldSnapshotted: part of safeStepCtx's persistent run state.
	// snapshot() must carry a deep copy of the field through.
	fieldSnapshotted templateDataFieldPolicy = iota
	// fieldPerStep: NOT part of safeStepCtx's persistent state. The
	// orchestrator sets it on the dsl.TemplateData returned BY snapshot(),
	// scoped to one step's own execution (Matrix/Foreach dimension values,
	// captured Stdout for an output template) — see orchestrator.go's
	// `tplData := sctx.snapshot(); tplData.Matrix = ...`. snapshot() must
	// leave it zero; there is nothing in safeStepCtx to copy it from.
	fieldPerStep
)

// templateDataFieldPolicies covers every field of dsl.TemplateData.
var templateDataFieldPolicies = map[string]templateDataFieldPolicy{
	"Params":  fieldSnapshotted,
	"Vars":    fieldSnapshotted,
	"Steps":   fieldSnapshotted,
	"Secrets": fieldSnapshotted,
	"Stdout":  fieldPerStep,
	"Foreach": fieldPerStep,
	"Matrix":  fieldPerStep,
}

// TestSafeStepCtxSnapshot_EveryTemplateDataFieldHasAPolicy is the coverage
// half of the guard: a field added to dsl.TemplateData with no entry here
// fails immediately, forcing an explicit decision about whether
// safeStepCtx.snapshot() needs to copy it.
func TestSafeStepCtxSnapshot_EveryTemplateDataFieldHasAPolicy(t *testing.T) {
	typ := reflect.TypeOf(dsl.TemplateData{})
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		_, ok := templateDataFieldPolicies[name]
		require.Truef(t, ok,
			"dsl.TemplateData.%s has no recorded snapshot() policy: add one to "+
				"templateDataFieldPolicies in internal/agent/pipeline_snapshot_fields_test.go, "+
				"and update safeStepCtx.snapshot() to copy it if it is part of the run's "+
				"persistent state", name)
	}
}

// TestSafeStepCtxSnapshot_CarriesEverySnapshottedField is the carriage half:
// every field marked fieldSnapshotted must actually come out of snapshot()
// non-zero when the live context has it set, and must be an independent copy
// rather than a shared map (mutating the live context afterwards must not
// change a snapshot already taken).
func TestSafeStepCtxSnapshot_CarriesEverySnapshottedField(t *testing.T) {
	sctx := &safeStepCtx{data: dsl.TemplateData{
		Params:  map[string]string{"p": "1"},
		Vars:    map[string]string{"v": "1"},
		Steps:   map[string]dsl.StepData{"build": {Outputs: map[string]any{"o": "1"}}},
		Secrets: map[string]string{"s": "1"},
	}}
	snap := sctx.snapshot()

	typ := reflect.TypeOf(dsl.TemplateData{})
	v := reflect.ValueOf(snap)
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if templateDataFieldPolicies[name] == fieldSnapshotted {
			assert.Falsef(t, v.Field(i).IsZero(),
				"snapshot() left %s zero though the live context had it set", name)
		}
	}

	// Independence: snapshot() must copy each map, not alias it, so a write
	// to the live context after the snapshot was taken (the normal case —
	// other steps keep running concurrently) cannot retroactively change a
	// template already rendered from this snapshot.
	sctx.data.Params["p"] = "2"
	sctx.data.Vars["v"] = "2"
	sctx.data.Secrets["s"] = "2"
	assert.Equal(t, "1", snap.Params["p"], "snapshot must not share the live Params map")
	assert.Equal(t, "1", snap.Vars["v"], "snapshot must not share the live Vars map")
	assert.Equal(t, "1", snap.Secrets["s"], "snapshot must not share the live Secrets map")
}
