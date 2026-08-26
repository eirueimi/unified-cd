package metrics

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scrape returns the rendered exposition text.
func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	fams := exportedFamiliesBody(t, m)
	return fams
}

// TestBackgroundTask_FailedItemsAreVisibleOnASucceedingPass is the reason the
// per-item counts exist.
//
// Several workers iterate a batch and swallow per-item failures so one bad
// item cannot abort the sweep — the archiver logs a run it could not archive
// and moves on, returning nil. Pass-level outcome therefore reports SUCCESS
// for a worker whose every item failed, which is exactly the silent breakage
// an operator needs to see. Without result="error" the dashboard cannot tell
// "nothing to archive" from "nothing archivable".
func TestBackgroundTask_FailedItemsAreVisibleOnASucceedingPass(t *testing.T) {
	m := New()
	m.BackgroundTask("log_archiver", 0, 7, 0.4, nil) // nil error, every item failed

	body := scrape(t, m)
	assert.Contains(t, body, `unifiedcd_background_task_runs_total{outcome="success",task="log_archiver"} 1`)
	assert.Contains(t, body, `unifiedcd_background_task_items_total{result="error",task="log_archiver"} 7`)
	assert.NotContains(t, body, `result="ok",task="log_archiver"`,
		"nothing succeeded, so no ok series should have been created")
}

// TestBackgroundTask_ErrorPassStillRecordsDuration keeps "failing slowly" and
// "failing fast" distinguishable — they are different problems.
func TestBackgroundTask_ErrorPassStillRecordsDuration(t *testing.T) {
	m := New()
	m.BackgroundTask("run_retention", 0, 0, 12.5, errors.New("lock timeout"))

	body := scrape(t, m)
	assert.Contains(t, body, `unifiedcd_background_task_runs_total{outcome="error",task="run_retention"} 1`)
	assert.Contains(t, body, `unifiedcd_background_task_duration_seconds_count{task="run_retention"} 1`)
}

// TestNilMetrics_RecordersAreNoOps guards the seam the background workers use:
// they hold a recorder that is nil until startup wires it, and every test in
// the controller package runs with it nil.
func TestNilMetrics_RecordersAreNoOps(t *testing.T) {
	var m *Metrics
	require.NotPanics(t, func() {
		m.BackgroundTask("x", 1, 1, 1, nil)
		m.LogsIngested(1, 1, 1)
		m.RunTimeToClaim(1)
	})
}

// TestRunTimeToClaim_NegativeIsDropped: a clock stepping backwards between
// creation and claim would otherwise pile a fictional instant-claim into the
// first bucket and make the p50 look better than reality.
func TestRunTimeToClaim_NegativeIsDropped(t *testing.T) {
	m := New()
	m.RunTimeToClaim(-3)
	assert.Contains(t, scrape(t, m), `unifiedcd_run_time_to_claim_seconds_count 0`)
}

// TestLogsIngested_CountsBytesForDroppedLinesToo: a sealed run still costs the
// ingress it spends, so the byte counter must not be conditioned on acceptance.
func TestLogsIngested_CountsBytesForDroppedLinesToo(t *testing.T) {
	m := New()
	m.LogsIngested(0, 4, 512)

	body := scrape(t, m)
	assert.Contains(t, body, `unifiedcd_log_lines_ingested_total{result="dropped"} 4`)
	assert.Contains(t, body, `unifiedcd_log_bytes_ingested_total 512`)
	assert.NotContains(t, body, `result="accepted"`)
}

func exportedFamiliesBody(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	require.Equal(t, 200, rec.Code)
	return rec.Body.String()
}
