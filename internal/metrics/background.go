package metrics

import (
	"sync/atomic"
	"time"
)

// backgroundRecorder is the Metrics that background workers report to.
//
// It lives here, next to construction, rather than in the package that owns
// the workers — because construction is the only moment at which it can be
// set correctly, and a separate "now also wire the workers" call in main.go is
// a step that can be forgotten. NewForController sets it, so a controller that
// has metrics at all has instrumented workers by construction.
//
// It is package-level rather than a parameter because the workers are
// package-level functions with forty-odd call sites, almost all of them tests
// that have no opinion about metrics. Threading a recorder through every one
// of them would churn those tests to express something none of them assert.
//
// atomic.Pointer because it is written on the startup goroutine and read from
// each worker's own; nil until set, and ObservePass tolerates nil so every
// test keeps working untouched.
var backgroundRecorder atomic.Pointer[Metrics]

// SetBackgroundRecorder overrides the recorder background workers report to.
//
// NewForController already does this, which is how production wires it. This
// exists for tests that need to install a recorder without a database, or to
// restore nil afterwards so one test's registry does not receive another
// test's observations.
func SetBackgroundRecorder(m *Metrics) { backgroundRecorder.Store(m) }

// ObservePass times one pass of a background worker and records its outcome.
//
// The pass function returns how many items succeeded, how many failed, and its
// own error, so the counts, the duration and the outcome are recorded from ONE
// place. Failed items are separate from the pass error because these workers
// iterate batches and keep going past a bad item — a pass can return nil while
// every item in it failed.
//
// Every one of these workers runs on a ticker with no caller waiting on it, so
// a pass that fails every time has nothing else to surface it: the log
// archiver can stop archiving indefinitely while every other controller signal
// stays healthy.
//
// A pass that returns an error still records its duration: "failing slowly"
// and "failing fast" are different problems and the histogram should show both.
func ObservePass(task string, fn func() (ok, failed int, err error)) {
	start := time.Now()
	ok, failed, err := fn()
	backgroundRecorder.Load().BackgroundTask(task, ok, failed, time.Since(start).Seconds(), err)
}
