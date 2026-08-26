package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// exportedFamilies scrapes m and returns the set of metric family names.
func exportedFamilies(t *testing.T, m *Metrics) map[string]bool {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	require.Equal(t, 200, rec.Code)
	out := map[string]bool{}
	for _, ln := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(ln, "# TYPE ") {
			out[strings.Fields(ln)[2]] = true
		}
	}
	return out
}

// TestNew_ExportsGoRuntimeMetrics guards the side effect of using a private
// registry.
//
// prometheus.DefaultRegisterer auto-registers the Go and process collectors;
// prometheus.NewRegistry() starts completely empty. New() deliberately uses
// the latter so several Server instances can coexist in one test binary — a
// correct decision that silently cost every go_* and process_* series. A
// scrape of the controller returned application metrics only, so a goroutine
// leak, a memory climb toward an OOMKill, or GC pressure behind a latency
// regression were all invisible to an operator's dashboard.
func TestNew_ExportsGoRuntimeMetrics(t *testing.T) {
	families := exportedFamilies(t, New())

	for _, want := range []string{
		"go_goroutines",
		"go_memstats_alloc_bytes",
		"go_gc_duration_seconds",
		"go_threads",
	} {
		assert.True(t, families[want], "%s missing: the Go collector is not registered", want)
	}
	for _, want := range []string{
		"process_resident_memory_bytes",
		"process_cpu_seconds_total",
		"process_open_fds",
	} {
		assert.True(t, families[want], "%s missing: the process collector is not registered", want)
	}
}

// TestNew_TwoInstancesCoexist is the property the private registry exists to
// provide, re-asserted now that the collectors are registered on it. The Go
// and process collectors are the two that panic on duplicate registration
// against a SHARED registry, so this is exactly where switching to the global
// one would bite — and a test binary constructing two Servers is not
// hypothetical, it is how this package's own tests run.
func TestNew_TwoInstancesCoexist(t *testing.T) {
	require.NotPanics(t, func() {
		a, b := New(), New()
		assert.True(t, exportedFamilies(t, a)["go_goroutines"])
		assert.True(t, exportedFamilies(t, b)["go_goroutines"])
	})
}

// TestNew_ApplicationMetricsStillExported proves the addition did not disturb
// what was already there: build_info carries a label so it appears as soon as
// it is set, and the collector-error counter is unlabelled so it exports at 0.
func TestNew_ApplicationMetricsStillExported(t *testing.T) {
	m := New()
	m.SetBuildInfo("v0.7.0")
	families := exportedFamilies(t, m)
	assert.True(t, families["unifiedcd_build_info"])
	assert.True(t, families["unifiedcd_scrape_collector_errors_total"])
}
