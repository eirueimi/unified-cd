package agent

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/config"
)

// explodingEntries returns 12 entries that pass the "more entries than distinct
// names" shortcut (12 names for 12 entries) and still have no distinct
// assignment: eleven entries share the ten names v0..v9, so by Hall's theorem
// they cannot all get one, and only an exhaustive search (about 10! orderings
// of the first ten entries before the eleventh fails, each time) finds that
// out. The twelfth entry, listed last, is what lifts the name count to 12.
func explodingEntries() [][]string {
	shared := namesN("v", 10)
	declared := make([][]string, 0, 12)
	for i := 0; i < 11; i++ {
		declared = append(declared, shared)
	}
	return append(declared, []string{"w0", "w1"})
}

// The search is bounded: an input whose distinct search is exponential returns
// within the node budget, as "wait", never as an aliased assignment and never
// after the full search. Work is counted (calls of the free predicate, at most
// one per candidate per node), not timed.
func TestChooseVolumes_ASearchThatExplodesIsAbandonedAndTheRunWaits(t *testing.T) {
	declared := explodingEntries()
	calls := 0
	free := func(string) bool { calls++; return true }

	got, ok, abandoned := ChooseVolumes(declared, free)

	assert.False(t, ok, "an input whose search was cut short must wait, not alias")
	assert.Nil(t, got)
	assert.True(t, abandoned, "the caller must be told, so that it can log")
	assert.LessOrEqual(t, calls, maxVolumeSearchNodes*10,
		"the first pass visited more nodes than the budget allows (10 candidates per node at most)")
}

// The search for a free assignment can be the one that blows up while distinct
// volumes plainly exist: w0 is busy, so the first ten entries are permuted over
// v0..v9 in vain before the eleventh, which only has w0 left, fails; enough
// volumes are free (12 for 12 entries) that the counting shortcut cannot see
// it. The run waits, and is told the search was cut short, since the wait may
// be needless.
func TestChooseVolumes_AFreeSearchThatExplodesWhileDistinctVolumesExistWaits(t *testing.T) {
	shared := namesN("v", 10)
	declared := make([][]string, 0, 12)
	for i := 0; i < 10; i++ {
		declared = append(declared, shared)
	}
	declared = append(declared, append(append([]string{}, shared...), "w0"), []string{"u0", "u1"})
	free := func(n string) bool { return n != "w0" }

	got, ok, abandoned := ChooseVolumes(declared, free)

	assert.False(t, ok)
	assert.Nil(t, got)
	assert.True(t, abandoned)
}

// The budget must not touch ordinary jobs: a dozen entries over a dozen
// volumes, free or partly busy, resolve exactly as an unbounded search would.
func TestChooseVolumes_OrdinaryJobsAreNotAbandoned(t *testing.T) {
	names := namesN("v", 12)
	declared := make([][]string, 12)
	for i := range declared {
		declared[i] = names
	}

	got, ok, abandoned := ChooseVolumes(declared, func(string) bool { return true })
	require.True(t, ok)
	assert.False(t, abandoned)
	assert.Equal(t, names, got)

	// Two volumes busy: the other ten cannot serve twelve entries, so the run
	// waits for them (distinct volumes exist). That is an ordinary wait, decided
	// by counting, not a search that ran out of budget.
	got, ok, abandoned = ChooseVolumes(declared, func(n string) bool { return n != "v0" && n != "v1" })
	assert.False(t, ok)
	assert.Nil(t, got)
	assert.False(t, abandoned)
}

// recordingHandler is a slog.Handler that keeps the messages it is given and
// notes whether m.mu was held at the moment each was written.
type recordingHandler struct {
	m *VolumeManager

	mu       sync.Mutex
	messages []string
	underMu  int
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	held := !h.m.mu.TryLock()
	if !held {
		h.m.mu.Unlock()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = append(h.messages, r.Message)
	if held {
		h.underMu++
	}
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

// The host agent logs the abandoned search once per call, naming the entry
// count, and logs it after m.mu is released: a log write that blocks must not
// stop every other claim on the agent. This test does not use t.Parallel: it
// replaces the global slog default.
func TestReserveSet_AnAbandonedSearchIsLoggedOnceOutsideTheLock(t *testing.T) {
	var cfgs []config.VolumeConfig
	for _, n := range append(namesN("v", 10), "w0", "w1") {
		cfgs = append(cfgs, exclusive(n))
	}
	m, err := NewVolumeManager(t.TempDir(), cfgs)
	require.NoError(t, err)

	h := &recordingHandler{m: m}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	waited := 0
	_, _, err = m.ReserveSet(ctx, "run-x", explodingEntries(), nil, func() { waited++ })

	require.ErrorIs(t, err, context.DeadlineExceeded, "the run waits; it is neither failed nor given a volume")
	assert.Equal(t, 1, waited)
	h.mu.Lock()
	defer h.mu.Unlock()
	var abandoned []string
	for _, msg := range h.messages {
		if strings.Contains(msg, "search abandoned") {
			abandoned = append(abandoned, msg)
		}
	}
	require.Len(t, abandoned, 1)
	assert.Contains(t, abandoned[0], "12 volume entries")
	assert.Zero(t, h.underMu, "the warning was written while m.mu was held")
}
