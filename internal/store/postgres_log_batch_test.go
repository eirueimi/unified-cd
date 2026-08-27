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
	run, err := pg.CreateRun(ctx, job, nil, []byte(`{}`), nil, nil, "", "")
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

// TestPostgres_AppendLogs_NULByte_DiffersFromAppendLog pins down, by
// measurement rather than assumption, the all-or-nothing-per-run property
// documented on AppendLogs: a line containing an embedded NUL byte makes
// PostgreSQL reject the whole statement (SQLSTATE 22021, "invalid byte
// sequence for encoding UTF8"), which costs a run its entire share of the
// batch instead of just the one bad line.
//
// The two methods differ in exactly the way that comment describes:
//
//   - AppendLogs groups a run's lines into one INSERT statement. When any
//     line in that statement is rejected, the whole statement errors and
//     NOTHING for that run in that batch lands -- not even the good lines
//     that sat on either side of the bad one. AppendLogs returns a non-nil
//     error and a nil seqs slice.
//   - AppendLog issues one statement per line. The bad line's call returns
//     an error and (0, err); the good calls before and after it are
//     independent statements and land normally.
//
// This is an accepted, intentional property, not a defect: the agent's
// retry queue (LogPusher.flushPendingLocked, internal/agent/runner.go)
// resends a failed batch oldest-first and stops at the first failure either
// way, so walked through a full retry loop rather than one request, the two
// methods converge on the same outcome -- the poison line wedges that run's
// log until drop-oldest eviction, on the same schedule, either way. The only
// difference is that today's per-line path lands the lines before the
// poison line on every retry attempt, duplicating them, where the batch path
// lands none of them and produces no duplicates. AppendLogs is therefore
// strictly better on the duplicate axis and neutral on the wedge axis; this
// test exists to keep that measurement honest, not to flag a regression.
func TestPostgres_AppendLogs_NULByte_DiffersFromAppendLog(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	now := time.Now().UTC()
	const bad = "b\x00d" // embedded NUL: PostgreSQL text rejects this

	t.Run("AppendLogs_wholeRunBatchIsLost", func(t *testing.T) {
		run := newRun(t, pg, "batch-nul")
		lines := []LogAppend{
			{RunID: run, StepIndex: 0, Stream: "stdout", Timestamp: now, Line: "before"},
			{RunID: run, StepIndex: 0, Stream: "stdout", Timestamp: now, Line: bad},
			{RunID: run, StepIndex: 0, Stream: "stdout", Timestamp: now, Line: "after"},
		}
		seqs, err := pg.AppendLogs(ctx, lines)

		require.Error(t, err, "the run's single INSERT statement must fail")
		assert.Contains(t, err.Error(), "22021", "expected Postgres's invalid-byte-sequence SQLSTATE")
		assert.Nil(t, seqs)

		count, _, _, cErr := pg.CountLogs(ctx, run, nil)
		require.NoError(t, cErr)
		assert.EqualValues(t, 0, count, "neither the bad line nor the good lines around it land")
	})

	t.Run("AppendLog_onlyTheBadLineIsLost", func(t *testing.T) {
		run := newRun(t, pg, "perline-nul")

		seq1, err1 := pg.AppendLog(ctx, run, 0, "stdout", now, "before")
		require.NoError(t, err1)
		assert.Positive(t, seq1)

		seq2, err2 := pg.AppendLog(ctx, run, 0, "stdout", now, bad)
		require.Error(t, err2, "the bad line's own statement must fail")
		assert.Contains(t, err2.Error(), "22021")
		assert.Zero(t, seq2)

		seq3, err3 := pg.AppendLog(ctx, run, 0, "stdout", now, "after")
		require.NoError(t, err3, "a later independent statement is unaffected by the earlier failure")
		assert.Positive(t, seq3)
		assert.Less(t, seq1, seq3)

		count, _, _, cErr := pg.CountLogs(ctx, run, nil)
		require.NoError(t, cErr)
		assert.EqualValues(t, 2, count, "the two good lines land; only the bad one is dropped")
	})
}

