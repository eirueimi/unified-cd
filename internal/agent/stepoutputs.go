package agent

import "github.com/eirueimi/unified-cd/internal/dsl"

// ApplyStepOutputs records a step's outputs into steps under stepName.
//
// If matrixKey is empty, it replaces steps[stepName] outright with a fresh
// StepData wrapping outputs (dsl.StringOutputs semantics).
//
// If matrixKey is non-empty, it merges this matrix copy's outputs into the
// aggregated per-combination map under stepName, using copy-on-write: every
// map at every level — the outer steps map, the per-step Outputs map, and
// each per-key combo map — is rebuilt fresh on every call and only assigned
// back into steps at the end. This mirrors the host's
// safeStepCtx.setStepMatrixOutputs so that any previously taken snapshot of
// steps (or of a StepData/Outputs value within it) is never mutated after
// being published.
//
// This is a pure function: it does not lock. Callers that share steps across
// goroutines must hold their own lock (see safeStepCtx in pipeline.go); the
// k8s agent runs steps sequentially and needs no lock.
func ApplyStepOutputs(steps map[string]dsl.StepData, stepName, matrixKey string, outputs map[string]string) {
	if matrixKey == "" {
		steps[stepName] = dsl.StepData{Outputs: dsl.StringOutputs(outputs)}
		return
	}

	sd := steps[stepName]
	newOutputs := make(map[string]any, len(sd.Outputs))
	for k, v := range sd.Outputs {
		newOutputs[k] = v
	}
	for k, v := range outputs {
		merged := map[string]string{matrixKey: v}
		if prev, ok := newOutputs[k].(map[string]string); ok {
			for pk, pv := range prev {
				merged[pk] = pv
			}
			merged[matrixKey] = v
		}
		newOutputs[k] = merged
	}
	sd.Outputs = newOutputs
	steps[stepName] = sd
}
