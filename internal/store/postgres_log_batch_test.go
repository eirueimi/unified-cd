package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newRun creates a job and a run, returning the run ID.
func newRun(t *testing.T, pg *Postgres, job string) string {
	t.Helper()
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, job, "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(ctx, job, nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	return run.ID
}

// TestPostgres_AppendLogs_MixedSealedAndLive is the core mapping test the
// design calls for: a batch straddling a sealed run and a live one must
// return zeros for the sealed run's lines and ascending seqs for the live
// one, positionally aligned with the input.
func TestPostgres_AppendLogs_MixedSealedAndLive(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	live := newRun(t, pg, "live")
	sealed := newRun(t, pg, "sealed")

	// Seal the second run.
	seq, err := pg.AppendLog(ctx, sealed, 0, "stdout", time.Now(), "before seal")
	require.NoError(t, err)
	require.NoError(t, pg.CreateLogArchive(ctx, sealed, "runs/"+sealed+"/logs.ndjson", 1, 1, seq))

	now := time.Now().UTC()
	// Interleave the two runs so a per-run grouping bug shows up as a
	// misalignment rather than a clean split.
	lines := []LogAppend{
		{RunID: live, StepIndex: 0, Stream: "stdout", Timestamp: now, Line: "live-1"},
		{RunID: sealed, StepIndex: 0, Stream: "stdout", Timestamp: now, Line: "dropped-1"},
		{RunID: live, StepIndex: 0, Stream: "stdout", Timestamp: now, Line: "live-2"},
		{RunID: sealed, StepIndex: 0, Stream: "stdout", Timestamp: now, Line: "dropped-2"},
		{RunID: live, StepIndex: 1, Stream: "stderr", Timestamp: now, Line: "live-3"},
	}

	seqs, err := pg.AppendLogs(ctx, lines)
	require.NoError(t, err)
	require.Len(t, seqs, len(lines))

	assert.Zero(t, seqs[1], "sealed run's line must be dropped")
	assert.Zero(t, seqs[3], "sealed run's line must be dropped")
	assert.Positive(t, seqs[0])
	assert.Positive(t, seqs[2])
	assert.Positive(t, seqs[4])
	assert.Less(t, seqs[0], seqs[2], "seqs must ascend in input order")
	assert.Less(t, seqs[2], seqs[4], "seqs must ascend in input order")

	// Nothing landed in the sealed run beyond its pre-seal line.
	count, _, _, err := pg.CountLogs(ctx, sealed, nil)
	require.NoError(t, err)
	assert.EqualValues(t, 1, count)

	// The live run has exactly the three lines, in order.
	got, err := pg.TailLogs(ctx, live, 0, 100)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, "live-1", got[0].Line)
	assert.Equal(t, "live-2", got[1].Line)
	assert.Equal(t, "live-3", got[2].Line)
	assert.EqualValues(t, 1, got[2].StepIndex)
	assert.Equal(t, "stderr", got[2].Stream)
}

// TestPostgres_AppendLogs_AllSealed: a batch whose every line belongs to a
// sealed run returns all zeros and stores nothing.
func TestPostgres_AppendLogs_AllSealed(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	run := newRun(t, pg, "j")

	seq, err := pg.AppendLog(ctx, run, 0, "stdout", time.Now(), "before seal")
	require.NoError(t, err)
	require.NoError(t, pg.CreateLogArchive(ctx, run, "runs/"+run+"/logs.ndjson", 1, 1, seq))

	seqs, err := pg.AppendLogs(ctx, []LogAppend{
		{RunID: run, StepIndex: 0, Stream: "stdout", Timestamp: time.Now().UTC(), Line: "a"},
		{RunID: run, StepIndex: 0, Stream: "stdout", Timestamp: time.Now().UTC(), Line: "b"},
	})
	require.NoError(t, err)
	assert.Equal(t, []int64{0, 0}, seqs)

	count, _, _, err := pg.CountLogs(ctx, run, nil)
	require.NoError(t, err)
	assert.EqualValues(t, 1, count)
}

// TestPostgres_AppendLogs_Empty: an empty batch is a no-op, not an error and
// not a round trip. handleAgentLogBulk can receive an empty array.
func TestPostgres_AppendLogs_Empty(t *testing.T) {
	pg := NewTestPostgres(t)
	seqs, err := pg.AppendLogs(context.Background(), nil)
	require.NoError(t, err)
	assert.Empty(t, seqs)
}

// TestPostgres_AppendLogs_OrderIsProvenNotAssumed inserts a large batch and
// reads it back, asserting the line stored at the i-th ascending seq is the
// i-th input line. This is what proves `ORDER BY ord` actually fixes the
// order seqs are assigned in — the plan's one load-bearing assumption.
func TestPostgres_AppendLogs_OrderIsProvenNotAssumed(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	run := newRun(t, pg, "j")

	const n = 500
	lines := make([]LogAppend, n)
	now := time.Now().UTC()
	for i := range lines {
		lines[i] = LogAppend{
			RunID: run, StepIndex: 0, Stream: "stdout",
			Timestamp: now, Line: fmt.Sprintf("line-%03d", i),
		}
	}

	seqs, err := pg.AppendLogs(ctx, lines)
	require.NoError(t, err)
	require.Len(t, seqs, n)
	for i := 1; i < n; i++ {
		require.Less(t, seqs[i-1], seqs[i], "seq must ascend at index %d", i)
	}

	got, err := pg.TailLogs(ctx, run, 0, n+10)
	require.NoError(t, err)
	require.Len(t, got, n)
	for i := range got {
		require.Equal(t, lines[i].Line, got[i].Line, "line at position %d", i)
		require.Equal(t, seqs[i], got[i].Seq, "seq at position %d", i)
	}
}
