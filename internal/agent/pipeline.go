package agent

import (
	"context"
	"fmt"
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

// RunPipeline executes stages sequentially.
// For each stage, if it is a parallel group or a foreach step, steps run concurrently.
// A stage fails if any step fails (unless ContinueOnError is true on that step).
// When a stage fails, subsequent stages are not executed.
func RunPipeline(
	ctx context.Context,
	stages []api.ClaimStage,
	getData func() dsl.TemplateData,
	run func(ctx context.Context, step api.ClaimStep) error,
) error {
	for _, stage := range stages {
		if stage.Step != nil {
			step := *stage.Step
			if step.Foreach != nil {
				items, err := EvalForeachSource(step.Foreach.Source, getData())
				if err != nil {
					return fmt.Errorf("foreach expansion for step %q: %w", step.Name, err)
				}
				expanded := make([]api.ClaimStep, len(items))
				for i, item := range items {
					s := step
					s.Foreach = nil
					s.ForeachKey = step.Foreach.Key
					s.ForeachValue = item
					expanded[i] = s
				}
				if err := runParallel(ctx, expanded, run); err != nil {
					return err
				}
			} else {
				if err := runOne(ctx, step, run); err != nil {
					return err
				}
			}
		} else {
			if err := runParallel(ctx, stage.Parallel, run); err != nil {
				return err
			}
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

