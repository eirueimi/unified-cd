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
}

// RegisterCustomCollector registers the unchecked collector on reg. now may be
// nil (time.Now).
func RegisterCustomCollector(reg *prometheus.Registry, cfg CustomConfig, gauges CustomGaugeStore, shapes func(name string) (dsl.MetricShape, bool), errs *prometheus.CounterVec, now func() time.Time) error {
	if now == nil {
		now = time.Now
	}
	return reg.Register(&customCollector{cfg: cfg, gauges: gauges, shapes: shapes, errs: errs, now: now})
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

	// Keyed by name + label values. The first help seen for a name wins; a
	// row with a different help would make the scrape inconsistent.
	out := map[string]*collected{}
	helps := map[string]string{}
	add := func(name, help string, labels map[string]string, v float64, realtime bool) {
		shape, ok := c.shapes(name)
		keys := sortedKeys(labels)
		if !ok || strings.Join(keys, "\x00") != strings.Join(shape.LabelKeys, "\x00") || len(keys) != len(shape.LabelKeys) {
			c.countError(name, ReasonConflict)
			return
		}
		if h, seen := helps[name]; seen && h != help {
			c.countError(name, ReasonConflict)
			return
		}
		helps[name] = help
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
		out[id] = &collected{name: name, help: help, keys: keys, vals: vals, value: v}
	}

	rows, err := c.gauges.ListMetricGauges(ctx, since)
	if err != nil {
		slog.Warn("metrics: list custom gauges", "error", err)
		c.countError("", ReasonDB)
	}
	for _, g := range rows {
		add(g.Name, g.Help, g.Labels, g.Value, false)
	}
	rt, err := c.gauges.ListMetricRealtime(ctx)
	if err != nil {
		slog.Warn("metrics: list custom realtime gauges", "error", err)
		c.countError("", ReasonDB)
	}
	for _, r := range rt {
		add(r.Name, r.Help, r.Labels, now.Sub(r.StartedAt).Seconds(), true)
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
