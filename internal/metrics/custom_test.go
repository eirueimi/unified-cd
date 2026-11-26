package metrics

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

type fakeGauges struct {
	mu       sync.Mutex
	applied  []store.MetricGauge
	ops      []string
	hashes   []string
	maxes    []int
	err      error
	gauges   []store.MetricGauge
	realtime []store.MetricRealtime
	since    time.Time
}

func (f *fakeGauges) ApplyMetricGauge(_ context.Context, g store.MetricGauge, hash, op string, max int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, g)
	f.ops = append(f.ops, op)
	f.hashes = append(f.hashes, hash)
	f.maxes = append(f.maxes, max)
	return f.err
}

func (f *fakeGauges) ListMetricGauges(_ context.Context, since time.Time) ([]store.MetricGauge, error) {
	f.since = since
	var out []store.MetricGauge
	for _, g := range f.gauges {
		if !g.UpdatedAt.Before(since) {
			out = append(out, g)
		}
	}
	return out, nil
}

func (f *fakeGauges) ListMetricRealtime(context.Context) ([]store.MetricRealtime, error) {
	return f.realtime, nil
}

func newTestEmitter(cfg CustomConfig, g CustomGaugeStore) (*CustomEmitter, *prometheus.Registry, *prometheus.CounterVec) {
	reg := prometheus.NewRegistry()
	errs := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "unifiedcd_custom_metric_errors_total", Help: "x"}, []string{"metric", "reason"})
	cfg.Enabled = true
	return NewCustomEmitter(reg, cfg, g, errs), reg, errs
}

func counterDef(name string, keys ...string) dsl.MetricDef {
	d := dsl.MetricDef{Name: name, Help: "h", Counter: &dsl.CounterMetric{}}
	for _, k := range keys {
		d.Labels = append(d.Labels, dsl.MetricLabel{Key: k})
	}
	return d
}

func errCount(errs *prometheus.CounterVec, metric, reason string) float64 {
	return testutil.ToFloat64(errs.WithLabelValues(metric, reason))
}

func TestEmitter_CounterAndHistogram(t *testing.T) {
	e, reg, _ := newTestEmitter(CustomConfig{MaxSeriesPerMetric: 10, TTL: time.Hour}, nil)
	ctx := context.Background()
	c := counterDef("builds", "result")
	e.Emit(ctx, c, map[string]string{"result": "ok"}, 1)
	e.Emit(ctx, c, map[string]string{"result": "ok"}, 1)
	exp := "# HELP unifiedcd_job_builds_total h\n# TYPE unifiedcd_job_builds_total counter\nunifiedcd_job_builds_total{result=\"ok\"} 2\n"
	if err := testutil.GatherAndCompare(reg, strings.NewReader(exp), "unifiedcd_job_builds_total"); err != nil {
		t.Fatal(err)
	}

	h := dsl.MetricDef{Name: "dur", Help: "d", Histogram: &dsl.HistogramMetric{Buckets: []float64{1, 10}}}
	e.Emit(ctx, h, map[string]string{}, 5)
	exp = "# HELP unifiedcd_job_dur d\n# TYPE unifiedcd_job_dur histogram\n" +
		"unifiedcd_job_dur_bucket{le=\"1\"} 0\nunifiedcd_job_dur_bucket{le=\"10\"} 1\nunifiedcd_job_dur_bucket{le=\"+Inf\"} 1\n" +
		"unifiedcd_job_dur_sum 5\nunifiedcd_job_dur_count 1\n"
	if err := testutil.GatherAndCompare(reg, strings.NewReader(exp), "unifiedcd_job_dur"); err != nil {
		t.Fatal(err)
	}

	// Empty buckets fall back to the Prometheus defaults.
	h2 := dsl.MetricDef{Name: "dur2", Help: "d", Histogram: &dsl.HistogramMetric{}}
	e.Emit(ctx, h2, map[string]string{}, 0.5)
	mfs, _ := reg.Gather()
	found := false
	for _, mf := range mfs {
		if mf.GetName() == "unifiedcd_job_dur2" {
			found = true
			if n := len(mf.GetMetric()[0].GetHistogram().GetBucket()); n != len(prometheus.DefBuckets) {
				t.Fatalf("buckets = %d", n)
			}
		}
	}
	if !found {
		t.Fatal("dur2 not exported")
	}
}

