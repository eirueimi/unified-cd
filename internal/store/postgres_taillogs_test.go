package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPostgres_TailLogsRecentByStep verifies the per-step tail query used by
// the WebUI's on-demand step-log backfill: only the given step's lines come
// back, capped to the most recent `limit`, in ascending seq order.
func TestPostgres_TailLogsRecentByStep(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "")
	require.NoError(t, err)

	now := time.Now().UTC()
	// Interleave: 3 lines for step 0, 5 lines for step 2.
	for i := 0; i < 3; i++ {
		_, err := pg.AppendLog(ctx, run.ID, 0, "stdout", now, fmt.Sprintf("zero-%d", i))
		require.NoError(t, err)
	}
	for i := 0; i < 5; i++ {
		_, err := pg.AppendLog(ctx, run.ID, 2, "stdout", now, fmt.Sprintf("two-%d", i))
		require.NoError(t, err)
	}

	// All of step 0's lines, none of step 2's.
	lines, err := pg.TailLogsRecentByStep(ctx, run.ID, 0, 100)
	require.NoError(t, err)
	require.Len(t, lines, 3)
	for i, l := range lines {
		assert.Equal(t, 0, l.StepIndex)
		assert.Equal(t, fmt.Sprintf("zero-%d", i), l.Line, "ascending seq order")
	}

	// Cap keeps the most RECENT lines of the step.
	lines, err = pg.TailLogsRecentByStep(ctx, run.ID, 2, 2)
	require.NoError(t, err)
	require.Len(t, lines, 2)
	assert.Equal(t, "two-3", lines[0].Line)
	assert.Equal(t, "two-4", lines[1].Line)

	// Unknown step: empty, no error.
	lines, err = pg.TailLogsRecentByStep(ctx, run.ID, 9, 10)
	require.NoError(t, err)
	assert.Empty(t, lines)
}

func TestPostgres_LogsRangeStatsSearch(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "")
	require.NoError(t, err)
	other, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "")
	require.NoError(t, err)
	now := time.Now().UTC()
	// 他 run の行が混入しないことの検証用
	_, err = pg.AppendLog(ctx, other.ID, 0, "stdout", now, "other-run-line")
	require.NoError(t, err)
	// step0: 3行 / step2: 5行(うち1行に "needle_50%" — ILIKE メタ文字入り)
	for i := 0; i < 3; i++ {
		_, err := pg.AppendLog(ctx, run.ID, 0, "stdout", now, fmt.Sprintf("zero-%d", i))
		require.NoError(t, err)
	}
	for i := 0; i < 5; i++ {
		line := fmt.Sprintf("two-%d", i)
		if i == 3 {
			line = "found needle_50% here"
		}
		_, err := pg.AppendLog(ctx, run.ID, 2, "stdout", now, line)
		require.NoError(t, err)
	}

	// --- CountLogs ---
	count, minSeq, maxSeq, err := pg.CountLogs(ctx, run.ID, nil)
	require.NoError(t, err)
	assert.EqualValues(t, 8, count)
	assert.Less(t, minSeq, maxSeq)
	count, _, _, err = pg.CountLogs(ctx, run.ID, []int{2})
	require.NoError(t, err)
	assert.EqualValues(t, 5, count)
	count, _, _, err = pg.CountLogs(ctx, run.ID, []int{7})
	require.NoError(t, err)
	assert.EqualValues(t, 0, count)

	// --- ListLogsRange(全体ビュー: 行番号は step0 の3行が 0..2、step2 が 3..7)---
	lines, err := pg.ListLogsRange(ctx, run.ID, nil, 2, 3)
	require.NoError(t, err)
	require.Len(t, lines, 3)
	assert.Equal(t, "zero-2", lines[0].Line)
	assert.Equal(t, "two-0", lines[1].Line)
	// steps ビュー: offset はビュー内アドレス
	lines, err = pg.ListLogsRange(ctx, run.ID, []int{2}, 0, 2)
	require.NoError(t, err)
	require.Len(t, lines, 2)
	assert.Equal(t, "two-0", lines[0].Line)
	// 末尾越え offset は空
	lines, err = pg.ListLogsRange(ctx, run.ID, nil, 100, 10)
	require.NoError(t, err)
	assert.Empty(t, lines)

	// --- SearchLogs(ILIKE メタ文字がリテラル扱いされること)---
	total, matches, err := pg.SearchLogs(ctx, run.ID, nil, "needle_50%", 10)
	require.NoError(t, err)
	assert.EqualValues(t, 1, total)
	require.Len(t, matches, 1)
	assert.EqualValues(t, 6, matches[0].Row) // 全体ビューで 0-based 7行目(zero×3 + two-0..2 の次)
	assert.Equal(t, 2, matches[0].StepIndex)
	// "_" はリテラル: "needle_" は1件、ワイルドカードとして "needleX" 等を拾わない
	total, _, err = pg.SearchLogs(ctx, run.ID, nil, "zero-", 2) // cap < total
	require.NoError(t, err)
	assert.EqualValues(t, 3, total)
	total, matches, err = pg.SearchLogs(ctx, run.ID, []int{0}, "o", 100) // steps ビュー内 Row
	require.NoError(t, err)
	require.NotEmpty(t, matches)
	assert.EqualValues(t, 0, matches[0].Row)
	_ = total
}
