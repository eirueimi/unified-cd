package metrics

import (
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/prometheus/client_golang/prometheus"
)

// PoolStatser is the narrow store surface the pool collector needs.
// *store.Postgres implements it.
type PoolStatser interface {
	PoolStats() map[string]store.PoolStat
}

var (
	poolConnsDesc = prometheus.NewDesc("unifiedcd_db_pool_connections",
		"Postgres pool connections, by pool and state.", []string{"pool", "state"}, nil)
	poolMaxDesc = prometheus.NewDesc("unifiedcd_db_pool_max_connections",
		"Configured connection ceiling, by pool.", []string{"pool"}, nil)
	poolEmptyAcquireDesc = prometheus.NewDesc("unifiedcd_db_pool_empty_acquires_total",
		"Acquires that found no free connection and had to wait, by pool.", []string{"pool"}, nil)
	poolCanceledAcquireDesc = prometheus.NewDesc("unifiedcd_db_pool_canceled_acquires_total",
		"Acquires abandoned because the caller's context ended while waiting, by pool.", []string{"pool"}, nil)
)

type poolCollector struct{ pools PoolStatser }

// RegisterPoolCollector registers the scrape-time connection-pool gauges.
//
// These are collected at scrape time rather than recorded at call sites
// because pgxpool already keeps the counters; sampling them costs no database
// work, which is why this collector — unlike dbCollector — needs no timeout
// and can never fail a scrape.
func (m *Metrics) RegisterPoolCollector(p PoolStatser) {
	m.reg.MustRegister(&poolCollector{pools: p})
}

func (c *poolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- poolConnsDesc
	ch <- poolMaxDesc
	ch <- poolEmptyAcquireDesc
	ch <- poolCanceledAcquireDesc
}

func (c *poolCollector) Collect(ch chan<- prometheus.Metric) {
	for name, s := range c.pools.PoolStats() {
		ch <- prometheus.MustNewConstMetric(poolConnsDesc, prometheus.GaugeValue, float64(s.AcquiredConns), name, "acquired")
		ch <- prometheus.MustNewConstMetric(poolConnsDesc, prometheus.GaugeValue, float64(s.IdleConns), name, "idle")
		ch <- prometheus.MustNewConstMetric(poolConnsDesc, prometheus.GaugeValue, float64(s.TotalConns), name, "total")
		ch <- prometheus.MustNewConstMetric(poolMaxDesc, prometheus.GaugeValue, float64(s.MaxConns), name)
		// Cumulative pgxpool counters, exported as counters so rate() is the
		// correct query. A pool restart resets them, which is exactly the
		// reset semantics Prometheus counters already handle.
		ch <- prometheus.MustNewConstMetric(poolEmptyAcquireDesc, prometheus.CounterValue, float64(s.EmptyAcquireCount), name)
		ch <- prometheus.MustNewConstMetric(poolCanceledAcquireDesc, prometheus.CounterValue, float64(s.CanceledAcquireCount), name)
	}
}
