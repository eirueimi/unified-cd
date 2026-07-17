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

// Mirrors the production main.go wiring: collector + decorator + endpoint.
func TestMetricsEndToEndWiring(t *testing.T) {
	pg := store.NewTestPostgres(t)
	m := metrics.New()
	m.RegisterDBCollector(pg, 90*time.Second)
	st := metrics.NewInstrumentedStore(pg, m)

	_, err := pg.UpsertBootstrapPAT(context.Background(), "test-bootstrap", HashToken("secret"))
	require.NoError(t, err)
	s := NewServer(Config{LegacyAgentToken: "agent-secret"}, st)
	s.SetMetrics(m)

	_, err = pg.UpsertJob(context.Background(), "wiring-job", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)

	run, err := st.CreateRun(context.Background(), "wiring-job", nil, []byte(`{}`), nil, nil, "api")
	require.NoError(t, err)
	require.NoError(t, st.MarkRunFinished(context.Background(), run.ID, api.RunFailed))

	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	for _, want := range []string{
		`unifiedcd_runs_created_total{trigger="api"} 1`,
		`unifiedcd_runs_finished_total{status="Failed"} 1`,
		`unifiedcd_runs_current{status="Pending"} 0`,
		`unifiedcd_agents{state="alive"} 0`,
	} {
		assert.True(t, strings.Contains(body, want), "missing %q in:\n%s", want, body)
	}
}
