package agent

import (
	"bytes"
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/config"
)

func newTestVolumes(t *testing.T, cfgs ...config.VolumeConfig) (*VolumeManager, string) {
	t.Helper()
	base := t.TempDir()
	m, err := NewVolumeManager(base, cfgs)
	require.NoError(t, err)
	return m, base
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
	custom := filepath.Join(t.TempDir(), "elsewhere", "unity")
	m, base := newTestVolumes(t,
		exclusive("a"),
		config.VolumeConfig{Name: "unity", Mode: "exclusive", Path: custom},
	)
	require.DirExists(t, filepath.Join(base, "volumes", "a"))
	require.DirExists(t, custom)
	require.Equal(t, []string{"a", "unity"}, m.Names())
}

func TestVolumeManager_DefaultPath(t *testing.T) {
	m, base := newTestVolumes(t, shared("go-cache"))
	require.Equal(t, filepath.Join(base, "volumes", "go-cache"), m.PathOf("go-cache"))
	require.Equal(t, "shared", m.ModeOf("go-cache"))
	require.Equal(t, "", m.PathOf("nope"))
	require.Equal(t, "", m.ModeOf("nope"))
}

func TestVolumeManager_RejectsUnknownMode(t *testing.T) {
	for _, mode := range []string{"", "readonly", "Exclusive"} {
		_, err := NewVolumeManager(t.TempDir(), []config.VolumeConfig{{Name: "a", Mode: mode}})
		require.Error(t, err, "mode %q", mode)
		require.Contains(t, err.Error(), `volume "a"`)
		require.Contains(t, err.Error(), "mode")
	}
}

func TestVolumeManager_RejectsDuplicateName(t *testing.T) {
	_, err := NewVolumeManager(t.TempDir(), []config.VolumeConfig{exclusive("a"), shared("a")})
	require.Error(t, err)
	require.Contains(t, err.Error(), `duplicate volume name "a"`)
}

func TestVolumeManager_RejectsBadNames(t *testing.T) {
	for _, name := range []string{"", ".", "..", "a/b", `a\b`} {
		_, err := NewVolumeManager(t.TempDir(), []config.VolumeConfig{exclusive(name)})
		require.Error(t, err, "name %q", name)
	}
}

func TestVolumeManager_RejectsPathUnderAWorkingDir(t *testing.T) {
	base := t.TempDir()
	for _, p := range []string{
		filepath.Join(base, "working0"),
		filepath.Join(base, "working3", "job", "cache"),
		filepath.Join(base, "working"), // matches the gc glob "working*"
		filepath.Join(base, "workingfoo"),
	} {
		_, err := NewVolumeManager(base, []config.VolumeConfig{{Name: "a", Mode: "exclusive", Path: p}})
		require.Error(t, err, "path %q", p)
		require.Contains(t, err.Error(), "workspace GC", "path %q", p)
		require.NoDirExists(t, p, "a refused path must not be created")
	}
	// A directory that merely resembles the glob without matching it is fine.
	_, err := NewVolumeManager(base, []config.VolumeConfig{{Name: "a", Mode: "exclusive", Path: filepath.Join(base, "my-working0")}})
	require.NoError(t, err)
}

func TestVolumeManager_RejectsPathContainingTheWorkspace(t *testing.T) {
	base := t.TempDir()
	_, err := NewVolumeManager(base, []config.VolumeConfig{{Name: "a", Mode: "exclusive", Path: base}})
	require.Error(t, err)
	_, err = NewVolumeManager(base, []config.VolumeConfig{{Name: "a", Mode: "exclusive", Path: filepath.Dir(base)}})
	require.Error(t, err)
}

func TestVolumeManager_RejectsOverlappingPaths(t *testing.T) {
	base := t.TempDir()
	p := filepath.Join(base, "volumes", "x")
	_, err := NewVolumeManager(base, []config.VolumeConfig{
		{Name: "a", Mode: "exclusive", Path: p},
		{Name: "b", Mode: "exclusive", Path: p},
	})
	require.Error(t, err)
	_, err = NewVolumeManager(base, []config.VolumeConfig{
		{Name: "a", Mode: "exclusive", Path: p},
		{Name: "b", Mode: "exclusive", Path: filepath.Join(p, "inner")},
	})
	require.Error(t, err)
}

