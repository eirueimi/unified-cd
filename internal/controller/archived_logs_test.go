package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedParityRun inserts a deliberately tricky log set into Postgres and
// uploads the equivalent ndjson archive, returning the run ID and the object
// store. Lines cover: multiple steps, mixed streams, ILIKE metacharacters
// (%, _, \), and mixed case.
func seedParityRun(t *testing.T, pg store.Store, obj objectstore.ObjectStore) string {
	t.Helper()
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	seed := []struct {
		step   int
		stream string
		line   string
	}{
		{0, "stdout", "Building target ALPHA"},
		{0, "stderr", "warn: 100% done_ok"},
		{1, "stdout", `path C:\tmp\x`},
		{1, "stdout", "building target alpha"},
		{2, "stderr", "under_score and per%cent"},
		{2, "stdout", "plain line"},
	}
	for _, l := range seed {
		_, err := pg.AppendLog(ctx, run.ID, l.step, l.stream, time.Now(), l.line)
		require.NoError(t, err)
	}
	// Build the archive exactly like archiveRunLogs does.
	lines, err := pg.TailLogs(ctx, run.ID, 0, 1_000_000)
	require.NoError(t, err)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, l := range lines {
		require.NoError(t, enc.Encode(l))
	}
	require.NoError(t, obj.Put(ctx, runLogArchiveKey(run.ID), &buf, int64(buf.Len())))
	return run.ID
}

// TestArchivedLogs_ParityWithStore asserts every reader contract returns
// results identical to the store methods over the same data.
func TestArchivedLogs_ParityWithStore(t *testing.T) {
	_, pg := newTestServer(t)
	obj := objectstore.NewLocalObjectStore(t.TempDir())
	runID := seedParityRun(t, pg, obj)
	ctx := context.Background()

	a := newArchivedLogs(obj)
	lines, err := a.lines(ctx, runID)
	require.NoError(t, err)
	require.Len(t, lines, 6)

	stepSets := [][]int{nil, {0}, {1, 2}, {5}}

	for _, steps := range stepSets {
		label := fmt.Sprintf("steps=%v", steps)

		wantCount, wantMin, wantMax, err := pg.CountLogs(ctx, runID, steps)
		require.NoError(t, err, label)
		gotCount, gotMin, gotMax := countArchivedLogs(lines, steps)
		assert.Equal(t, wantCount, gotCount, label)
		assert.Equal(t, wantMin, gotMin, label)
		assert.Equal(t, wantMax, gotMax, label)

		for _, window := range []struct{ offset, limit int }{{0, 10}, {1, 2}, {4, 10}, {99, 5}} {
			want, err := pg.ListLogsRange(ctx, runID, steps, window.offset, window.limit)
			require.NoError(t, err, label)
			got := archivedLogRange(lines, steps, window.offset, window.limit)
			assert.Equal(t, normalize(want), normalize(got), "%s offset=%d limit=%d", label, window.offset, window.limit)
		}

		for _, q := range []string{"alpha", "ALPHA", "100%", "under_score", `C:\tmp`, "nomatch", "_"} {
			wantTotal, wantMatches, err := pg.SearchLogs(ctx, runID, steps, q, 3)
			require.NoError(t, err, label)
			gotTotal, gotMatches := searchArchivedLogs(lines, steps, q, 3)
			assert.Equal(t, wantTotal, gotTotal, "%s q=%q", label, q)
			assert.Equal(t, normalizeMatches(wantMatches), normalizeMatches(gotMatches), "%s q=%q", label, q)
		}
	}

	// TailLogs paging parity over the full view.
	all, err := pg.TailLogs(ctx, runID, 0, 1000)
	require.NoError(t, err)
	for _, after := range []int64{0, all[0].Seq, all[2].Seq, all[5].Seq} {
		for _, limit := range []int{1, 3, 100} {
			want, err := pg.TailLogs(ctx, runID, after, limit)
			require.NoError(t, err)
			got := tailAfter(lines, after, limit)
			assert.Equal(t, normalize(want), normalize(got), "after=%d limit=%d", after, limit)
		}
	}

	// TailLogsRecent parity.
	for _, limit := range []int{2, 6, 100} {
		want, err := pg.TailLogsRecent(ctx, runID, limit)
		require.NoError(t, err)
		got := tailRecent(lines, limit)
		assert.Equal(t, normalize(want), normalize(got), "recent limit=%d", limit)
	}
}

// normalize maps nil/empty to empty and truncates timestamps to microseconds:
// Postgres stores timestamptz at microsecond precision, while the ndjson
// round-trip keeps Go's nanoseconds, so exact time.Time equality would fail
// spuriously.
func normalize(in []api.LogLine) []api.LogLine {
	out := make([]api.LogLine, 0, len(in))
	for _, l := range in {
		l.Timestamp = l.Timestamp.Truncate(time.Microsecond)
		out = append(out, l)
	}
	return out
}

func normalizeMatches(in []store.LogSearchMatch) []store.LogSearchMatch {
	if len(in) == 0 {
		return []store.LogSearchMatch{}
	}
	return in
}

func TestArchivedLogs_CacheEvictsByBytes(t *testing.T) {
	ctx := context.Background()
	obj := objectstore.NewLocalObjectStore(t.TempDir())
	put := func(runID, line string) {
		var buf bytes.Buffer
		require.NoError(t, json.NewEncoder(&buf).Encode(api.LogLine{Seq: 1, Line: line}))
		require.NoError(t, obj.Put(ctx, runLogArchiveKey(runID), &buf, int64(buf.Len())))
	}
	put("r1", strings.Repeat("a", 100))
	put("r2", strings.Repeat("b", 100))

	old := archivedLogsCacheBytes
	archivedLogsCacheBytes = 200 // each entry is ~180 bytes: two never fit
	defer func() { archivedLogsCacheBytes = old }()

	a := newArchivedLogs(obj)
	_, err := a.lines(ctx, "r1")
	require.NoError(t, err)
	_, err = a.lines(ctx, "r2")
	require.NoError(t, err)
	assert.Equal(t, 1, a.cacheLen(), "r1 must have been evicted to fit r2")

	// Oversized archive: served but never cached.
	put("big", strings.Repeat("c", 500))
	_, err = a.lines(ctx, "big")
	require.NoError(t, err)
	assert.Equal(t, 1, a.cacheLen())
}
