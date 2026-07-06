package metrics

import (
	"context"
	"log/slog"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/prometheus/client_golang/prometheus"
)

// Counts is the narrow store surface the scrape-time collector needs.
// *store.Postgres implements it.
type Counts interface {
	CountRunsByStatus(ctx context.Context) (map[api.RunStatus]int, error)
	CountAgentsByLiveness(ctx context.Context, staleAfter time.Duration) (alive, stale int, err error)
}

// collectTimeout bounds the DB work done per scrape.
const collectTimeout = 3 * time.Second

var (
	runsCurrentDesc = prometheus.NewDesc("unifiedcd_runs_current",
		"Current number of non-terminal runs, by status.", []string{"status"}, nil)
	agentsDesc = prometheus.NewDesc("unifiedcd_agents",
		"Registered agents, by heartbeat liveness.", []string{"state"}, nil)
)

type dbCollector struct {
	counts     Counts
	staleAfter time.Duration
	errors     prometheus.Counter
}

// RegisterDBCollector registers the scrape-time DB gauges on m's registry.
// staleAfter is the agent-liveness window (matches the stuck-run reaper).
func (m *Metrics) RegisterDBCollector(c Counts, staleAfter time.Duration) {
	m.reg.MustRegister(&dbCollector{counts: c, staleAfter: staleAfter, errors: m.collectorErrors})
}

func (d *dbCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- runsCurrentDesc
	ch <- agentsDesc
}

// Collect queries the DB with a bounded timeout. On error it increments the
// error counter and omits the affected family — the scrape never fails.
func (d *dbCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	if counts, err := d.counts.CountRunsByStatus(ctx); err != nil {
		d.errors.Inc()
		slog.Warn("metrics: count runs by status", "error", err)
	} else {
		for _, st := range []api.RunStatus{api.RunPending, api.RunQueued, api.RunRunning} {
			ch <- prometheus.MustNewConstMetric(runsCurrentDesc, prometheus.GaugeValue,
				float64(counts[st]), string(st))
		}
	}

	if alive, stale, err := d.counts.CountAgentsByLiveness(ctx, d.staleAfter); err != nil {
		d.errors.Inc()
		slog.Warn("metrics: count agents by liveness", "error", err)
	} else {
		ch <- prometheus.MustNewConstMetric(agentsDesc, prometheus.GaugeValue, float64(alive), "alive")
		ch <- prometheus.MustNewConstMetric(agentsDesc, prometheus.GaugeValue, float64(stale), "stale")
	}
}
