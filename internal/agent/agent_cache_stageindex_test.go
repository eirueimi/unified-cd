package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A cache step that is not the first step must report its own StageIndex, not
// the int zero value. Otherwise the run-detail UI groups it with stage 0 and
// renders it as a spurious "parallel" group.
func TestExecuteCacheStep_ReportsStageIndex(t *testing.T) {
	var mu sync.Mutex
	var reports []api.StepReportRequest
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/a1/steps", func(w http.ResponseWriter, r *http.Request) {
		var req api.StepReportRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		reports = append(reports, req)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := &Agent{ID: "a1", Client: NewClient(srv.URL, "tok")} // CacheStore nil: restore/save skipped, reporting still happens

	// index 1, stage 1 (like restore-cache after checkout)
	step := api.ClaimStep{Index: 1, StageIndex: 1, Name: "restore-cache", Cache: &dsl.CacheStep{Path: "p", Key: "k"}}
	sctx := &safeStepCtx{data: dsl.TemplateData{}}

	var postHooksMu sync.Mutex
	var postHooks []func(context.Context)
	require.NoError(t, a.executeCacheStep(context.Background(), step, "r1", sctx, &postHooksMu, &postHooks, nil, crt.ContainerHandle{}))

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, reports, "cache step should report at least once")
	for _, rep := range reports {
		assert.Equal(t, 1, rep.StageIndex, "cache step report %q must carry StageIndex=1, not the zero value", rep.Status)
	}
}