// TestPostgres_AppendLogs_ThreeLiveRunsPlusSealed exercises the per-run
// round-trip fan-out beyond the two-run case: a batch interleaving three
// distinct live runs and a fourth, sealed run must produce independent,
// positionally-aligned, ascending seqs for each live run and all zeros for
// the sealed one. Interleaving (rather than grouping runs contiguously in
// the input) is what would expose a bug that let one run's grouping bleed
// into another's.
//
// The fan-out itself -- one INSERT and one pg_notify per distinct run -- is
// structural (one loop iteration per entry in `order` in AppendLogs) rather
// than directly observable through the Store interface, so it is not
// asserted by request/round-trip count here. What is asserted is the
// observable consequence that only correct per-run grouping produces: each
// run's own seqs strictly ascend in that run's own input order, independent
// of the other runs and of the sealed run's zeros.
func TestPostgres_AppendLogs_ThreeLiveRunsPlusSealed(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	runA := newRun(t, pg, "three-a")
	runB := newRun(t, pg, "three-b")
	runC := newRun(t, pg, "three-c")
	sealed := newRun(t, pg, "three-sealed")

	seq, err := pg.AppendLog(ctx, sealed, 0, "stdout", time.Now(), "before seal")
	require.NoError(t, err)
	require.NoError(t, pg.CreateLogArchive(ctx, sealed, "runs/"+sealed+"/logs.ndjson", 1, 1, seq))

	now := time.Now().UTC()
	// Interleave all four runs round-robin so a bug that mixes up which
	// input positions belong to which run would show up as a misalignment
	// rather than being masked by contiguous grouping in the input.
	lines := []LogAppend{
		{RunID: runA, StepIndex: 0, Stream: "stdout", Timestamp: now, Line: "a-1"},
		{RunID: runB, StepIndex: 0, Stream: "stdout", Timestamp: now, Line: "b-1"},
		{RunID: runC, StepIndex: 0, Stream: "stdout", Timestamp: now, Line: "c-1"},
		{RunID: sealed, StepIndex: 0, Stream: "stdout", Timestamp: now, Line: "dropped-1"},
		{RunID: runA, StepIndex: 0, Stream: "stdout", Timestamp: now, Line: "a-2"},
		{RunID: runB, StepIndex: 0, Stream: "stdout", Timestamp: now, Line: "b-2"},
		{RunID: sealed, StepIndex: 0, Stream: "stdout", Timestamp: now, Line: "dropped-2"},
		{RunID: runC, StepIndex: 0, Stream: "stdout", Timestamp: now, Line: "c-2"},
		{RunID: runA, StepIndex: 0, Stream: "stdout", Timestamp: now, Line: "a-3"},
	}
	// Indexes, by run, in input order -- used below to check each run's
	// seqs ascend in that run's own input order.
	idxA := []int{0, 4, 8}
	idxB := []int{1, 5}
	idxC := []int{2, 7}
	idxSealed := []int{3, 6}

	seqs, err := pg.AppendLogs(ctx, lines)
	require.NoError(t, err)
	require.Len(t, seqs, len(lines))

	for _, i := range idxSealed {
		assert.Zero(t, seqs[i], "sealed run's line at index %d must be dropped", i)
	}
	for _, group := range [][]int{idxA, idxB, idxC} {
		for j, i := range group {
			assert.Positive(t, seqs[i], "index %d must get a real seq", i)
			if j > 0 {
				assert.Less(t, seqs[group[j-1]], seqs[i],
					"seqs must ascend in this run's own input order (indexes %d then %d)", group[j-1], i)
			}
		}
	}

	// Each live run stored exactly its own lines, in its own input order --
	// confirming no cross-run bleed, positionally or in content.
	gotA, err := pg.TailLogs(ctx, runA, 0, 100)
	require.NoError(t, err)
	require.Len(t, gotA, 3)
	assert.Equal(t, []string{"a-1", "a-2", "a-3"}, []string{gotA[0].Line, gotA[1].Line, gotA[2].Line})

	gotB, err := pg.TailLogs(ctx, runB, 0, 100)
	require.NoError(t, err)
	require.Len(t, gotB, 2)
	assert.Equal(t, []string{"b-1", "b-2"}, []string{gotB[0].Line, gotB[1].Line})

	gotC, err := pg.TailLogs(ctx, runC, 0, 100)
	require.NoError(t, err)
	require.Len(t, gotC, 2)
	assert.Equal(t, []string{"c-1", "c-2"}, []string{gotC[0].Line, gotC[1].Line})

	count, _, _, err := pg.CountLogs(ctx, sealed, nil)
	require.NoError(t, err)
	assert.EqualValues(t, 1, count, "only the pre-seal line; both dropped-N lines were rejected")
}
