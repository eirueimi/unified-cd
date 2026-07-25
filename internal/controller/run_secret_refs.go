package controller

import (
	"encoding/json"
	"fmt"

	"github.com/eirueimi/unified-cd/internal/dsl"
)

func prepareRunSpec(spec dsl.Spec, params map[string]string) ([]byte, error) {
	spec = cloneRunSpecExecutableFields(spec)
	if err := dsl.ResolveSecretNameParamsInSpec(&spec, params); err != nil {
		return nil, fmt.Errorf("resolve secret name parameters: %w", err)
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("marshal resolved run spec: %w", err)
	}
	return raw, nil
}

func cloneRunSpecExecutableFields(spec dsl.Spec) dsl.Spec {
	spec.Steps = cloneRunSpecEntries(spec.Steps)
	spec.Finally = cloneRunSpecEntries(spec.Finally)
	return spec
}

func cloneRunSpecEntries(entries []dsl.StepEntry) []dsl.StepEntry {
	if entries == nil {
		return nil
	}
	cloned := make([]dsl.StepEntry, len(entries))
	for i, entry := range entries {
		clonedEntry := entry
		clonedEntry.Env = cloneRunSpecEnv(entry.Env)
		if entry.Parallel != nil {
			clonedEntry.Parallel = make([]dsl.Step, len(entry.Parallel))
			for j, step := range entry.Parallel {
				clonedStep := step
				clonedStep.Env = cloneRunSpecEnv(step.Env)
				clonedEntry.Parallel[j] = clonedStep
			}
		}
		cloned[i] = clonedEntry
	}
	return cloned
}

func cloneRunSpecEnv(env map[string]string) map[string]string {
	if env == nil {
		return nil
	}
	cloned := make(map[string]string, len(env))
	for key, value := range env {
		cloned[key] = value
	}
	return cloned
}
