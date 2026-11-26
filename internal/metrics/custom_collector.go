package metrics

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/prometheus/client_golang/prometheus"
)

// customCollector is an unchecked collector: Describe sends nothing because
// names and label sets are dynamic. It exports DB-backed custom gauges and
// realtime rows. Metric names are the EXPOSED names (unifiedcd_job_<name>),
// which is what the store rows hold and what shapes() is keyed by.
type customCollector struct {
	cfg    CustomConfig
	gauges CustomGaugeStore
	shapes func(name string) (dsl.MetricShape, bool)
	errs   *prometheus.CounterVec
	now    func() time.Time
	// taken, when set, reports whether an in-memory vec already exposes the
	// family; such rows are skipped so the two collectors never emit the same
	// family in one scrape.
	taken func(family string) bool
}

// RegisterCustomCollector registers the unchecked collector on reg. now may be
// nil (time.Now).
func RegisterCustomCollector(reg *prometheus.Registry, cfg CustomConfig, gauges CustomGaugeStore, shapes func(name string) (dsl.MetricShape, bool), errs *prometheus.CounterVec, now func() time.Time) error {
	if now == nil {
		now = time.Now
	}
	return registerCustomCollector(reg, cfg, gauges, shapes, errs, now, nil)
}

func registerCustomCollector(reg *prometheus.Registry, cfg CustomConfig, gauges CustomGaugeStore, shapes func(name string) (dsl.MetricShape, bool), errs *prometheus.CounterVec, now func() time.Time, taken func(string) bool) error {
	if now == nil {
		now = time.Now
	}
	return reg.Register(&customCollector{cfg: cfg, gauges: gauges, shapes: shapes, errs: errs, now: now, taken: taken})
}

func (c *customCollector) Describe(chan<- *prometheus.Desc) {}

func (c *customCollector) countError(metric, reason string) {
	if c.errs == nil {
		return
	}
	if ctr, err := c.errs.GetMetricWithLabelValues(metric, reason); err == nil {
		ctr.Inc()
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

type collected struct {
	name, help string
	keys, vals []string
	value      float64
}

func (c *customCollector) Collect(ch chan<- prometheus.Metric) {
	if !c.cfg.Enabled || c.gauges == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()
	now := c.now()

	var since time.Time
	if c.cfg.TTL > 0 {
		since = now.Add(-c.cfg.TTL)
	}

	// Keyed by name + label values. The help always comes from the current
	// shape, so every row of a name agrees (client_golang rejects a gather
	// where one family has two help strings).
	out := map[string]*collected{}
	add := func(name string, labels map[string]string, v float64, realtime bool) {
		shape, ok := c.shapes(name)
		keys := sortedKeys(labels)
		// A row whose name is now a counter or histogram would collide with that
		// family and fail the whole /metrics response.
		if !ok || shape.Kind != "gauge" || (c.taken != nil && c.taken(name)) || strings.Join(keys, "\x00") != strings.Join(shape.LabelKeys, "\x00") || len(keys) != len(shape.LabelKeys) {
			c.countError(name, ReasonConflict)
			return
		}
		vals := make([]string, len(keys))
		for i, k := range keys {
			vals[i] = labels[k]
		}
		id := name + "\x01" + seriesKey(vals)
		if prev, dup := out[id]; dup {
			if realtime && v > prev.value {
				prev.value = v
			}
			return
		}
		out[id] = &collected{name: name, help: shape.Help, keys: keys, vals: vals, value: v}
	}

	rows, err := c.gauges.ListMetricGauges(ctx, since)
	if err != nil {
		slog.Warn("metrics: list custom gauges", "error", err)
		c.countError("", ReasonDB)
	}
	for _, g := range rows {
		add(g.Name, g.Labels, g.Value, false)
	}
	rt, err := c.gauges.ListMetricRealtime(ctx)
	if err != nil {
		slog.Warn("metrics: list custom realtime gauges", "error", err)
		c.countError("", ReasonDB)
	}
	for _, r := range rt {
		add(r.Name, r.Labels, now.Sub(r.StartedAt).Seconds(), true)
	}

	for _, s := range out {
		desc := prometheus.NewDesc(s.name, s.help, s.keys, nil)
		m, err := prometheus.NewConstMetric(desc, prometheus.GaugeValue, s.value, s.vals...)
		if err != nil {
			slog.Warn("metrics: invalid custom gauge", "metric", s.name, "error", err)
			c.countError(s.name, ReasonConflict)
			continue
		}
		ch <- m
	}
}
