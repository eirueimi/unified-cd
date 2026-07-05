package agent

import (
	"context"
	"sync"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

// safeStepCtx protects a dsl.TemplateData with a sync.RWMutex.
// Allows goroutines running in parallel to safely read and write step outputs.
type safeStepCtx struct {
	mu   sync.RWMutex
	data dsl.TemplateData
}

// snapshot returns a copy of the current template data (for template expansion).
func (s *safeStepCtx) snapshot() dsl.TemplateData {
	s.mu.RLock()
	defer s.mu.RUnlock()
	steps := make(map[string]dsl.StepData, len(s.data.Steps))
	for k, v := range s.data.Steps {
		steps[k] = v
	}
	params := make(map[string]string, len(s.data.Params))
	for k, v := range s.data.Params {
		params[k] = v
	}
	secr := make(map[string]string, len(s.data.Secrets))
	for k, v := range s.data.Secrets {
		secr[k] = v
	}
	return dsl.TemplateData{
		Params:  params,
		Steps:   steps,
		Secrets: secr,
	}
}

// setStep writes the outputs for a step.
func (s *safeStepCtx) setStep(name string, data dsl.StepData) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.Steps == nil {
		s.data.Steps = make(map[string]dsl.StepData)
	}
	s.data.Steps[name] = data
}

// setStepMatrixOutputs merges one matrix copy's outputs into the aggregated
// per-combination map under the base step name. Inner maps are rebuilt on
// every write so snapshots never observe concurrent mutation.
func (s *safeStepCtx) setStepMatrixOutputs(name, comboKey string, outputs map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.Steps == nil {
		s.data.Steps = make(map[string]dsl.StepData)
	}
	sd := s.data.Steps[name]
	if sd.Outputs == nil {
		sd.Outputs = map[string]any{}
	}
	for k, v := range outputs {
		merged := map[string]string{comboKey: v}
		if prev, ok := sd.Outputs[k].(map[string]string); ok {
			for pk, pv := range prev {
				merged[pk] = pv
			}
			merged[comboKey] = v
		}
		sd.Outputs[k] = merged
	}
	s.data.Steps[name] = sd
}

// RunPipeline executes stages sequentially. Within a stage, matrix expansion
// applies to the single step and to every member of a parallel group; all
// resulting copies run concurrently. When a stage fails, subsequent stages are
// not executed.
func RunPipeline(
	ctx context.Context,
	stages []api.ClaimStage,
	getData func() dsl.TemplateData,
	maxCombos int,
	run func(ctx context.Context, step api.ClaimStep) error,
) error {
	for _, stage := range stages {
		members := stage.Parallel
		if stage.Step != nil {
			members = []api.ClaimStep{*stage.Step}
		}
		var expanded []api.ClaimStep
		for _, st := range members {
			ex, err := ExpandMatrixStep(st, getData(), maxCombos)
			if err != nil {
				return err
			}
			expanded = append(expanded, ex...)
		}
		// Preserve the historical single-step path (no goroutine) for a plain
		// non-matrix single-step stage.
		if stage.Step != nil && stage.Step.Matrix == nil {
			if err := runOne(ctx, expanded[0], run); err != nil {
				return err
			}
			continue
		}
		if err := runParallel(ctx, expanded, run); err != nil {
			return err
		}
	}
	return nil
}

// runParallel starts all steps concurrently and waits for all to finish.
// Returns a combined error if any step fails (respecting ContinueOnError).
func runParallel(ctx context.Context, steps []api.ClaimStep, run func(context.Context, api.ClaimStep) error) error {
	var wg sync.WaitGroup
	errs := make([]error, len(steps))
	for i, step := range steps {
		wg.Add(1)
		go func(idx int, s api.ClaimStep) {
			defer wg.Done()
			errs[idx] = runOne(ctx, s, run)
		}(i, step)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// runOne calls run for a single step, suppressing the error when ContinueOnError is set.
func runOne(ctx context.Context, step api.ClaimStep, run func(context.Context, api.ClaimStep) error) error {
	err := run(ctx, step)
	if err != nil && step.ContinueOnError {
		return nil
	}
	return err
}

