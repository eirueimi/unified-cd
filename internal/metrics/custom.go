package metrics

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/prometheus/client_golang/prometheus"
)

// Error reasons for unifiedcd_custom_metric_errors_total.
const (
	ReasonTemplate  = "template"
	ReasonValue     = "value"
	ReasonConflict  = "conflict"
	ReasonSeriesCap = "series_cap"
	ReasonDB        = "db"
)

// CustomConfig is the controller's customMetrics section.
type CustomConfig struct {
	Enabled            bool
	MaxSeriesPerMetric int           // per exposed name per replica; <= 0 means unlimited
	TTL                time.Duration // 0 disables the sweep and the export-time age filter
}

// CustomGaugeStore is the slice of the store that custom gauges need.
// *store.Postgres implements it.
type CustomGaugeStore interface {
	ApplyMetricGauge(ctx context.Context, g store.MetricGauge, labelsHash, op string, maxSeries int) error
	ListMetricGauges(ctx context.Context, since time.Time) ([]store.MetricGauge, error)
	ListMetricRealtime(ctx context.Context) ([]store.MetricRealtime, error)
}

type seriesEntry struct {
	labelValues []string
	lastUpdate  time.Time
}

// customVec is one dynamic CounterVec or HistogramVec plus its own series
// bookkeeping (client_golang records neither series count nor update time).
type customVec struct {
	hash    string
	fams    []string
	counter *prometheus.CounterVec
	hist    *prometheus.HistogramVec
	series  map[string]*seriesEntry
}

func (v *customVec) collector() prometheus.Collector {
	if v.counter != nil {
		return v.counter
	}
	return v.hist
}

func (v *customVec) delete(values []string) {
	if v.counter != nil {
		v.counter.DeleteLabelValues(values...)
	} else {
		v.hist.DeleteLabelValues(values...)
	}
}

// CustomEmitter records job-defined metric samples. Counters and histograms
// live in memory on the controller registry; gauges go to the database.
// Nothing a job defines can panic it: every client_golang call that can fail
// is the error-returning variant.
type CustomEmitter struct {
	reg    *prometheus.Registry
	errors *prometheus.CounterVec

	mu     sync.Mutex
	cfg    CustomConfig
	gauges CustomGaugeStore
	vecs   map[string]*customVec
}

// NewCustomEmitter builds an emitter. errs is the
// unifiedcd_custom_metric_errors_total vec (labels metric, reason).
//
// The vecs are not registered on reg one by one: client_golang remembers the
// label names and help of every name ever registered, even after Unregister, so
// a shape change could never re-register. Instead the emitter registers one
// unchecked collector that exposes whichever vecs are current.
func NewCustomEmitter(reg *prometheus.Registry, cfg CustomConfig, gauges CustomGaugeStore, errs *prometheus.CounterVec) *CustomEmitter {
	e := &CustomEmitter{reg: reg, cfg: cfg, gauges: gauges, errors: errs, vecs: map[string]*customVec{}}
	if err := reg.Register((*vecCollector)(e)); err != nil {
		slog.Error("metrics: register custom vec collector", "error", err)
	}
	return e
}

// vecCollector is the unchecked collector over the emitter's current vecs.
type vecCollector CustomEmitter

func (c *vecCollector) Describe(chan<- *prometheus.Desc) {}

func (c *vecCollector) Collect(ch chan<- prometheus.Metric) {
	e := (*CustomEmitter)(c)
	e.mu.Lock()
	cols := make([]prometheus.Collector, 0, len(e.vecs))
	for _, v := range e.vecs {
		cols = append(cols, v.collector())
	}
	e.mu.Unlock()
	for _, col := range cols {
		col.Collect(ch)
	}
}

// configure replaces the emitter's config and gauge store (used by
// Metrics.ConfigureCustom once the controller has both).
func (e *CustomEmitter) configure(cfg CustomConfig, gauges CustomGaugeStore) {
	e.mu.Lock()
	e.cfg, e.gauges = cfg, gauges
	e.mu.Unlock()
}

