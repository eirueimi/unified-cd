package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/metrics"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMetricsEndToEndWiring exercises the SAME constructor main.go calls.
//
// It used to re-perform main.go's registration calls itself, under a comment
// saying it mirrored them. That made the test unable to fail for the defect it
// exists to catch: deleting a registration from main.go left it green, and
// exactly that happened — the controller shipped with no Go runtime, process,
// or connection-pool metrics at all while this test passed.
//
// The family list below is therefore the contract: a collector that stops
// being registered stops appearing here, whether it was dropped from
// NewForController or from main.go's single call to it.
func TestMetricsEndToEndWiring(t *testing.T) {
	pg := store.NewTestPostgres(t)
	m := metrics.NewForController("v-test", pg, 90*time.Second)
	t.Cleanup(func() { metrics.SetBackgroundRecorder(nil) })
	st := metrics.NewInstrumentedStore(pg, m)

	_, err := pg.UpsertBootstrapPAT(context.Background(), "test-bootstrap", HashToken("secret"))
	require.NoError(t, err)
	s := NewServer(Config{}, st)
	s.SetMetrics(m)

	_, err = pg.UpsertJob(context.Background(), "wiring-job", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)

	run, err := st.CreateRun(context.Background(), "wiring-job", nil, []byte(`{}`), nil, nil, "api", "")
	require.NoError(t, err)
	require.NoError(t, st.MarkRunFinished(context.Background(), run.ID, api.RunFailed))

	// A background pass through the recorder NewForController wired. Nothing
	// in main.go wires this separately any more, so if the constructor stops
	// doing it, these series vanish.
	metrics.ObservePass("wiring_probe", func() (int, int, error) { return 2, 1, nil })

	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()

	for _, want := range []string{
		// Store decorator.
		`unifiedcd_runs_created_total{trigger="api"} 1`,
		`unifiedcd_runs_finished_total{status="Failed"} 1`,
		// DB-backed scrape-time collector.
		`unifiedcd_runs_current{status="Pending"} 0`,
		`unifiedcd_agents{state="alive"} 0`,
		// Build info, from the version passed to the constructor.
		`unifiedcd_build_info{version="v-test"} 1`,
		// Connection pools — all four, so dropping one from PoolStats fails here.
		`unifiedcd_db_pool_max_connections{pool="api"}`,
		`unifiedcd_db_pool_max_connections{pool="background"}`,
		`unifiedcd_db_pool_max_connections{pool="lock"}`,
		`unifiedcd_db_pool_max_connections{pool="listen"}`,
		`unifiedcd_db_pool_empty_acquires_total{pool="api"}`,
		// Background worker recorder.
		`unifiedcd_background_task_items_total{result="ok",task="wiring_probe"} 2`,
		`unifiedcd_background_task_items_total{result="error",task="wiring_probe"} 1`,
		// Go and process collectors, which a private registry does NOT
		// install for you. Their absence was invisible for exactly this
		// reason: no test asserted the endpoint served them.
		"\ngo_goroutines ",
		"\nprocess_cpu_seconds_total ",
	} {
		assert.True(t, strings.Contains(body, want), "missing %q", want)
	}
}
