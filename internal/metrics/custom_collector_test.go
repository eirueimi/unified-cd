package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func newCollectorReg(t *testing.T, f *fakeGauges, cfg CustomConfig, now time.Time, shapes map[string][]string) (*prometheus.Registry, *prometheus.CounterVec) {
	t.Helper()
	reg := prometheus.NewRegistry()
	errs := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "e_total", Help: "x"}, []string{"metric", "reason"})
	cfg.Enabled = true
	err := RegisterCustomCollector(reg, cfg, f, func(name string) (dsl.MetricShape, bool) {
		k, ok := shapes[name]
		return dsl.MetricShape{Kind: "gauge", LabelKeys: k}, ok
	}, errs, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	return reg, errs
}

func TestDBCollector_DedupesRealtime(t *testing.T) {
	now := time.Now()
	f := &fakeGauges{realtime: []store.MetricRealtime{
		{RunID: "r1", Name: "unifiedcd_job_rt", Help: "h", Labels: map[string]string{"a": "1", "b": "2"}, StartedAt: now.Add(-10 * time.Second)},
		{RunID: "r2", Name: "unifiedcd_job_rt", Help: "h", Labels: map[string]string{"b": "2", "a": "1"}, StartedAt: now.Add(-30 * time.Second)},
	}}
	reg, _ := newCollectorReg(t, f, CustomConfig{TTL: time.Hour}, now, map[string][]string{"unifiedcd_job_rt": {"a", "b"}})
	n, err := testutil.GatherAndCount(reg, "unifiedcd_job_rt")
	if err != nil || n != 1 {
		t.Fatalf("count = %d, %v", n, err)
	}
	exp := "# HELP unifiedcd_job_rt h\n# TYPE unifiedcd_job_rt gauge\nunifiedcd_job_rt{a=\"1\",b=\"2\"} 30\n"
	if err := testutil.GatherAndCompare(reg, strings.NewReader(exp), "unifiedcd_job_rt"); err != nil {
		t.Fatal(err)
	}
}

func TestDBCollector_SkipsShapeMismatch(t *testing.T) {
	now := time.Now()
	f := &fakeGauges{gauges: []store.MetricGauge{
		{Name: "unifiedcd_job_ok", Help: "h", Labels: map[string]string{"a": "1"}, Value: 4, UpdatedAt: now},
		{Name: "unifiedcd_job_stale", Help: "h", Labels: map[string]string{"old": "1"}, Value: 1, UpdatedAt: now},
		{Name: "unifiedcd_job_unknown", Help: "h", Labels: map[string]string{}, Value: 1, UpdatedAt: now},
	}}
	reg, errs := newCollectorReg(t, f, CustomConfig{TTL: time.Hour}, now,
		map[string][]string{"unifiedcd_job_ok": {"a"}, "unifiedcd_job_stale": {"new"}})
	mfs, err := reg.Gather()
	if err != nil || len(mfs) != 1 || mfs[0].GetName() != "unifiedcd_job_ok" {
		t.Fatalf("gather = %v, %v", mfs, err)
	}
	if testutil.ToFloat64(errs.WithLabelValues("unifiedcd_job_stale", "conflict")) != 1 ||
		testutil.ToFloat64(errs.WithLabelValues("unifiedcd_job_unknown", "conflict")) != 1 {
		t.Fatal("conflicts not counted")
	}
}

func TestDBCollector_TTLFilter(t *testing.T) {
	now := time.Now()
	f := &fakeGauges{gauges: []store.MetricGauge{
		{Name: "unifiedcd_job_g", Help: "h", Labels: map[string]string{"a": "new"}, Value: 1, UpdatedAt: now.Add(-time.Minute)},
		{Name: "unifiedcd_job_g", Help: "h", Labels: map[string]string{"a": "old"}, Value: 1, UpdatedAt: now.Add(-2 * time.Hour)},
	}}
	reg, _ := newCollectorReg(t, f, CustomConfig{TTL: time.Hour}, now, map[string][]string{"unifiedcd_job_g": {"a"}})
	if n, err := testutil.GatherAndCount(reg, "unifiedcd_job_g"); err != nil || n != 1 {
		t.Fatalf("count = %d, %v", n, err)
	}
	if !f.since.Equal(now.Add(-time.Hour)) {
		t.Fatalf("since = %v", f.since)
	}
}

func TestDBCollector_ConfigureCustomEndToEnd(t *testing.T) {
	now := time.Now()
	f := &fakeGauges{gauges: []store.MetricGauge{
		{Name: "unifiedcd_job_g", Help: "h", Labels: map[string]string{}, Value: 2, UpdatedAt: now},
	}}
	m := New()
	if err := m.ConfigureCustom(CustomConfig{Enabled: true, TTL: time.Hour}, f, func(string) (dsl.MetricShape, bool) {
		return dsl.MetricShape{Kind: "gauge"}, true
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := testutil.GatherAndCount(m.reg, "unifiedcd_job_g"); err != nil || n != 1 {
		t.Fatalf("count = %d, %v", n, err)
	}
}
