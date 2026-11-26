package metrics

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestDBCollector_SkipsNonGaugeShape(t *testing.T) {
	now := time.Now()
	f := &fakeGauges{gauges: []store.MetricGauge{
		{Name: "unifiedcd_job_h", Help: "h", Labels: map[string]string{}, Value: 1, UpdatedAt: now},
	}}
	reg := prometheus.NewRegistry()
	errs := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "e_total", Help: "x"}, []string{"metric", "reason"})
	err := RegisterCustomCollector(reg, CustomConfig{Enabled: true, TTL: time.Hour}, f,
		func(string) (dsl.MetricShape, bool) { return dsl.MetricShape{Kind: "histogram", Help: "h"}, true },
		errs, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	mfs, err := reg.Gather()
	if err != nil || len(mfs) != 0 {
		t.Fatalf("gather = %v, %v", mfs, err)
	}
	if got := testutil.ToFloat64(errs.WithLabelValues("unifiedcd_job_h", "conflict")); got != 1 {
		t.Fatalf("conflict = %v", got)
	}
}

func TestCustom_GaugeRowCollidingWithVecIsSkipped(t *testing.T) {
	now := time.Now()
	f := &fakeGauges{gauges: []store.MetricGauge{
		// A stale gauge row whose exposed name equals a live counter's family.
		{Name: "unifiedcd_job_a_total", Help: "h", Labels: map[string]string{}, Value: 1, UpdatedAt: now},
	}}
	m := New()
	err := m.ConfigureCustom(CustomConfig{Enabled: true, TTL: time.Hour, MaxSeriesPerMetric: 10}, f,
		func(string) (dsl.MetricShape, bool) { return dsl.MetricShape{Kind: "gauge", Help: "h"}, true })
	if err != nil {
		t.Fatal(err)
	}
	m.Custom().Emit(context.Background(), counterDef("a"), map[string]string{}, 1)
	if _, err := m.reg.Gather(); err != nil {
		t.Fatalf("gather must stay healthy: %v", err)
	}
	if got := testutil.ToFloat64(m.customErrors.WithLabelValues("unifiedcd_job_a_total", "conflict")); got != 1 {
		t.Fatalf("conflict = %v", got)
	}
}

func TestEmitter_HistogramPreValidation(t *testing.T) {
	e, _, errs := newTestEmitter(CustomConfig{MaxSeriesPerMetric: 10, TTL: time.Minute}, nil)
	h := dsl.MetricDef{Name: "bb", Help: "h", Histogram: &dsl.HistogramMetric{Buckets: []float64{5, 1}}}
	for i := 0; i < 3; i++ {
		e.Emit(context.Background(), h, map[string]string{}, 1)
	}
	if got := errCount(errs, "unifiedcd_job_bb", "conflict"); got != 3 {
		t.Fatalf("conflict = %v", got)
	}
	if len(e.vecs) != 0 {
		t.Fatal("rejected vec stored")
	}
}

func TestEmitter_SweepDropsEmptyVecFreeingFamilies(t *testing.T) {
	e, reg, errs := newTestEmitter(CustomConfig{MaxSeriesPerMetric: 10, TTL: time.Minute}, nil)
	ctx := context.Background()
	e.Emit(ctx, dsl.MetricDef{Name: "a_total", Help: "h", Histogram: &dsl.HistogramMetric{}}, map[string]string{}, 1)
	e.Emit(ctx, counterDef("a"), map[string]string{}, 1) // family clash
	if got := errCount(errs, "unifiedcd_job_a_total", "conflict"); got != 1 {
		t.Fatalf("conflict = %v", got)
	}
	e.Sweep(time.Now().Add(time.Hour))
	if len(e.vecs) != 0 {
		t.Fatal("empty vec kept")
	}
	e.Emit(ctx, counterDef("a"), map[string]string{}, 1)
	if n, _ := testutil.GatherAndCount(reg, "unifiedcd_job_a_total"); n != 1 {
		t.Fatalf("series = %d", n)
	}
}

func TestEmitter_ConcurrentEmitSweepGather(t *testing.T) {
	f := &fakeGauges{}
	m := New()
	if err := m.ConfigureCustom(CustomConfig{Enabled: true, TTL: time.Millisecond, MaxSeriesPerMetric: 50}, f,
		func(string) (dsl.MetricShape, bool) { return dsl.MetricShape{Kind: "gauge", Help: "h"}, true }); err != nil {
		t.Fatal(err)
	}
	e := m.Custom()
	ctx := context.Background()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				// Flip the label set (a shape change) between iterations.
				key := "a"
				if i%7 < 3 {
					key = "b"
				}
				e.Emit(ctx, counterDef("flip", key), map[string]string{key: "v"}, 1)
				e.Emit(ctx, dsl.MetricDef{Name: "hist", Help: "h", Histogram: &dsl.HistogramMetric{}}, map[string]string{}, 1)
				e.Emit(ctx, dsl.MetricDef{Name: "a_total", Help: "h", Histogram: &dsl.HistogramMetric{}}, map[string]string{}, 1)
				e.Emit(ctx, counterDef("a"), map[string]string{}, 1)
			}
		}(g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			e.Sweep(time.Now().Add(time.Hour))
			time.Sleep(time.Millisecond)
		}
	}()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := m.reg.Gather(); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("gather failed: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}
