package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

func getData() dsl.TemplateData {
	return dsl.TemplateData{Params: map[string]string{}, Steps: map[string]dsl.StepData{}}
}

func TestRunPipeline_Sequential(t *testing.T) {
	stages := []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "a", Run: "echo a"}},
		{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "b", Run: "echo b"}},
	}
	var order []string
	var mu sync.Mutex
	err := RunPipeline(t.Context(), stages, func() dsl.TemplateData { return getData() },
		func(_ context.Context, s api.ClaimStep) error {
			mu.Lock()
			order = append(order, s.Name)
			mu.Unlock()
			return nil
		})
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, order)
}

func TestRunPipeline_ParallelGroup(t *testing.T) {
	stages := []api.ClaimStage{
		{Parallel: []api.ClaimStep{
			{Index: 0, StageIndex: 0, Name: "a", Run: "echo a"},
			{Index: 1, StageIndex: 0, Name: "b", Run: "echo b"},
		}},
	}
	var mu sync.Mutex
	running := 0
	maxConcurrent := 0
	err := RunPipeline(t.Context(), stages, func() dsl.TemplateData { return getData() },
		func(_ context.Context, s api.ClaimStep) error {
			mu.Lock()
			running++
			if running > maxConcurrent {
				maxConcurrent = running
			}
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			mu.Lock()
			running--
			mu.Unlock()
			return nil
		})
	require.NoError(t, err)
	assert.Equal(t, 2, maxConcurrent)
}

func TestRunPipeline_ParallelGroupFailure(t *testing.T) {
	stages := []api.ClaimStage{
		{Parallel: []api.ClaimStep{
			{Index: 0, StageIndex: 0, Name: "ok", Run: "echo ok"},
			{Index: 1, StageIndex: 0, Name: "fail", Run: "exit 1"},
		}},
		{Step: &api.ClaimStep{Index: 2, StageIndex: 1, Name: "after", Run: "echo after"}},
	}
	var afterRan atomic.Bool
	err := RunPipeline(t.Context(), stages, func() dsl.TemplateData { return getData() },
		func(_ context.Context, s api.ClaimStep) error {
			if s.Name == "fail" {
				return errors.New("step failed")
			}
			if s.Name == "after" {
				afterRan.Store(true)
			}
			return nil
		})
	require.Error(t, err)
	assert.False(t, afterRan.Load(), "stage after a failed parallel group must not run")
}

func TestRunPipeline_ContinueOnError(t *testing.T) {
	stages := []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "flaky", Run: "exit 1", ContinueOnError: true}},
		{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "after", Run: "echo after"}},
	}
	var afterRan atomic.Bool
	err := RunPipeline(t.Context(), stages, func() dsl.TemplateData { return getData() },
		func(_ context.Context, s api.ClaimStep) error {
			if s.Name == "flaky" {
				return errors.New("flaky failed")
			}
			afterRan.Store(true)
			return nil
		})
	require.NoError(t, err)
	assert.True(t, afterRan.Load())
}

func TestRunPipeline_ForeachLiteral(t *testing.T) {
	stages := []api.ClaimStage{
		{Step: &api.ClaimStep{
			Index: 0, StageIndex: 0, Name: "deploy",
			Foreach: &api.ClaimForeachDef{
				Key:    "env",
				Source: api.ClaimForeachSource{Literal: []string{"prod", "staging"}},
			},
			Run: "echo {{ .Foreach.env }}",
		}},
	}
	var mu sync.Mutex
	var seen []string
	err := RunPipeline(t.Context(), stages, func() dsl.TemplateData { return getData() },
		func(_ context.Context, s api.ClaimStep) error {
			mu.Lock()
			seen = append(seen, s.ForeachValue)
			mu.Unlock()
			return nil
		})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"prod", "staging"}, seen)
}

func TestRunPipeline_ForeachExprParam(t *testing.T) {
	envs, _ := json.Marshal([]string{"a", "b", "c"})
	stages := []api.ClaimStage{
		{Step: &api.ClaimStep{
			Index: 0, StageIndex: 0, Name: "deploy",
			Foreach: &api.ClaimForeachDef{
				Key:    "env",
				Source: api.ClaimForeachSource{Expr: "$envs"},
			},
			Run: "echo {{ .Foreach.env }}",
		}},
	}
	var mu sync.Mutex
	var seen []string
	err := RunPipeline(t.Context(), stages,
		func() dsl.TemplateData {
			return dsl.TemplateData{
				Params: map[string]string{"envs": string(envs)},
				Steps:  map[string]dsl.StepData{},
			}
		},
		func(_ context.Context, s api.ClaimStep) error {
			mu.Lock()
			seen = append(seen, s.ForeachValue)
			mu.Unlock()
			return nil
		})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"a", "b", "c"}, seen)
}