// CountError increments the runtime error counter. Safe on a nil emitter.
func (e *CustomEmitter) CountError(metric, reason string) {
	if e == nil || e.errors == nil {
		return
	}
	if c, err := e.errors.GetMetricWithLabelValues(metric, reason); err == nil {
		c.Inc()
	}
}

// Emit records one sample. labels are already rendered and value is already
// parsed. A disabled emitter does nothing. Realtime gauges must never be
// passed here (the caller writes their rows); if one is, it is dropped and
// counted as a conflict.
func (e *CustomEmitter) Emit(ctx context.Context, def dsl.MetricDef, labels map[string]string, value float64) {
	if e == nil {
		return
	}
	e.mu.Lock()
	cfg, gauges := e.cfg, e.gauges
	e.mu.Unlock()
	if !cfg.Enabled {
		return
	}
	name := def.ExposedName()
	if math.IsNaN(value) || math.IsInf(value, 0) {
		e.CountError(name, ReasonValue)
		return
	}
	shape := def.Shape()
	switch shape.Kind {
	case "counter":
		if value < 0 {
			e.CountError(name, ReasonValue)
			return
		}
		e.emitVec(def, shape, labels, value, cfg)
	case "histogram":
		e.emitVec(def, shape, labels, value, cfg)
	case "gauge":
		if shape.Realtime {
			slog.Warn("metrics: realtime gauge passed to Emit; dropped", "metric", name)
			e.CountError(name, ReasonConflict)
			return
		}
		e.emitGauge(ctx, def, shape, labels, value, cfg, gauges)
	default:
		e.CountError(name, ReasonConflict)
	}
}

// labelValues orders labels by the shape's sorted keys. ok is false when the
// label set does not match the shape exactly.
func labelValues(keys []string, labels map[string]string) ([]string, bool) {
	if len(labels) != len(keys) {
		return nil, false
	}
	vals := make([]string, len(keys))
	for i, k := range keys {
		v, ok := labels[k]
		if !ok {
			return nil, false
		}
		vals[i] = v
	}
	return vals, true
}

// familyTaken reports whether another vec already exposes one of fams.
// e.mu must be held.
func (e *CustomEmitter) familyTaken(name string, fams []string) bool {
	for other, v := range e.vecs {
		if other == name {
			continue
		}
		for _, a := range fams {
			for _, b := range v.fams {
				if a == b {
					return true
				}
			}
		}
	}
	return false
}

func seriesKey(values []string) string { return fmt.Sprintf("%q", values) }

func (e *CustomEmitter) emitVec(def dsl.MetricDef, shape dsl.MetricShape, labels map[string]string, value float64, cfg CustomConfig) {
	name := def.ExposedName()
	// client_golang panics on a few definitions it cannot represent (a
	// histogram label named "le"); a job must never be able to crash the
	// controller, so this is the backstop for any such case.
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("metrics: custom metric rejected by client_golang", "metric", name, "panic", r)
			e.CountError(name, ReasonConflict)
		}
	}()
	vals, ok := labelValues(shape.LabelKeys, labels)
	if !ok {
		e.CountError(name, ReasonTemplate)
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	hash := shape.Hash()
	v := e.vecs[name]
	if v != nil && v.hash != hash {
		if len(v.series) > 0 {
			e.CountError(name, ReasonConflict)
			return
		}
		delete(e.vecs, name)
		v = nil
	}
	if v == nil {
		nv := &customVec{hash: hash, fams: def.Families(), series: map[string]*seriesEntry{}}
		if shape.Kind == "counter" {
			nv.counter = prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: shape.Help}, shape.LabelKeys)
		} else {
			buckets := shape.Buckets
			if len(buckets) == 0 {
				buckets = prometheus.DefBuckets
			}
			nv.hist = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: name, Help: shape.Help, Buckets: buckets}, shape.LabelKeys)
		}
		// A scratch registry rejects an invalid descriptor here instead of
		// poisoning every later scrape, and the family check keeps two vecs from
		// exposing the same family (which would also fail the scrape).
		if err := prometheus.NewRegistry().Register(nv.collector()); err != nil || e.familyTaken(name, nv.fams) {
			slog.Warn("metrics: custom metric rejected", "metric", name, "error", err)
			e.CountError(name, ReasonConflict)
			return
		}
		e.vecs[name] = nv
		v = nv
	}

	key := seriesKey(vals)
	entry := v.series[key]
	if entry == nil && cfg.MaxSeriesPerMetric > 0 && len(v.series) >= cfg.MaxSeriesPerMetric {
		e.CountError(name, ReasonSeriesCap)
		return
	}
	if v.counter != nil {
		c, err := v.counter.GetMetricWithLabelValues(vals...)
		if err != nil {
			e.CountError(name, ReasonValue)
			return
		}
		c.Add(value)
	} else {
		o, err := v.hist.GetMetricWithLabelValues(vals...)
		if err != nil {
			e.CountError(name, ReasonValue)
			return
		}
		o.Observe(value)
	}
	if entry == nil {
		entry = &seriesEntry{labelValues: vals}
		v.series[key] = entry
	}
	entry.lastUpdate = time.Now()
}

