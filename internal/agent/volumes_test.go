package agent

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/config"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

func newTestVolumes(t *testing.T, cfgs ...config.VolumeConfig) (*VolumeManager, string) {
	t.Helper()
	base := t.TempDir()
	return mustVolumeManager(t, base, cfgs...), base
}

// mustVolumeManager builds a manager on base and applies cfgs as the desired
// set, requiring every volume to come up ready. A config Path is ignored: a
// volume always lives at <base>/volumes/<name>.
func mustVolumeManager(t *testing.T, base string, cfgs ...config.VolumeConfig) *VolumeManager {
	t.Helper()
	m, err := NewVolumeManager(base)
	require.NoError(t, err)
	for _, st := range m.Apply(desiredFromConfigs(cfgs)) {
		require.True(t, st.Ready, "volume %q: %s", st.Name, st.Reason)
	}
	return m
}

func desiredFromConfigs(cfgs []config.VolumeConfig) []api.DesiredVolume {
	out := make([]api.DesiredVolume, 0, len(cfgs))
	for _, c := range cfgs {
		out = append(out, api.DesiredVolume{Name: c.Name, Mode: c.Mode, Size: c.Size, ReclaimPolicy: dsl.ReclaimRetain})
	}
	return out
}

func exclusive(name string) config.VolumeConfig {
	return config.VolumeConfig{Name: name, Mode: "exclusive"}
}

func shared(name string) config.VolumeConfig {
	return config.VolumeConfig{Name: name, Mode: "shared"}
}

// reserveResult is the outcome of a Reserve started by reserveAsync, so a test
// can assert that a call is (still) blocked.
type reserveResult struct {
	name    string
	release func()
	err     error
}

func reserveAsync(m *VolumeManager, runID string, names ...string) <-chan reserveResult {
	ch := make(chan reserveResult, 1)
	go func() {
		n, rel, err := m.Reserve(runID, names)
		ch <- reserveResult{n, rel, err}
	}()
	return ch
}

func requireBlocked(t *testing.T, ch <-chan reserveResult) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("expected the call to block, but it returned %q (err %v)", r.name, r.err)
	case <-time.After(150 * time.Millisecond):
	}
}

func requireReturns(t *testing.T, ch <-chan reserveResult) reserveResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("the call stayed blocked after the holder released")
		return reserveResult{}
	}
}

func TestVolumeManager_CreatesDirectories(t *testing.T) {
	m, base := newTestVolumes(t, exclusive("a"), exclusive("unity"))
	require.DirExists(t, filepath.Join(base, "volumes", "a"))
	require.DirExists(t, filepath.Join(base, "volumes", "unity"))
	require.Equal(t, []string{"a", "unity"}, m.Names())
}

func TestVolumeManager_DefaultPath(t *testing.T) {
	m, base := newTestVolumes(t, shared("go-cache"))
	require.Equal(t, filepath.Join(base, "volumes", "go-cache"), m.PathOf("go-cache"))
	require.Equal(t, "shared", m.ModeOf("go-cache"))
	require.Equal(t, "", m.PathOf("nope"))
	require.Equal(t, "", m.ModeOf("nope"))
}

func applyOneVolume(t *testing.T, d api.DesiredVolume) api.AgentVolumeStatus {
	t.Helper()
	m, err := NewVolumeManager(t.TempDir())
	require.NoError(t, err)
	sts := m.Apply([]api.DesiredVolume{d})
	require.Len(t, sts, 1)
	require.Empty(t, m.Names(), "an invalid volume must not be offered")
	return sts[0]
}

func TestVolumeManager_RejectsUnknownMode(t *testing.T) {
	for _, mode := range []string{"", "readonly", "Exclusive"} {
		st := applyOneVolume(t, api.DesiredVolume{Name: "a", Mode: mode})
		require.False(t, st.Ready, "mode %q", mode)
		require.Contains(t, st.Reason, "invalid:")
		require.Contains(t, st.Reason, "mode")
	}
}

func TestVolumeManager_RejectsDuplicateName(t *testing.T) {
	m, err := NewVolumeManager(t.TempDir())
	require.NoError(t, err)
	sts := m.Apply([]api.DesiredVolume{
		{Name: "a", Mode: "exclusive", ReclaimPolicy: "Retain"},
		{Name: "a", Mode: "shared", ReclaimPolicy: "Retain"},
	})
	require.Len(t, sts, 1)
	require.False(t, sts[0].Ready)
	require.Contains(t, sts[0].Reason, "more than once")
}

func TestVolumeManager_RejectsBadNames(t *testing.T) {
	for _, name := range []string{"", ".", "..", "a/b", `a\b`} {
		st := applyOneVolume(t, api.DesiredVolume{Name: name, Mode: "exclusive"})
		require.False(t, st.Ready, "name %q", name)
		require.Contains(t, st.Reason, "invalid:")
	}
}

