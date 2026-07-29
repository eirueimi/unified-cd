//go:build edgeprobe

package controller

// Observational probes for scheduler clock/TZ boundary behavior (campaign
// scenario W0-2 / C14 — see docs/superpowers/specs/2026-07-29-edge-case-
// testing-design.md). These probes PASS unless infrastructure fails; the
// t.Logf output IS the result and is copied into test/edgecase/FINDINGS.md.
// Run: go test -tags edgeprobe ./internal/controller -run TestEdgeProbe -v

import (
	"context"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/require"
)

func probeSetup(t *testing.T) *store.Postgres {
	t.Helper()
	pg := store.NewTestPostgres(t)
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "probe-job", "unified-cd/v1",
		[]byte(`{"steps":[{"name":"s","run":"echo x"}]}`))
	require.NoError(t, err)
	return pg
}

func countRuns(t *testing.T, pg *store.Postgres) int {
	t.Helper()
	runs, err := pg.ListRunsByJob(context.Background(), "probe-job", 100)
	require.NoError(t, err)
	return len(runs)
}

// TestEdgeProbe_DSTGap: "30 2 * * *" on the US spring-forward night —
// 02:30 does not exist on 2026-03-08 in America/New_York.
func TestEdgeProbe_DSTGap(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	base := time.Date(2026, 3, 8, 1, 0, 0, 0, ny)
	next, err := dsl.NextCronTime("30 2 * * *", base)
	require.NoError(t, err)
	t.Logf("DST gap: next('30 2 * * *') after %s = %s (skipped? %v)",
		base, next, next.Day() != 8)
}

// TestEdgeProbe_DSTFold: "30 1 * * *" on the US fall-back night —
// 01:30 occurs twice on 2026-11-01 in America/New_York.
func TestEdgeProbe_DSTFold(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, ny)
	first, err := dsl.NextCronTime("30 1 * * *", base)
	require.NoError(t, err)
	second, err := dsl.NextCronTime("30 1 * * *", first)
	require.NoError(t, err)
	t.Logf("DST fold: first=%s (%s) second=%s (%s) — fires twice on the fold night? %v",
		first, first.UTC(), second, second.UTC(), second.Sub(first) == time.Hour)
}

// TestEdgeProbe_CatchupWindowBoundary: the fire window is [now-1h, now].
// With cron */5, last_fired = now-65m puts next exactly AT now-60m
// (boundary), and last_fired = now-66m puts next just OUTSIDE it.
func TestEdgeProbe_CatchupWindowBoundary(t *testing.T) {
	pg := probeSetup(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	_, err := pg.UpsertSchedule(ctx, "probe-sched", "*/5 * * * *", "probe-job", nil)
	require.NoError(t, err)

	// Case A: next == now-60m exactly (on the window edge).
	require.NoError(t, pg.UpdateScheduleLastFiredAt(ctx, "probe-sched", now.Add(-65*time.Minute)))
	checkAndFireSchedules(ctx, pg, now)
	after := countRuns(t, pg)
	scA, err := pg.GetSchedule(ctx, "probe-sched")
	require.NoError(t, err)
	t.Logf("boundary next==now-1h: fired=%d last_fired_at=%v", after, scA.LastFiredAt)

	// Case B: next == now-61m (just outside) — expect silent advance, no run.
	require.NoError(t, pg.UpdateScheduleLastFiredAt(ctx, "probe-sched", now.Add(-66*time.Minute)))
	before := countRuns(t, pg)
	checkAndFireSchedules(ctx, pg, now)
	scB, err := pg.GetSchedule(ctx, "probe-sched")
	require.NoError(t, err)
	t.Logf("just outside window: new-runs=%d (silent skip? %v) last_fired_at=%v",
		countRuns(t, pg)-before, countRuns(t, pg) == before, scB.LastFiredAt)
}

// TestEdgeProbe_BacklogDrainRate: a 30-minute outage with cron */5 leaves 6
// due occurrences inside the window. checkAndFireSchedules computes ONE next
// per schedule per call — measure how many calls drain the backlog.
func TestEdgeProbe_BacklogDrainRate(t *testing.T) {
	pg := probeSetup(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	_, err := pg.UpsertSchedule(ctx, "probe-sched", "*/5 * * * *", "probe-job", nil)
	require.NoError(t, err)
	require.NoError(t, pg.UpdateScheduleLastFiredAt(ctx, "probe-sched", now.Add(-30*time.Minute)))

	for call := 1; call <= 8; call++ {
		checkAndFireSchedules(ctx, pg, now)
		t.Logf("call %d: total runs=%d", call, countRuns(t, pg))
	}
	// Production calls this once per minute: if each call fires one backlog
	// occurrence, a 30-min outage takes ~6 real minutes to drain.
}

// TestEdgeProbe_TZDivergentLeaders: replicas in different TZs. A UTC leader
// checks, then "fails over" to a JST leader one minute later, same instant
// stream. Cron interprets wall-clock in the Location carried by `now`/base.
func TestEdgeProbe_TZDivergentLeaders(t *testing.T) {
	pg := probeSetup(t)
	ctx := context.Background()
	jst, err := time.LoadLocation("Asia/Tokyo")
	require.NoError(t, err)

	_, err = pg.UpsertSchedule(ctx, "probe-sched", "0 9 * * *", "probe-job", nil)
	require.NoError(t, err)

	// 2026-07-29 00:05 UTC == 09:05 JST: "daily at 09:00" is 5 minutes PAST
	// due for a JST replica and ~9h away for a UTC replica.
	nowUTC := time.Date(2026, 7, 29, 0, 5, 0, 0, time.UTC)
	checkAndFireSchedules(ctx, pg, nowUTC)
	afterUTC := countRuns(t, pg)
	checkAndFireSchedules(ctx, pg, nowUTC.Add(time.Minute).In(jst))
	afterJST := countRuns(t, pg)
	sc, err := pg.GetSchedule(ctx, "probe-sched")
	require.NoError(t, err)
	t.Logf("UTC-leader fired=%d, JST-takeover fired=%d, last_fired_at=%v — TZ divergence causes skip/dup? utc=%d jst=%d",
		afterUTC, afterJST-afterUTC, sc.LastFiredAt, afterUTC, afterJST-afterUTC)
}

// TestEdgeProbe_BackwardClockStep: last_fired_at ahead of now (NTP step
// back / a fast-clock leader wrote it). How long does scheduling stall?
func TestEdgeProbe_BackwardClockStep(t *testing.T) {
	pg := probeSetup(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	_, err := pg.UpsertSchedule(ctx, "probe-sched", "* * * * *", "probe-job", nil)
	require.NoError(t, err)
	// A leader 10 minutes fast stamped last_fired_at in our future.
	require.NoError(t, pg.UpdateScheduleLastFiredAt(ctx, "probe-sched", now.Add(10*time.Minute)))

	checkAndFireSchedules(ctx, pg, now)
	t.Logf("with future last_fired_at: fired=%d (every-minute schedule silent until wall clock catches up: %v)",
		countRuns(t, pg), countRuns(t, pg) == 0)
}
