package controller

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/store"
)

// archivedLogsCacheBytes bounds the total raw-ndjson bytes of parsed archives
// kept in memory. Trimmed runs are terminal and their archives immutable, so
// caching is safe; an archive larger than the whole cap is decoded per
// request and never cached. A var (not const) so tests can shrink it.
var archivedLogsCacheBytes = int64(128 << 20) // 128 MiB

type archivedLogEntry struct {
	runID string
	lines []api.LogLine
	bytes int64
}

// archiveFetch is an in-flight fetch+decode of one run's archive, shared by
// every concurrent caller asking for the same runID (see the singleflight
// join in (*archivedLogs).lines). done is closed once lines/err are set.
type archiveFetch struct {
	done  chan struct{}
	lines []api.LogLine
	err   error
}

// archivedLogs serves the log read contracts for runs whose logs rows were
// trimmed from the DB, by fetching and decoding runs/<runID>/logs.ndjson.
type archivedLogs struct {
	obj objectstore.ObjectStore

	mu       sync.Mutex
	cache    map[string]*list.Element // runID -> element holding *archivedLogEntry
	order    *list.List               // front = most recently used
	total    int64
	inflight map[string]*archiveFetch // runID -> shared fetch+decode in progress
}

func newArchivedLogs(obj objectstore.ObjectStore) *archivedLogs {
	return &archivedLogs{
		obj:      obj,
		cache:    map[string]*list.Element{},
		order:    list.New(),
		inflight: map[string]*archiveFetch{},
	}
}

func (a *archivedLogs) cacheLen() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.cache)
}

// lines returns the run's full archived log, seq-ascending (the archiver
// wrote it in TailLogs order). Callers must treat the slice as read-only —
// it may be shared via the cache.
//
// Concurrent cache misses for the same runID are coalesced (singleflight):
// the first caller becomes the fetcher and does the object-store Get plus
// ndjson decode; every other concurrent caller for the same runID just
// waits on that one fetch instead of redoing it. This matters most for an
// archive larger than the whole cache cap, which is never cached and would
// otherwise be re-fetched and re-decoded on every single request.
func (a *archivedLogs) lines(ctx context.Context, runID string) ([]api.LogLine, error) {
	a.mu.Lock()
	if el, ok := a.cache[runID]; ok {
		a.order.MoveToFront(el)
		lines := el.Value.(*archivedLogEntry).lines
		a.mu.Unlock()
		return lines, nil
	}
	if f, ok := a.inflight[runID]; ok {
		a.mu.Unlock()
		<-f.done
		return f.lines, f.err
	}
	f := &archiveFetch{done: make(chan struct{})}
	a.inflight[runID] = f
	a.mu.Unlock()

	// Cleanup is deferred (not inline after fetchAndDecode) for two reasons:
	//  - NEW-4: if fetchAndDecode or the caching logic below panics, the
	//    deferred close(f.done) still runs, so coalesced waiters observe the
	//    panic-time zero values instead of blocking on f.done forever, and
	//    the deferred delete(a.inflight, runID) still runs, so the runID
	//    isn't left permanently stuck routing every future caller to a dead
	//    fetch that will never complete.
	//  - the leader's fetch uses fetchCtx (see below), decoupled from the
	//    leader's own ctx, so waiters must not be released before fetchCtx's
	//    fetch actually finishes; deferring keeps that ordering regardless of
	//    how the function returns.
	var lines []api.LogLine
	var size int64
	var err error
	defer func() {
		f.lines, f.err = lines, err
		close(f.done)

		a.mu.Lock()
		delete(a.inflight, runID)
		if err == nil && size <= archivedLogsCacheBytes {
			if _, ok := a.cache[runID]; !ok {
				el := a.order.PushFront(&archivedLogEntry{runID: runID, lines: lines, bytes: size})
				a.cache[runID] = el
				a.total += size
				for a.total > archivedLogsCacheBytes {
					oldest := a.order.Back()
					e := oldest.Value.(*archivedLogEntry)
					a.order.Remove(oldest)
					delete(a.cache, e.runID)
					a.total -= e.bytes
				}
			}
		}
		a.mu.Unlock()
	}()

	// NEW-3: the leader fetches on a context decoupled from cancellation of
	// the leader's own request (context.WithoutCancel keeps ctx's values —
	// e.g. tracing — while dropping its Done channel and deadline). Without
	// this, every coalesced waiter would get "context canceled" whenever the
	// leader's client happened to disconnect first, even though the fetch
	// itself is still perfectly servable for the other waiters.
	fetchCtx := context.WithoutCancel(ctx)
	lines, size, err = a.fetchAndDecode(fetchCtx, runID)
	return lines, err
}