func TestVolumeManager_RejectsBadSizeAndReclaimPolicy(t *testing.T) {
	for _, size := range []string{"lots", "0", "-1Gi"} {
		st := applyOneVolume(t, api.DesiredVolume{Name: "a", Mode: "exclusive", Size: size})
		require.False(t, st.Ready, "size %q", size)
		require.Contains(t, st.Reason, "size")
	}
	st := applyOneVolume(t, api.DesiredVolume{Name: "a", Mode: "exclusive", ReclaimPolicy: "Recycle"})
	require.False(t, st.Ready)
	require.Contains(t, st.Reason, "reclaimPolicy")
}

// A <base>/volumes that is a link into a working* directory would put every
// volume where the workspace GC sweeps; the lexical check cannot see it, the
// resolved-path check must.
func TestVolumeManager_RejectsVolumesRootResolvingIntoAWorkingDir(t *testing.T) {
	base := t.TempDir()
	working := filepath.Join(base, "working0")
	require.NoError(t, os.MkdirAll(working, 0o755))
	if err := os.Symlink(working, filepath.Join(base, "volumes")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	m, err := NewVolumeManager(base)
	require.NoError(t, err)
	sts := m.Apply([]api.DesiredVolume{{Name: "a", Mode: "exclusive", ReclaimPolicy: "Retain"}})
	require.Len(t, sts, 1)
	require.False(t, sts[0].Ready)
	require.Contains(t, sts[0].Reason, "workspace GC")
	require.Empty(t, m.Names())
}

func TestVolumeManager_SharedAllowsConcurrentReserve(t *testing.T) {
	m, _ := newTestVolumes(t, shared("s"))
	n1, rel1, err := m.Reserve("r1", []string{"s"})
	require.NoError(t, err)
	n2, rel2, err := m.Reserve("r2", []string{"s"})
	require.NoError(t, err)
	require.Equal(t, "s", n1)
	require.Equal(t, "s", n2)
	require.Empty(t, m.Busy(), "a shared volume is never reported busy")
	rel1()
	rel2()
}

func TestVolumeManager_ExclusiveBlocksSecondReserve(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("a"))
	_, rel, err := m.Reserve("r1", []string{"a"})
	require.NoError(t, err)

	ch := reserveAsync(m, "r2", "a")
	requireBlocked(t, ch)
	rel()
	r := requireReturns(t, ch)
	require.NoError(t, r.err)
	require.Equal(t, "a", r.name)
	r.release()
}

func TestVolumeManager_ReservePicksFreeCandidate(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("a"), exclusive("b"))
	_, relA, err := m.Reserve("r1", []string{"a"})
	require.NoError(t, err)
	name, relB, err := m.Reserve("r2", []string{"a", "b"})
	require.NoError(t, err)
	require.Equal(t, "b", name)
	relA()
	relB()
}

func TestVolumeManager_ReserveSkipsUnknownCandidates(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("b"))
	name, rel, err := m.Reserve("r", []string{"ghost", "b"})
	require.NoError(t, err)
	require.Equal(t, "b", name)
	rel()

	_, _, err = m.Reserve("r", []string{"ghost"})
	require.Error(t, err, "no known candidate must fail, not block forever")
	require.Contains(t, err.Error(), "ghost")
	_, _, err = m.Reserve("r", nil)
	require.Error(t, err)
	_, _, err = m.Reserve("", []string{"b"})
	require.Error(t, err)
}

func TestVolumeManager_BusyListsExclusiveNamesAndAbsolutePaths(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("a"), exclusive("b"), shared("s"))
	require.Empty(t, m.Busy())
	require.NotNil(t, m.Busy(), "an empty report marshals as [], not null")

	_, relB, err := m.Reserve("r1", []string{"b"})
	require.NoError(t, err)
	_, relS, err := m.Reserve("r1", []string{"s"})
	require.NoError(t, err)
	abs := filepath.Join(t.TempDir(), "cache")
	relP, err := m.ReservePath(abs)
	require.NoError(t, err)

	require.ElementsMatch(t, []string{"b", posixKey(abs)}, m.Busy())
	relB()
	relS()
	relP()
	require.Empty(t, m.Busy())
}

func TestVolumeManager_ReservePathBlocksSecondHolder(t *testing.T) {
	m, _ := newTestVolumes(t)
	abs := filepath.Join(t.TempDir(), "gocache")
	rel, err := m.ReservePath(abs)
	require.NoError(t, err)

	done := make(chan func(), 1)
	go func() {
		r2, err := m.ReservePath(abs + string(filepath.Separator)) // same path, different spelling
		if err != nil {
			t.Error(err)
		}
		done <- r2
	}()
	select {
	case <-done:
		t.Fatal("second holder was not blocked")
	case <-time.After(150 * time.Millisecond):
	}
	rel()
	select {
	case r2 := <-done:
		r2()
	case <-time.After(5 * time.Second):
		t.Fatal("second holder stayed blocked after release")
	}
	_, err = m.ReservePath("relative/path")
	require.Error(t, err)
}

