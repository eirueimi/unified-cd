package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

func emptyData() dsl.TemplateData {
	return dsl.TemplateData{Params: map[string]string{}, Steps: map[string]dsl.StepData{}}
}

func TestRunPipeline_Sequential(t *testing.T) {
	stages := []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "a", Run: "echo a"}},
		{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "b", Run: "echo b"}},
	}
	var order []string
	var mu sync.Mutex
	err := RunPipeline(t.Context(), stages, emptyData, 0,
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
	err := RunPipeline(t.Context(), stages, emptyData, 0,
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
	err := RunPipeline(t.Context(), stages, emptyData, 0,
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
	err := RunPipeline(t.Context(), stages, emptyData, 0,
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

func TestRunPipeline_MatrixExpansion(t *testing.T) {
	stages := []api.ClaimStage{{Step: &api.ClaimStep{
		Name: "build",
		Matrix: &api.ClaimMatrixDef{Dimensions: []api.ClaimMatrixDimension{
			{Name: "os", Source: api.ClaimForeachSource{Literal: []string{"linux", "windows"}}},
			{Name: "arch", Source: api.ClaimForeachSource{Literal: []string{"amd64", "arm64"}}},
		}},
	}}}
	var mu sync.Mutex
	var keys []string
	err := RunPipeline(t.Context(), stages, emptyData, 0, func(_ context.Context, s api.ClaimStep) error {
		mu.Lock()
		defer mu.Unlock()
		keys = append(keys, s.MatrixKey)
		require.Equal(t, s.MatrixValues["os"]+"/"+s.MatrixValues["arch"], s.MatrixKey)
		return nil
	})
	require.NoError(t, err)
	sort.Strings(keys)
	require.Equal(t, []string{"linux/amd64", "linux/arm64", "windows/amd64", "windows/arm64"}, keys)
}

func TestRunPipeline_MatrixInsideParallelExpands(t *testing.T) {
	// 従来バグ: parallel 内の foreach/matrix が展開されず1回だけ実行されていた
	stages := []api.ClaimStage{{Parallel: []api.ClaimStep{
		{Name: "plain", Run: "echo"},
		{Name: "fanout", Matrix: &api.ClaimMatrixDef{Dimensions: []api.ClaimMatrixDimension{
			{Name: "env", Source: api.ClaimForeachSource{Literal: []string{"dev", "stg", "prod"}}},
		}}},
	}}}
	var mu sync.Mutex
	count := map[string]int{}
	err := RunPipeline(t.Context(), stages, emptyData, 0, func(_ context.Context, s api.ClaimStep) error {
		mu.Lock()
		defer mu.Unlock()
		count[s.Name]++
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, count["plain"])
	require.Equal(t, 3, count["fanout"])
}

func TestRunPipeline_MatrixCapFailsRun(t *testing.T) {
	stages := []api.ClaimStage{{Step: &api.ClaimStep{
		Name: "big",
		Matrix: &api.ClaimMatrixDef{Dimensions: []api.ClaimMatrixDimension{
			{Name: "a", Source: api.ClaimForeachSource{Literal: []string{"1", "2", "3"}}},
		}},
	}}}
	err := RunPipeline(t.Context(), stages, emptyData, 2, func(_ context.Context, _ api.ClaimStep) error { return nil })
	require.ErrorContains(t, err, "exceed")
}

func TestRunPipeline_MatrixExprParam(t *testing.T) {
	envs, _ := json.Marshal([]string{"a", "b", "c"})
	stages := []api.ClaimStage{
		{Step: &api.ClaimStep{
			Index: 0, StageIndex: 0, Name: "deploy",
			Matrix: &api.ClaimMatrixDef{Dimensions: []api.ClaimMatrixDimension{
				{Name: "env", Source: api.ClaimForeachSource{Expr: "$envs"}},
			}},
			Run: "echo {{ .Matrix.env }}",
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
		0,
		func(_ context.Context, s api.ClaimStep) error {
			mu.Lock()
			seen = append(seen, s.MatrixValues["env"])
			mu.Unlock()
			return nil
		})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"a", "b", "c"}, seen)
}

func TestSafeStepCtx_MatrixOutputAggregation(t *testing.T) {
	sctx := &safeStepCtx{data: dsl.TemplateData{Steps: map[string]dsl.StepData{}}}
	sctx.setStepMatrixOutputs("build", "linux/amd64", map[string]string{"version": "1.2"})
	sctx.setStepMatrixOutputs("build", "linux/arm64", map[string]string{"version": "1.3"})
	snap := sctx.snapshot()
	agg, ok := snap.Steps["build"].Outputs["version"].(map[string]string)
	require.True(t, ok)
	require.Equal(t, map[string]string{"linux/amd64": "1.2", "linux/arm64": "1.3"}, agg)
}
