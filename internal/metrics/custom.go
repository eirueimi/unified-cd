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
	"runtime/debug"
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
	// rejected caches definitions that failed validation (name -> shape hash),
	// so a repeatedly emitted bad definition does not rebuild a scratch registry
	// on every sample.
	rejected map[string]string
	// combined is set once the combined vec + DB collector is registered.
	combined bool
}

// NewCustomEmitter builds an emitter. errs is the
// unifiedcd_custom_metric_errors_total vec (labels metric, reason).
//
// The vecs are not registered on reg one by one: client_golang remembers the
// label names and help of every name ever registered, even after Unregister, so
// a shape change could never re-register. Instead the emitter registers one
// unchecked collector that exposes whichever vecs are current.
func NewCustomEmitter(reg *prometheus.Registry, cfg CustomConfig, gauges CustomGaugeStore, errs *prometheus.CounterVec) *CustomEmitter {
	e := &CustomEmitter{reg: reg, cfg: cfg, gauges: gauges, errors: errs, vecs: map[string]*customVec{}, rejected: map[string]string{}}
	if err := reg.Register((*vecCollector)(e)); err != nil {
		slog.Error("metrics: register custom vec collector", "error", err)
	}
	return e
}

// vecCollector is the unchecked collector over the emitter's current vecs.
type vecCollector CustomEmitter

func (c *vecCollector) Describe(chan<- *prometheus.Desc) {}

// Collect exposes the current vecs, unless ConfigureCustom installed the
// combined collector, which then does it (and must be the only one to, so
// that vecs and DB rows come from a single snapshot).
func (c *vecCollector) Collect(ch chan<- prometheus.Metric) {
	e := (*CustomEmitter)(c)
	cols, _, combined := e.snapshot()
	if combined {
		return
	}
	for _, col := range cols {
		col.Collect(ch)
	}
}

// snapshot returns, under one lock, the current vec collectors and the set of
// family names they expose.
func (e *CustomEmitter) snapshot() (cols []prometheus.Collector, fams map[string]struct{}, combined bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	fams = map[string]struct{}{}
	for _, v := range e.vecs {
		cols = append(cols, v.collector())
		for _, f := range v.fams {
			fams[f] = struct{}{}
		}
	}
	return cols, fams, e.combined
}

// configure replaces the emitter's config and gauge store (used by
// Metrics.ConfigureCustom once the controller has both).
func (e *CustomEmitter) configure(cfg CustomConfig, gauges CustomGaugeStore) {
	e.mu.Lock()
	e.cfg, e.gauges = cfg, gauges
	// Only the combined collector exists once configured (see ConfigureCustom).
	e.combined = true
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

// validateVec rejects definitions client_golang cannot represent before a vec
// is built (it panics on them when the first child is created).
func validateVec(shape dsl.MetricShape) error {
	if shape.Kind == "histogram" {
		for _, k := range shape.LabelKeys {
			if k == "le" {
				return errors.New(`histogram label "le" is reserved`)
			}
		}
		for i, b := range shape.Buckets {
			if math.IsNaN(b) || (i > 0 && b <= shape.Buckets[i-1]) {
				return errors.New("histogram buckets must be strictly increasing")
			}
		}
	}
	return nil
}

func (e *CustomEmitter) emitVec(def dsl.MetricDef, shape dsl.MetricShape, labels map[string]string, value float64, cfg CustomConfig) {
	name := def.ExposedName()
	vals, ok := labelValues(shape.LabelKeys, labels)
	if !ok {
		e.CountError(name, ReasonTemplate)
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	hash := shape.Hash()
	// Backstop for any definition client_golang still panics on after
	// validateVec: log loudly, drop the vec so it cannot re-panic on every
	// sample, and keep the controller alive.
	defer func() {
		if r := recover(); r != nil {
			slog.Error("metrics: custom metric panicked in client_golang", "metric", name, "panic", r, "stack", string(debug.Stack()))
			delete(e.vecs, name)
			e.rejected[name] = hash
			e.CountError(name, ReasonConflict)
		}
	}()

	if h, ok := e.rejected[name]; ok {
		if h == hash {
			e.CountError(name, ReasonConflict)
			return
		}
		delete(e.rejected, name) // the definition changed; judge it afresh
	}
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
		if err := validateVec(shape); err != nil {
			slog.Warn("metrics: custom metric rejected", "metric", name, "error", err)
			e.rejected[name] = hash
			e.CountError(name, ReasonConflict)
			return
		}
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
		// poisoning every later scrape.
		if err := prometheus.NewRegistry().Register(nv.collector()); err != nil {
			slog.Warn("metrics: custom metric rejected", "metric", name, "error", err)
			e.rejected[name] = hash
			e.CountError(name, ReasonConflict)
			return
		}
		// Another vec exposing the same family would fail the whole scrape.
		// This can clear once that vec expires, so it is not cached.
		if e.familyTaken(name, nv.fams) {
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
	for name, v := range e.vecs {
		for k, s := range v.series {
			if now.Sub(s.lastUpdate) > e.cfg.TTL {
				v.delete(s.labelValues)
				delete(v.series, k)
			}
		}
		// A vec with no series left holds nothing; dropping it frees its
		// families for other definitions.
		if len(v.series) == 0 {
			delete(e.vecs, name)
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