func TestEmitter_SeriesCap(t *testing.T) {
	e, reg, errs := newTestEmitter(CustomConfig{MaxSeriesPerMetric: 2, TTL: time.Hour}, nil)
	c := counterDef("capped", "k")
	for _, v := range []string{"a", "b", "c", "a"} {
		e.Emit(context.Background(), c, map[string]string{"k": v}, 1)
	}
	if n, _ := testutil.GatherAndCount(reg, "unifiedcd_job_capped_total"); n != 2 {
		t.Fatalf("series = %d, want 2", n)
	}
	if got := errCount(errs, "unifiedcd_job_capped_total", "series_cap"); got != 1 {
		t.Fatalf("series_cap = %v", got)
	}
}

func TestEmitter_TTLSweep(t *testing.T) {
	e, reg, _ := newTestEmitter(CustomConfig{MaxSeriesPerMetric: 10, TTL: time.Minute}, nil)
	e.Emit(context.Background(), counterDef("ttl", "k"), map[string]string{"k": "a"}, 1)
	e.Sweep(time.Now())
	if n, _ := testutil.GatherAndCount(reg, "unifiedcd_job_ttl_total"); n != 1 {
		t.Fatalf("series before expiry = %d", n)
	}
	e.Sweep(time.Now().Add(time.Minute + time.Second))
	if n, _ := testutil.GatherAndCount(reg, "unifiedcd_job_ttl_total"); n != 0 {
		t.Fatalf("series after expiry = %d", n)
	}
}

func TestEmitter_ShapeChange(t *testing.T) {
	e, reg, errs := newTestEmitter(CustomConfig{MaxSeriesPerMetric: 10, TTL: time.Minute}, nil)
	ctx := context.Background()
	e.Emit(ctx, counterDef("shape", "a"), map[string]string{"a": "1"}, 1)
	e.Emit(ctx, counterDef("shape", "b"), map[string]string{"b": "1"}, 1)
	if got := errCount(errs, "unifiedcd_job_shape_total", "conflict"); got != 1 {
		t.Fatalf("conflict = %v", got)
	}
	e.Sweep(time.Now().Add(time.Hour))
	e.Emit(ctx, counterDef("shape", "b"), map[string]string{"b": "1"}, 1)
	if got := errCount(errs, "unifiedcd_job_shape_total", "conflict"); got != 1 {
		t.Fatalf("conflict after sweep = %v", got)
	}
	exp := "# HELP unifiedcd_job_shape_total h\n# TYPE unifiedcd_job_shape_total counter\nunifiedcd_job_shape_total{b=\"1\"} 1\n"
	if err := testutil.GatherAndCompare(reg, strings.NewReader(exp), "unifiedcd_job_shape_total"); err != nil {
		t.Fatal(err)
	}
}

func TestEmitter_BadValue(t *testing.T) {
	e, reg, errs := newTestEmitter(CustomConfig{MaxSeriesPerMetric: 10, TTL: time.Minute}, nil)
	ctx := context.Background()
	e.CountError("unifiedcd_job_x", "value")
	if got := errCount(errs, "unifiedcd_job_x", "value"); got != 1 {
		t.Fatalf("value = %v", got)
	}
	c := counterDef("bad", "k")
	e.Emit(ctx, c, map[string]string{"k": "a"}, -1)
	if got := errCount(errs, "unifiedcd_job_bad_total", "value"); got != 1 {
		t.Fatalf("negative counter = %v", got)
	}
	// Wrong label counts and keys never panic.
	e.Emit(ctx, c, map[string]string{}, 1)
	e.Emit(ctx, c, map[string]string{"k": "a", "extra": "b"}, 1)
	e.Emit(ctx, c, map[string]string{"other": "a"}, 1)
	// Invalid UTF-8 label value is an error, not a panic.
	e.Emit(ctx, c, map[string]string{"k": "\xff"}, 1)
	if n, _ := testutil.GatherAndCount(reg, "unifiedcd_job_bad_total"); n != 0 {
		t.Fatalf("series = %d", n)
	}
	if got := errCount(errs, "unifiedcd_job_bad_total", "template"); got != 3 {
		t.Fatalf("template = %v", got)
	}
	if got := errCount(errs, "unifiedcd_job_bad_total", "value"); got != 2 {
		t.Fatalf("value (negative + invalid utf-8) = %v", got)
	}
}

