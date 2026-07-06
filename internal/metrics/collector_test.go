package metrics

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeCounts struct {
	runs         map[api.RunStatus]int
	alive, stale int
	err          error
}

func (f *fakeCounts) CountRunsByStatus(ctx context.Context) (map[api.RunStatus]int, error) {
	return f.runs, f.err
}

func (f *fakeCounts) CountAgentsByLiveness(ctx context.Context, staleAfter time.Duration) (int, int, error) {
	return f.alive, f.stale, f.err
}

func TestDBCollectorExportsGauges(t *testing.T) {
	m := New()
	m.RegisterDBCollector(&fakeCounts{
		runs:  map[api.RunStatus]int{api.RunPending: 2, api.RunRunning: 1},
		alive: 3, stale: 1,
	}, 90*time.Second)

	expected := `
# HELP unifiedcd_agents Registered agents, by heartbeat liveness.
# TYPE unifiedcd_agents gauge
unifiedcd_agents{state="alive"} 3
unifiedcd_agents{state="stale"} 1
# HELP unifiedcd_runs_current Current number of non-terminal runs, by status.
# TYPE unifiedcd_runs_current gauge
unifiedcd_runs_current{status="Pending"} 2
unifiedcd_runs_current{status="Queued"} 0
unifiedcd_runs_current{status="Running"} 1
`
	require.NoError(t, testutil.GatherAndCompare(m.reg, strings.NewReader(expected),
		"unifiedcd_runs_current", "unifiedcd_agents"))
}

func TestDBCollectorErrorsIncrementCounterAndOmitFamilies(t *testing.T) {
	m := New()
	m.RegisterDBCollector(&fakeCounts{err: errors.New("db down")}, 90*time.Second)

	families, err := m.reg.Gather()
	require.NoError(t, err) // scrape itself must not fail
	for _, f := range families {
		assert.NotEqual(t, "unifiedcd_runs_current", f.GetName())
		assert.NotEqual(t, "unifiedcd_agents", f.GetName())
	}
	assert.Equal(t, 2.0, testutil.ToFloat64(m.collectorErrors)) // one per failed query
}
