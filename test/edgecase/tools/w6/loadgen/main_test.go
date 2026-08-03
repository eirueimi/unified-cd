package main

// Proves the two concurrency guards BY EFFECT rather than by inspection, which
// is the W6 rule for every instrument in this directory. Neither case can be
// produced by running the tool correctly — an over-report is by definition an
// instrument fault — so the records are fabricated and `summarize`'s real
// stdout is read back.
//
// Run: go test -buildvcs=false ./test/edgecase/tools/w6/loadgen/

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// captureSummarize runs summarize with stdout redirected and returns what it
// printed.
func captureSummarize(t *testing.T, conc int, serial bool, recs []record) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	var lo, hi time.Time
	for i, rec := range recs {
		if i == 0 || rec.start.Before(lo) {
			lo = rec.start
		}
		if i == 0 || rec.end.After(hi) {
			hi = rec.end
		}
	}
	summarize("guardtest", "burst", serial, conc, recs, lo, hi)
	w.Close()
	os.Stdout = orig
	return <-done
}

func mk(n int, start time.Time, dur time.Duration) []record {
	recs := make([]record, 0, n)
	for i := 0; i < n; i++ {
		recs = append(recs, record{seq: i + 1, worker: i, start: start, end: start.Add(dur), code: 200})
	}
	return recs
}

// A clean -c 8 run: 8 overlapping requests, maxInFlight 8, no warning at all.
func TestNoWarningWhenMaxEqualsConc(t *testing.T) {
	base := time.Date(2026, 8, 2, 3, 57, 56, 0, time.UTC)
	out := captureSummarize(t, 8, false, mk(8, base, 20*time.Millisecond))
	if !strings.Contains(out, "maxInFlight=8") {
		t.Fatalf("expected maxInFlight=8, got:\n%s", out)
	}
	if strings.Contains(out, "WARNING") {
		t.Fatalf("clean run must not warn, got:\n%s", out)
	}
}

// The pre-existing guard: the rig serialised the requests.
func TestWarnsOnUnderReport(t *testing.T) {
	base := time.Date(2026, 8, 2, 3, 57, 56, 0, time.UTC)
	recs := []record{
		{seq: 1, start: base, end: base.Add(10 * time.Millisecond), code: 200},
		{seq: 2, start: base.Add(10 * time.Millisecond), end: base.Add(20 * time.Millisecond), code: 200},
	}
	out := captureSummarize(t, 8, false, recs)
	if !strings.Contains(out, "maxInFlight (1) < -c (8)") {
		t.Fatalf("expected the under-report warning, got:\n%s", out)
	}
}

// The guard this file exists for. 12 requests from a nominal 8-worker pool is
// impossible; before this guard it printed maxInFlight=12 and said nothing,
// which is how `maxInFlight=19` for `-c 8` reached a FINDINGS entry.
func TestWarnsOnOverReport(t *testing.T) {
	base := time.Date(2026, 8, 2, 3, 57, 56, 0, time.UTC)
	out := captureSummarize(t, 8, false, mk(12, base, 20*time.Millisecond))
	if !strings.Contains(out, "maxInFlight=12") {
		t.Fatalf("expected maxInFlight=12, got:\n%s", out)
	}
	if !strings.Contains(out, "maxInFlight (12) > -c (8)") {
		t.Fatalf("OVER-REPORT PASSED SILENTLY — the guard is inert. Output:\n%s", out)
	}
	if !strings.Contains(out, "Do NOT report this number") {
		t.Fatalf("guard fired without the do-not-report instruction:\n%s", out)
	}
}

// The negative control must report exactly 1. This is the specific regression
// the end-first tie-break fixed; it is now asserted rather than remembered.
func TestSerialControlMustReportOne(t *testing.T) {
	base := time.Date(2026, 8, 2, 3, 57, 56, 0, time.UTC)
	// Back-to-back requests sharing a timestamp at the seam — the exact shape
	// Windows' coarse clock produces, and the shape a start-first tie-break
	// mis-reads as overlap.
	recs := []record{
		{seq: 1, start: base, end: base.Add(10 * time.Millisecond), code: 200},
		{seq: 2, start: base.Add(10 * time.Millisecond), end: base.Add(20 * time.Millisecond), code: 200},
		{seq: 3, start: base.Add(20 * time.Millisecond), end: base.Add(30 * time.Millisecond), code: 200},
	}
	out := captureSummarize(t, 20, true, recs)
	if !strings.Contains(out, "maxInFlight=1") {
		t.Fatalf("the serial control must report maxInFlight=1, got:\n%s", out)
	}
	if strings.Contains(out, "the SERIAL negative control reported") {
		t.Fatalf("serial guard fired on a correct serial run:\n%s", out)
	}
}
