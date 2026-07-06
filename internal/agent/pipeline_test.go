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
	err := RunPipeline(t.Context(), stages, emptyData, 0, Concurrent,
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
	err := RunPipeline(t.Context(), stages, emptyData, 0, Concurrent,
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
	err := RunPipeline(t.Context(), stages, emptyData, 0, Concurrent,
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
	err := RunPipeline(t.Context(), stages, emptyData, 0, Concurrent,
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
	err := RunPipeline(t.Context(), stages, emptyData, 0, Concurrent, func(_ context.Context, s api.ClaimStep) error {
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
	// Former bug: foreach/matrix inside parallel was not expanded and ran only once
	stages := []api.ClaimStage{{Parallel: []api.ClaimStep{
		{Name: "plain", Run: "echo"},
		{Name: "fanout", Matrix: &api.ClaimMatrixDef{Dimensions: []api.ClaimMatrixDimension{
			{Name: "env", Source: api.ClaimForeachSource{Literal: []string{"dev", "stg", "prod"}}},
		}}},
	}}}
	var mu sync.Mutex
	count := map[string]int{}
	err := RunPipeline(t.Context(), stages, emptyData, 0, Concurrent, func(_ context.Context, s api.ClaimStep) error {
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
	err := RunPipeline(t.Context(), stages, emptyData, 2, Concurrent, func(_ context.Context, _ api.ClaimStep) error { return nil })
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
		Concurrent,
		func(_ context.Context, s api.ClaimStep) error {
			mu.Lock()
			seen = append(seen, s.MatrixValues["env"])
			mu.Unlock()
			return nil
		})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"a", "b", "c"}, seen)
}

// TestRunPipeline_SequentialRunsMembersOneAtATime asserts that Sequential mode
// runs parallel-group members one at a time (max-in-flight == 1) and in
// declaration order.
func TestRunPipeline_SequentialRunsMembersOneAtATime(t *testing.T) {
	stages := []api.ClaimStage{
		{Parallel: []api.ClaimStep{
			{Index: 0, StageIndex: 0, Name: "a", Run: "echo a"},
			{Index: 1, StageIndex: 0, Name: "b", Run: "echo b"},
			{Index: 2, StageIndex: 0, Name: "c", Run: "echo c"},
		}},
	}
	var inFlight int32
	var maxInFlight int32
	var mu sync.Mutex
	var order []string
	err := RunPipeline(t.Context(), stages, emptyData, 0, Sequential,
		func(_ context.Context, s api.ClaimStep) error {
			cur := atomic.AddInt32(&inFlight, 1)
			for {
				old := atomic.LoadInt32(&maxInFlight)
				if cur <= old || atomic.CompareAndSwapInt32(&maxInFlight, old, cur) {
					break
				}
			}
			mu.Lock()
			order = append(order, s.Name)
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			atomic.AddInt32(&inFlight, -1)
			return nil
		})
	require.NoError(t, err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&maxInFlight))
	assert.Equal(t, []string{"a", "b", "c"}, order)
}

// TestRunPipeline_ConcurrentRunsMembersTogether locks in the existing
// (Concurrent) behavior: all members of a parallel group are in flight at
// once. Uses a barrier channel so the assertion doesn't depend on timing —
// each worker blocks until all N have arrived, with a deadlock-timeout guard
// so a regression to sequential execution fails fast instead of hanging.
func TestRunPipeline_ConcurrentRunsMembersTogether(t *testing.T) {
	const n = 3
	stages := []api.ClaimStage{
		{Parallel: []api.ClaimStep{
			{Index: 0, StageIndex: 0, Name: "a", Run: "echo a"},
			{Index: 1, StageIndex: 0, Name: "b", Run: "echo b"},
			{Index: 2, StageIndex: 0, Name: "c", Run: "echo c"},
		}},
	}
	var inFlight int32
	var maxInFlight int32
	arrived := make(chan struct{}, n)
	release := make(chan struct{})

	done := make(chan error, 1)
	go func() {
		done <- RunPipeline(t.Context(), stages, emptyData, 0, Concurrent,
			func(_ context.Context, s api.ClaimStep) error {
				cur := atomic.AddInt32(&inFlight, 1)
				for {
					old := atomic.LoadInt32(&maxInFlight)
					if cur <= old || atomic.CompareAndSwapInt32(&maxInFlight, old, cur) {
						break
					}
				}
				arrived <- struct{}{}
				<-release
				atomic.AddInt32(&inFlight, -1)
				return nil
			})
	}()

	// Wait for all n members to be in flight simultaneously, with a
	// deadlock-timeout guard in case a regression serializes execution.
	for i := 0; i < n; i++ {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for all %d members to be in flight (deadlock guard)", n)
		}
	}
	close(release)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("RunPipeline did not return (deadlock guard)")
	}
	assert.Equal(t, int32(n), atomic.LoadInt32(&maxInFlight))
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

// TestSafeStepCtx_ConcurrentSetStepMatrixOutputsAndSnapshot is a regression
// test for review finding I3: setStepMatrixOutputs used to rebuild only the
// inner per-key combo map, writing it back into the *existing* outer
// s.data.Steps map and the existing per-step Outputs map that a concurrent
// snapshot() may have already copied a reference to (snapshot only copies one
// level deep). That's a genuine data race: one goroutine mutates a map in
// place while another iterates/reads the same map instance.
//
// This test hammers setStepMatrixOutputs and snapshot concurrently across
// many goroutines and combos. Without `go test -race` it cannot prove the
// race is gone by itself — a racy map read/write does not reliably panic or
// produce a visibly wrong value under `go test` alone — but it documents the
// intended concurrent-access contract and will reliably crash
// ("concurrent map read and map write" / "concurrent map writes") under the
// old implementation if run enough iterations, and is exactly the shape
// `-race` should be pointed at in CI (this Windows dev box has no gcc, so
// `-race` cannot run locally here — see report).
func TestSafeStepCtx_ConcurrentSetStepMatrixOutputsAndSnapshot(t *testing.T) {
	sctx := &safeStepCtx{data: dsl.TemplateData{Steps: map[string]dsl.StepData{}}}

	const goroutines = 8
	const itersPerGoroutine = 200

	var wg sync.WaitGroup
	wg.Add(goroutines + 1)

	// Writers: concurrently merge outputs for different combos of the same step.
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < itersPerGoroutine; i++ {
				combo := "combo-" + string(rune('a'+g))
				sctx.setStepMatrixOutputs("build", combo, map[string]string{
					"version": string(rune('0' + (i % 10))),
				})
			}
		}(g)
	}

	// Reader: concurrently snapshots and reads through the aggregated map,
	// exercising exactly the access pattern that used to race.
	go func() {
		defer wg.Done()
		for i := 0; i < itersPerGoroutine; i++ {
			snap := sctx.snapshot()
			if sd, ok := snap.Steps["build"]; ok {
				if agg, ok := sd.Outputs["version"].(map[string]string); ok {
					for range agg {
						// touch every entry to force a full map read
					}
				}
			}
		}
	}()

	wg.Wait()

	snap := sctx.snapshot()
	agg, ok := snap.Steps["build"].Outputs["version"].(map[string]string)
	require.True(t, ok)
	require.Len(t, agg, goroutines, "expected one aggregated combo per writer goroutine")
}