func TestVolumeManager_ReserveIsReentrantForTheSameRun(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("a"), exclusive("b"))
	name1, rel1, err := m.Reserve("r", []string{"a"})
	require.NoError(t, err)
	require.Equal(t, "a", name1)

	// The same volume named in a second entry, with a free alternative listed
	// first: the run's own hold wins and nothing blocks.
	name2, rel2, err := m.Reserve("r", []string{"b", "a"})
	require.NoError(t, err)
	require.Equal(t, "a", name2, "the run's own volume comes back, not the free candidate")
	require.Equal(t, []string{"a"}, m.Busy())

	// The no-op release must not free the volume while the real one is held.
	rel2()
	require.Equal(t, []string{"a"}, m.Busy())
	rel1()
	require.Empty(t, m.Busy(), "releasing both leaves the volume free")

	// A release is idempotent: a stale second call must not free a later holder.
	_, relOther, err := m.Reserve("s", []string{"a"})
	require.NoError(t, err)
	rel1()
	require.Equal(t, []string{"a"}, m.Busy())
	relOther()
}

func TestVolumeManager_ReserveStillBlocksADifferentRun(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("a"))
	_, rel, err := m.Reserve("r", []string{"a"})
	require.NoError(t, err)
	ch := reserveAsync(m, "s", "a")
	requireBlocked(t, ch)
	rel()
	r := requireReturns(t, ch)
	require.NoError(t, r.err)
	r.release()
}

// Many runs contend for fewer exclusive volumes than there are runs; at no
// instant may two runs hold the same volume. Run under -race.
func TestVolumeManager_ExclusionHoldsUnderContention(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("a"), exclusive("b"))
	var inUse [2]atomic.Int32
	idx := map[string]int{"a": 0, "b": 1}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			runID := string(rune('A' + i))
			for j := 0; j < 20; j++ {
				name, rel, err := m.Reserve(runID, []string{"a", "b"})
				if err != nil {
					t.Error(err)
					return
				}
				if n := inUse[idx[name]].Add(1); n != 1 {
					t.Errorf("volume %s held by %d runs at once", name, n)
				}
				time.Sleep(50 * time.Microsecond)
				inUse[idx[name]].Add(-1)
				rel()
			}
		}(i)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("contending runs deadlocked")
	}
	require.Empty(t, m.Busy())
}

// posixKey is the spelling Busy reports for a reserved path: slash-separated on
// every OS, so it can match the DSL's POSIX mountPath on a Windows agent too.
func posixKey(p string) string { return path.Clean(filepath.ToSlash(p)) }

func TestVolumeManager_NilManagerIsSafe(t *testing.T) {
	var m *VolumeManager
	require.Empty(t, m.Names())
	require.NotNil(t, m.Busy(), "Busy must serialise as [] not null")
	require.Empty(t, m.Busy())
	require.Equal(t, "", m.ModeOf("a"))
	require.Equal(t, "", m.PathOf("a"))

	_, _, err := m.Reserve("r", []string{"a"})
	require.Error(t, err)
	_, _, err = m.ReserveContext(context.Background(), "r", []string{"a"})
	require.Error(t, err)
	_, err = m.ReservePath("/go/cache")
	require.Error(t, err)
}

func TestVolumeManager_ReservePathKeyIsPosix(t *testing.T) {
	m, _ := newTestVolumes(t)
	rel, err := m.ReservePath("/go//cache/")
	require.NoError(t, err)
	require.Equal(t, []string{"/go/cache"}, m.Busy(), "a POSIX mountPath must be reported as written, on every OS")
	rel()
	require.Empty(t, m.Busy())
}

func TestVolumeManager_ReserveContext_CancelledWaitReturns(t *testing.T) {
	m, _ := newTestVolumes(t, exclusive("a"))
	_, rel, err := m.Reserve("holder", []string{"a"})
	require.NoError(t, err)
	defer rel()

	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() {
		_, _, err := m.ReserveContext(ctx, "waiter", []string{"a"})
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("expected the wait to block, got %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-got:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the context did not end the wait")
	}
	// The abandoned wait must not hold anything.
	require.Equal(t, []string{"a"}, m.Busy())
	rel()
	require.Empty(t, m.Busy())
}

func TestVolumeManager_ReservePathContext_CancelledWaitReturns(t *testing.T) {
	m, _ := newTestVolumes(t)
	rel, err := m.ReservePath("/go/cache")
	require.NoError(t, err)
	defer rel()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = m.ReservePathContext(ctx, "/go/cache")
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

// --- helpers ---

// captureSlog routes the default logger into buf and returns the restore func.
func captureSlog(buf *bytes.Buffer) func() {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return func() { slog.SetDefault(prev) }
}