func TestEmitter_RejectedDefinitions(t *testing.T) {
	e, reg, errs := newTestEmitter(CustomConfig{MaxSeriesPerMetric: 10, TTL: time.Minute}, nil)
	ctx := context.Background()
	// A histogram label named "le" makes client_golang panic when the first
	// child is created; it must be counted, not crash.
	h := dsl.MetricDef{Name: "lehist", Help: "h", Histogram: &dsl.HistogramMetric{}, Labels: []dsl.MetricLabel{{Key: "le"}}}
	e.Emit(ctx, h, map[string]string{"le": "x"}, 1)
	if got := errCount(errs, "unifiedcd_job_lehist", "conflict"); got != 1 {
		t.Fatalf("le conflict = %v", got)
	}
	// Two vecs may not expose the same family: histogram "a_total" and counter
	// "a" both expose unifiedcd_job_a_total.
	e.Emit(ctx, dsl.MetricDef{Name: "a_total", Help: "h", Histogram: &dsl.HistogramMetric{}}, map[string]string{}, 1)
	e.Emit(ctx, dsl.MetricDef{Name: "a", Help: "h", Counter: &dsl.CounterMetric{}}, map[string]string{}, 1)
	if got := errCount(errs, "unifiedcd_job_a_total", "conflict"); got != 1 {
		t.Fatalf("family conflict = %v", got)
	}
	if _, err := reg.Gather(); err != nil {
		t.Fatalf("gather must stay healthy: %v", err)
	}
}

func TestEmitter_GaugeOps(t *testing.T) {
	f := &fakeGauges{}
	e, _, errs := newTestEmitter(CustomConfig{MaxSeriesPerMetric: 7, TTL: time.Minute}, f)
	ctx := context.Background()
	def := func(op string) dsl.MetricDef {
		return dsl.MetricDef{Name: "g", Help: "h", Gauge: &dsl.GaugeMetric{Operation: op},
			Labels: []dsl.MetricLabel{{Key: "a"}, {Key: "b"}}}
	}
	e.Emit(ctx, def(""), map[string]string{"a": "1", "b": "2"}, 3)
	e.Emit(ctx, def("Add"), map[string]string{"b": "2", "a": "1"}, 3)
	e.Emit(ctx, def("Sub"), map[string]string{"a": "1", "b": "2"}, 3)
	if strings.Join(f.ops, ",") != "Set,Add,Sub" {
		t.Fatalf("ops = %v", f.ops)
	}
	if f.hashes[0] != f.hashes[1] || f.hashes[1] != f.hashes[2] {
		t.Fatalf("hashes differ: %v", f.hashes)
	}
	if f.applied[0].Name != "unifiedcd_job_g" || f.maxes[0] != 7 {
		t.Fatalf("row = %+v max=%d", f.applied[0], f.maxes[0])
	}
	f.err = store.ErrSeriesCap
	e.Emit(ctx, def("Set"), map[string]string{"a": "9", "b": "2"}, 1)
	if got := errCount(errs, "unifiedcd_job_g", "series_cap"); got != 1 {
		t.Fatalf("series_cap = %v", got)
	}
	f.err = context.DeadlineExceeded
	e.Emit(ctx, def("Set"), map[string]string{"a": "9", "b": "2"}, 1)
	if got := errCount(errs, "unifiedcd_job_g", "db"); got != 1 {
		t.Fatalf("db = %v", got)
	}
	// Realtime gauges are dropped and counted as conflict, never stored.
	n := len(f.applied)
	rt := dsl.MetricDef{Name: "r", Help: "h", Gauge: &dsl.GaugeMetric{Realtime: true}}
	e.Emit(ctx, rt, map[string]string{}, 1)
	if len(f.applied) != n || errCount(errs, "unifiedcd_job_r", "conflict") != 1 {
		t.Fatalf("realtime handling wrong")
	}
}

func TestEmitter_DisabledAndNil(t *testing.T) {
	var nilE *CustomEmitter
	nilE.Emit(context.Background(), counterDef("x"), nil, 1)
	nilE.Sweep(time.Now())
	nilE.CountError("x", "db")
	m := New()
	m.Custom().Emit(context.Background(), counterDef("x"), map[string]string{}, 1) // disabled
	if n, _ := testutil.GatherAndCount(m.reg, "unifiedcd_job_x_total"); n != 0 {
		t.Fatal("disabled emitter recorded a sample")
	}
}

func TestBuiltinFamilyNames(t *testing.T) {
	names := map[string]bool{}
	for _, n := range BuiltinFamilyNames() {
		names[n] = true
	}
	for _, want := range []string{
		"unifiedcd_runs_created_total", "unifiedcd_build_info", "unifiedcd_custom_metric_errors_total",
		"unifiedcd_http_request_duration_seconds_bucket", "unifiedcd_run_time_to_claim_seconds_count",
		"go_goroutines",
	} {
		if !names[want] {
			t.Errorf("missing %s", want)
		}
	}
}