// fetchAndDecode fetches and ndjson-decodes one run's archive object. It
// holds no lock — callers coordinate via the inflight map in lines().
func (a *archivedLogs) fetchAndDecode(ctx context.Context, runID string) ([]api.LogLine, int64, error) {
	rc, err := a.obj.Get(ctx, runLogArchiveKey(runID))
	if err != nil {
		return nil, 0, fmt.Errorf("fetch log archive: %w", err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return nil, 0, fmt.Errorf("read log archive: %w", err)
	}
	var lines []api.LogLine
	dec := json.NewDecoder(bytes.NewReader(raw))
	for {
		var l api.LogLine
		if err := dec.Decode(&l); err == io.EOF {
			break
		} else if err != nil {
			return nil, 0, fmt.Errorf("decode log archive: %w", err)
		}
		lines = append(lines, l)
	}
	return lines, int64(len(raw)), nil
}

// logsTrimmed reports whether the run's logs rows were trimmed from the DB
// (archive record with trimmed_at set) and the archive reader is available,
// i.e. log reads must be served from the archive object.
func (s *Server) logsTrimmed(ctx context.Context, runID string) (bool, error) {
	if s.archLogs == nil {
		return false, nil
	}
	arch, err := s.store.GetLogArchive(ctx, runID)
	if err != nil {
		return false, err
	}
	return arch != nil && arch.TrimmedAt != nil, nil
}

// filterSteps returns the step-filtered view; an empty steps set means the
// whole log (same convention as logsStepFilter in the store).
func filterSteps(lines []api.LogLine, steps []int) []api.LogLine {
	if len(steps) == 0 {
		return lines
	}
	want := make(map[int]bool, len(steps))
	for _, s := range steps {
		want[s] = true
	}
	out := make([]api.LogLine, 0, len(lines))
	for _, l := range lines {
		if want[l.StepIndex] {
			out = append(out, l)
		}
	}
	return out
}

// tailAfter mirrors store TailLogs: lines with seq > afterSeq, ascending, LIMIT.
func tailAfter(lines []api.LogLine, afterSeq int64, limit int) []api.LogLine {
	i := sort.Search(len(lines), func(i int) bool { return lines[i].Seq > afterSeq })
	out := lines[i:]
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// tailRecent mirrors store TailLogsRecent: the last `limit` lines, ascending.
func tailRecent(lines []api.LogLine, limit int) []api.LogLine {
	if len(lines) > limit {
		return lines[len(lines)-limit:]
	}
	return lines
}

// countArchivedLogs mirrors store CountLogs over the step-filtered view.
func countArchivedLogs(lines []api.LogLine, steps []int) (count, minSeq, maxSeq int64) {
	v := filterSteps(lines, steps)
	if len(v) == 0 {
		return 0, 0, 0
	}
	return int64(len(v)), v[0].Seq, v[len(v)-1].Seq
}

// archivedLogRange mirrors store ListLogsRange (view order, OFFSET/LIMIT).
func archivedLogRange(lines []api.LogLine, steps []int, offset, limit int) []api.LogLine {
	v := filterSteps(lines, steps)
	if offset >= len(v) {
		return nil
	}
	v = v[offset:]
	if len(v) > limit {
		v = v[:limit]
	}
	return v
}

// searchArchivedLogs mirrors store SearchLogs: case-insensitive literal
// substring match (the escaped-ILIKE semantics), row numbers are 0-based
// positions in the step-filtered view BEFORE the match filter, results
// seq-ordered and capped at capN with the uncapped total returned.
func searchArchivedLogs(lines []api.LogLine, steps []int, q string, capN int) (int64, []store.LogSearchMatch) {
	v := filterSteps(lines, steps)
	needle := strings.ToLower(q)
	var total int64
	var out []store.LogSearchMatch
	for row, l := range v {
		if strings.Contains(strings.ToLower(l.Line), needle) {
			total++
			if len(out) < capN {
				out = append(out, store.LogSearchMatch{Row: int64(row), Seq: l.Seq, StepIndex: l.StepIndex})
			}
		}
	}
	return total, out
}