func TestVolumeManager_RejectsRelativePathAndBadSize(t *testing.T) {
	_, err := NewVolumeManager(t.TempDir(), []config.VolumeConfig{{Name: "a", Mode: "exclusive", Path: "rel/dir"}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "absolute")
	_, err = NewVolumeManager(t.TempDir(), []config.VolumeConfig{{Name: "a", Mode: "exclusive", Size: "lots"}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "size")
}

func TestVolumeManager_UsageMeasurementToleratesPermissionErrors(t *testing.T) {
	m, _ := newTestVolumes(t, config.VolumeConfig{Name: "a", Mode: "exclusive", Size: "5"})
	root := m.PathOf("a")

	// The failure is injected through the walk function: a root-owned subtree
	// left by an isolated run on a rootful runtime cannot be created portably
	// (producing one needs root, and chmod 000 means nothing on Windows). The
	// injected walk reports the same fs.ErrPermission a real unreadable
	// directory yields. A real walk aborts when the callback returns an error,
	// so the callback must return nil here.
	m.walk = func(r string, fn fs.WalkDirFunc) error {
		require.Equal(t, root, r)
		require.NoError(t, fn(r, fakeDir{name: "a"}, nil))
		require.NoError(t, fn(filepath.Join(r, "readable"), fakeFile{name: "readable", size: 4}, nil))
		require.NoError(t, fn(filepath.Join(r, "rootowned"), fakeDir{name: "rootowned"},
			&fs.PathError{Op: "open", Path: filepath.Join(r, "rootowned"), Err: fs.ErrPermission}))
		require.NoError(t, fn(filepath.Join(r, "after"), fakeFile{name: "after", size: 3}, nil))
		return nil
	}

	used, skipped, firstErr := m.measureUsage("a")
	require.EqualValues(t, 7, used, "readable files on both sides of the failure are counted")
	require.Equal(t, 1, skipped)
	require.ErrorIs(t, firstErr, fs.ErrPermission)

	// CheckUsage never fails the run, and says what it could not read.
	var logs bytes.Buffer
	defer captureSlog(&logs)()
	m.CheckUsage("a")
	require.Contains(t, logs.String(), "could not be read")
	require.Contains(t, logs.String(), "exceeds")
}

func TestVolumeManager_UsageMeasurementOnARealUnreadableDir(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a non-root POSIX user for chmod to deny reads")
	}
	m, _ := newTestVolumes(t, config.VolumeConfig{Name: "a", Mode: "exclusive", Size: "1Gi"})
	root := m.PathOf("a")
	require.NoError(t, os.WriteFile(filepath.Join(root, "ok"), []byte("12345"), 0o644))
	locked := filepath.Join(root, "locked")
	require.NoError(t, os.Mkdir(locked, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(locked, "hidden"), []byte("x"), 0o644))
	require.NoError(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	used, skipped, firstErr := m.measureUsage("a")
	require.EqualValues(t, 5, used)
	require.Equal(t, 1, skipped)
	require.ErrorIs(t, firstErr, fs.ErrPermission)
}

func TestVolumeManager_UsageWithinSizeIsQuiet(t *testing.T) {
	m, _ := newTestVolumes(t, config.VolumeConfig{Name: "a", Mode: "exclusive", Size: "1Mi"})
	require.NoError(t, os.WriteFile(filepath.Join(m.PathOf("a"), "f"), []byte("abc"), 0o644))
	var logs bytes.Buffer
	defer captureSlog(&logs)()
	m.CheckUsage("a")
	require.Empty(t, logs.String())

	// No declared size: nothing to compare against, nothing measured.
	m2, _ := newTestVolumes(t, exclusive("b"))
	m2.walk = func(string, fs.WalkDirFunc) error { t.Fatal("walked a volume with no size"); return nil }
	m2.CheckUsage("b")
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
	require.NotPanics(t, func() { m.CheckUsage("a") })

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

func TestVolumeManager_RejectsPathsTheAgentItselfManages(t *testing.T) {
	base := t.TempDir()
	for _, p := range []string{
		filepath.Join(base, "detached"),
		filepath.Join(base, "detached", "run-1"),
		filepath.Join(base, ".ucd-tools"),
		filepath.Join(base, ".ucd-tools", "inner"),
		filepath.Join(base, ".hidden"),
	} {
		_, err := NewVolumeManager(base, []config.VolumeConfig{{Name: "a", Mode: "exclusive", Path: p}})
		require.Error(t, err, "path %q", p)
		require.NoDirExists(t, p, "a refused path must not be created")
	}
	// A near miss is fine.
	_, err := NewVolumeManager(base, []config.VolumeConfig{{Name: "a", Mode: "exclusive", Path: filepath.Join(base, "detached-cache")}})
	require.NoError(t, err)
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

type fakeFile struct {
	name string
	size int64
}

func (f fakeFile) Name() string               { return f.name }
func (f fakeFile) IsDir() bool                { return false }
func (f fakeFile) Type() fs.FileMode          { return 0 }
func (f fakeFile) Info() (fs.FileInfo, error) { return fakeInfo{f}, nil }

type fakeInfo struct{ f fakeFile }

func (i fakeInfo) Name() string       { return i.f.name }
func (i fakeInfo) Size() int64        { return i.f.size }
func (i fakeInfo) Mode() fs.FileMode  { return 0o644 }
func (i fakeInfo) ModTime() time.Time { return time.Time{} }
func (i fakeInfo) IsDir() bool        { return false }
func (i fakeInfo) Sys() any           { return nil }

type fakeDir struct{ name string }

func (d fakeDir) Name() string               { return d.name }
func (d fakeDir) IsDir() bool                { return true }
func (d fakeDir) Type() fs.FileMode          { return fs.ModeDir }
func (d fakeDir) Info() (fs.FileInfo, error) { return nil, fs.ErrPermission }

// captureSlog routes the default logger into buf and returns the restore func.
func captureSlog(buf *bytes.Buffer) func() {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return func() { slog.SetDefault(prev) }
}