func (e *CustomEmitter) emitGauge(ctx context.Context, def dsl.MetricDef, shape dsl.MetricShape, labels map[string]string, value float64, cfg CustomConfig, gauges CustomGaugeStore) {
	name := def.ExposedName()
	if _, ok := labelValues(shape.LabelKeys, labels); !ok {
		e.CountError(name, ReasonTemplate)
		return
	}
	if gauges == nil {
		e.CountError(name, ReasonDB)
		return
	}
	op := def.Gauge.Operation
	if op == "" {
		op = "Set"
	}
	// The store row holds the EXPOSED name (unifiedcd_job_<name>), so the
	// collector can emit it verbatim and shapes() is keyed by exposed name.
	g := store.MetricGauge{Name: name, Help: shape.Help, Labels: labels, Value: value, UpdatedAt: time.Now()}
	err := gauges.ApplyMetricGauge(ctx, g, labelsHash(labels), op, cfg.MaxSeriesPerMetric)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrSeriesCap):
		e.CountError(name, ReasonSeriesCap)
	default:
		slog.Warn("metrics: apply custom gauge", "metric", name, "error", err)
		e.CountError(name, ReasonDB)
	}
}

// labelsHash is a digest of the label set that does not depend on map order.
func labelsHash(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%d:%s=%d:%s;", len(k), k, len(labels[k]), labels[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Sweep deletes in-memory series not updated within the TTL. A zero TTL
// disables it.
func (e *CustomEmitter) Sweep(now time.Time) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cfg.TTL <= 0 {
		return
	}
	for _, v := range e.vecs {
		for k, s := range v.series {
			if now.Sub(s.lastUpdate) > e.cfg.TTL {
				v.delete(s.labelValues)
				delete(v.series, k)
			}
		}
	}
}

var fqNameRe = regexp.MustCompile(`fqName: "([^"]*)"`)

// BuiltinFamilyNames returns every family name New() registers, plus the
// _bucket/_sum/_count variants of each (a built-in histogram exposes them, and
// a job metric named like one would collide). It is derived from the
// collectors' own Describe output and a Gather of a fresh registry, so it
// cannot drift from New(). Used by the apply-time conflict check.
func BuiltinFamilyNames() []string {
	builtinOnce.Do(func() { builtinNames = computeBuiltinFamilyNames() })
	return append([]string(nil), builtinNames...)
}

var (
	builtinOnce  sync.Once
	builtinNames []string
)

func computeBuiltinFamilyNames() []string {
	m := New()
	set := map[string]struct{}{}
	for _, c := range m.builtinCollectors {
		ch := make(chan *prometheus.Desc, 64)
		go func(c prometheus.Collector) { c.Describe(ch); close(ch) }(c)
		for d := range ch {
			if sm := fqNameRe.FindStringSubmatch(d.String()); sm != nil {
				set[sm[1]] = struct{}{}
			}
		}
	}
	if mfs, err := m.reg.Gather(); err == nil {
		for _, mf := range mfs {
			set[mf.GetName()] = struct{}{}
		}
	}
	for n := range set {
		set[n+"_bucket"] = struct{}{}
		set[n+"_sum"] = struct{}{}
		set[n+"_count"] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
