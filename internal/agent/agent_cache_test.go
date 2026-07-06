package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/cache"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/objectstore"
)

// newCacheTestAgent returns an Agent whose Client points at a stub controller
// that accepts step reports, backed by a LocalObjectStore cache.
func newCacheTestAgent(t *testing.T) *Agent {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/a1/steps", stepHandler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Agent{
		ID:         "a1",
		Client:     NewClient(srv.URL, "tok"),
		CacheStore: objectstore.NewLocalObjectStore(t.TempDir()),
	}
}

func cacheClaimStep(cs *dsl.CacheStep) api.ClaimStep {
	return api.ClaimStep{Index: 0, Name: "cache-step", Cache: cs}
}

func TestExecuteCacheStep_PathTemplateExpandedOnRestore(t *testing.T) {
	ctx := context.Background()
	a := newCacheTestAgent(t)
	// If path expansion regresses, restore would extract into a literal
	// "{{ .Params.dir }}" directory under the test cwd; remove it defensively.
	t.Cleanup(func() { _ = os.RemoveAll("{{ .Params.dir }}") })

	// Seed the store under the key the step's key template expands to.
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "f.txt"), []byte("cached"), 0o644))
	require.NoError(t, cache.Save(ctx, a.CacheStore, src, "k-1", 7))

	dest := t.TempDir()
	sctx := &safeStepCtx{data: dsl.TemplateData{Params: map[string]string{"dir": dest, "v": "1"}}}
	step := cacheClaimStep(&dsl.CacheStep{Path: "{{ .Params.dir }}", Key: "k-{{ .Params.v }}"})

	var postHooksMu sync.Mutex
	var postHooks []func(context.Context)
	require.NoError(t, executeCacheStep(ctx, a.Client, a.ID, step, "r1", sctx, &postHooksMu, &postHooks, newHostBackend(a, "r1", ""), ScopeHandle{}))

	got, err := os.ReadFile(filepath.Join(dest, "f.txt"))
	require.NoError(t, err, "cache should restore into the template-expanded path")
	assert.Equal(t, "cached", string(got))
}

func TestExecuteCacheStep_PathTemplateExpandedOnDeferredSave(t *testing.T) {
	ctx := context.Background()
	a := newCacheTestAgent(t)

	dest := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dest, "built.txt"), []byte("artifact"), 0o644))
	sctx := &safeStepCtx{data: dsl.TemplateData{Params: map[string]string{"dir": dest}}}
	step := cacheClaimStep(&dsl.CacheStep{Path: "{{ .Params.dir }}", Key: "save-key"})

	var postHooksMu sync.Mutex
	var postHooks []func(context.Context)
	require.NoError(t, executeCacheStep(ctx, a.Client, a.ID, step, "r1", sctx, &postHooksMu, &postHooks, newHostBackend(a, "r1", ""), ScopeHandle{}))
	require.Len(t, postHooks, 1)
	postHooks[0](ctx)

	restored := t.TempDir()
	hit, err := cache.Restore(ctx, a.CacheStore, restored, "save-key", nil)
	require.NoError(t, err)
	require.True(t, hit, "deferred save should archive the template-expanded path")
	got, err := os.ReadFile(filepath.Join(restored, "built.txt"))
	require.NoError(t, err)
	assert.Equal(t, "artifact", string(got))
}

func TestExecuteCacheStep_PathTemplateParseErrorFailsStep(t *testing.T) {
	a := newCacheTestAgent(t)
	sctx := &safeStepCtx{data: dsl.TemplateData{}}
	step := cacheClaimStep(&dsl.CacheStep{Path: "{{ .Params.dir", Key: "k"})

	var postHooksMu sync.Mutex
	var postHooks []func(context.Context)
	err := executeCacheStep(context.Background(), a.Client, a.ID, step, "r1", sctx, &postHooksMu, &postHooks, newHostBackend(a, "r1", ""), ScopeHandle{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cache path")
	assert.Empty(t, postHooks, "no save should be registered when the path template is invalid")
}

func TestExecuteCacheStep_EmptyExpandedPathSkipsCache(t *testing.T) {
	a := newCacheTestAgent(t)
	sctx := &safeStepCtx{data: dsl.TemplateData{Params: map[string]string{}}}
	step := cacheClaimStep(&dsl.CacheStep{Path: "{{ .Params.missing }}", Key: "k"})

	var postHooksMu sync.Mutex
	var postHooks []func(context.Context)
	err := executeCacheStep(context.Background(), a.Client, a.ID, step, "r1", sctx, &postHooksMu, &postHooks, newHostBackend(a, "r1", ""), ScopeHandle{})
	require.NoError(t, err, "an empty expanded path is warn+skip, not a step failure")
	assert.Empty(t, postHooks, "no save should be registered when the path expands to empty")
}

func TestExecuteCacheStep_EmptyExpandedKeySkipsCache(t *testing.T) {
	a := newCacheTestAgent(t)
	sctx := &safeStepCtx{data: dsl.TemplateData{Params: map[string]string{}}}
	step := cacheClaimStep(&dsl.CacheStep{Path: "/tmp/some-dir", Key: "{{ .Params.missing }}"})

	var postHooksMu sync.Mutex
	var postHooks []func(context.Context)
	err := executeCacheStep(context.Background(), a.Client, a.ID, step, "r1", sctx, &postHooksMu, &postHooks, newHostBackend(a, "r1", ""), ScopeHandle{})
	require.NoError(t, err, "an empty expanded key is warn+skip, not a step failure")
	assert.Empty(t, postHooks, "no save should be registered when the key expands to empty")
}
