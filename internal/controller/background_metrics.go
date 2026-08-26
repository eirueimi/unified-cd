package controller

import (
	"sync/atomic"
	"time"

	"github.com/eirueimi/unified-cd/internal/metrics"
)

// backgroundMetrics is the recorder every background worker reports to.
//
// It is package-level rather than a parameter because the twelve workers are
// package-level functions with forty-odd call sites, almost all of them tests
// that have no opinion about metrics. Threading a recorder through every one
// of them would churn those tests to express something none of them care
// about. Instrumentation is a genuine cross-cutting concern here, and this is
// the narrowest form it can take: write-once at startup, read-only after.
//
// atomic.Pointer rather than a plain variable because main.go sets it on the
// startup goroutine while workers read it on their own; nil until then, and
// observePass tolerates nil so every test keeps working untouched.
var backgroundMetrics atomic.Pointer[metrics.Metrics]

// SetBackgroundMetrics wires background workers to a metrics recorder. Call
// once during startup, before the workers are launched.
func SetBackgroundMetrics(m *metrics.Metrics) { backgroundMetrics.Store(m) }

// observePass times one pass of a background worker and records its outcome.
//
// The pass function returns how many items succeeded, how many failed, and
// its own error, so the counts, the duration and the outcome are recorded from
// ONE place. Failed items are separate from the pass error because these
// workers iterate batches and keep going past a bad item — a pass can return
// nil while every item in it failed. Every
// one of these workers runs on a ticker with no caller waiting on it, so a
// pass that fails every time has nothing else to surface it — the archiver in
// particular can stop archiving indefinitely while every other controller
// signal stays healthy.
//
// A pass that returns an error still records its duration: "failing slowly"
// and "failing fast" are different problems and the histogram should show
// both.
func observePass(task string, fn func() (ok, failed int, err error)) {
	start := time.Now()
	ok, failed, err := fn()
	if m := backgroundMetrics.Load(); m != nil {
		m.BackgroundTask(task, ok, failed, time.Since(start).Seconds(), err)
	}
}
