package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

func dv(name, mode, reclaim string) api.DesiredVolume {
	return api.DesiredVolume{Name: name, Mode: mode, Size: "1Gi", ReclaimPolicy: reclaim}
}

func newEmptyVM(t *testing.T) (*VolumeManager, string) {
	t.Helper()
	base := t.TempDir()
	m, err := NewVolumeManager(base)
	require.NoError(t, err)
	return m, base
}

func readReclaimRecord(t *testing.T, base string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(base, volumeReclaimFile))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}
	}
	require.NoError(t, err)
	var rec reclaimRecord
	require.NoError(t, json.Unmarshal(b, &rec))
	return rec.Volumes
}

// withClock gives m a clock the test moves with the returned function.
func withClock(m *VolumeManager) func(time.Duration) {
	var mu sync.Mutex
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	return func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}
}

func readLeftSetAt(t *testing.T, base string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(base, volumeReclaimFile))
	require.NoError(t, err)
	var rec reclaimRecord
	require.NoError(t, json.Unmarshal(b, &rec))
	return rec.LeftSetAt
}

// trashEntries lists the moved-aside reclaimed directories under base.
func trashEntries(t *testing.T, base string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(base, "volumes"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), reclaimTrashPrefix) {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestApplyAddCreatesDirAndKeepsExisting(t *testing.T) {
	m, base := newEmptyVM(t)
	dir := filepath.Join(base, "volumes", "u")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("x"), 0o644))

	sts := m.Apply([]api.DesiredVolume{dv("u", "exclusive", dsl.ReclaimRetain)})
	require.Equal(t, []api.AgentVolumeStatus{{Name: "u", Ready: true}}, sts)
	require.FileExists(t, filepath.Join(dir, "keep.txt"))
	require.Equal(t, []string{"u"}, m.Names())
	require.Equal(t, []string{dir}, m.Paths())
}

func TestApplyRemoveWhileHeld(t *testing.T) {
	m, base := newEmptyVM(t)
	m.Apply([]api.DesiredVolume{dv("u", "exclusive", dsl.ReclaimRetain)})
	_, rel, err := m.Reserve("r1", []string{"u"})
	require.NoError(t, err)

	require.Empty(t, m.Apply(nil))
	require.Empty(t, m.Names())
	require.Equal(t, []string{"u"}, m.Busy(), "the holder is still counted")
	_, _, err = m.ReserveSet(context.Background(), "r2", [][]string{{"u"}}, nil, nil)
	require.ErrorContains(t, err, "declares none")
	require.Len(t, m.Paths(), 1, "the retiring volume is still sampled")

	rel()
	m.Apply(nil)
	require.Empty(t, m.Paths(), "forgotten once the last holder released")
	require.Equal(t, "", m.PathOf("u"))
	require.DirExists(t, filepath.Join(base, "volumes", "u"), "Retain keeps the data")
}

func TestApplyRetiringVolumeStillServesItsHolder(t *testing.T) {
	m, _ := newEmptyVM(t)
	m.Apply([]api.DesiredVolume{dv("u", "exclusive", dsl.ReclaimRetain)})
	_, rel, err := m.Reserve("r1", []string{"u"})
	require.NoError(t, err)
	defer rel()
	m.Apply(nil)
	require.NotEmpty(t, m.PathOf("u"))
	n, _, err := m.Reserve("r1", []string{"u"})
	require.NoError(t, err, "a holder may re-enter a retiring volume")
	require.Equal(t, "u", n)
}

func TestApplyModeChangeWaitsForHolders(t *testing.T) {
	m, _ := newEmptyVM(t)
	m.Apply([]api.DesiredVolume{dv("u", "exclusive", dsl.ReclaimRetain)})
	_, rel, err := m.Reserve("r1", []string{"u"})
	require.NoError(t, err)

	sts := m.Apply([]api.DesiredVolume{dv("u", "shared", dsl.ReclaimRetain)})
	require.Equal(t, []api.AgentVolumeStatus{{Name: "u", Reason: "waiting for 1 run(s) to release before switching to shared"}}, sts)
	require.Empty(t, m.Names())
	require.Equal(t, "exclusive", m.ModeOf("u"))

	rel()
	sts = m.Apply([]api.DesiredVolume{dv("u", "shared", dsl.ReclaimRetain)})
	require.Equal(t, []api.AgentVolumeStatus{{Name: "u", Ready: true}}, sts)
	require.Equal(t, []string{"u"}, m.Names())
	require.Equal(t, "shared", m.ModeOf("u"))
	_, r1, err := m.Reserve("a", []string{"u"})
	require.NoError(t, err)
	_, r2, err := m.Reserve("b", []string{"u"})
	require.NoError(t, err, "a shared volume admits two runs")
	r1()
	r2()
}

func TestApplyModeChangeBackWhileHeldCancelsTheSwitch(t *testing.T) {
	m, _ := newEmptyVM(t)
	m.Apply([]api.DesiredVolume{dv("u", "exclusive", dsl.ReclaimRetain)})
	_, rel, err := m.Reserve("r1", []string{"u"})
	require.NoError(t, err)
	defer rel()
	m.Apply([]api.DesiredVolume{dv("u", "shared", dsl.ReclaimRetain)})
	sts := m.Apply([]api.DesiredVolume{dv("u", "exclusive", dsl.ReclaimRetain)})
	require.True(t, sts[0].Ready)
	require.Equal(t, []string{"u"}, m.Names())
}

func TestApplyOneBadVolumeDoesNotBlockOthers(t *testing.T) {
	m, base := newEmptyVM(t)
	require.NoError(t, os.MkdirAll(filepath.Join(base, "volumes"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(base, "volumes", "bad"), []byte("file"), 0o644))

	sts := m.Apply([]api.DesiredVolume{dv("good", "exclusive", dsl.ReclaimRetain), dv("bad", "exclusive", dsl.ReclaimRetain)})
	require.Len(t, sts, 2)
	require.Equal(t, "bad", sts[0].Name)
	require.False(t, sts[0].Ready)
	require.Contains(t, sts[0].Reason, "create directory:")
	require.Equal(t, api.AgentVolumeStatus{Name: "good", Ready: true}, sts[1])
	require.Equal(t, []string{"good"}, m.Names())
}

func TestApplyIdempotent(t *testing.T) {
	m, base := newEmptyVM(t)
	want := []api.DesiredVolume{dv("a", "exclusive", dsl.ReclaimDelete), dv("b", "shared", dsl.ReclaimRetain)}
	first := m.Apply(want)
	before := readReclaimRecord(t, base)
	second := m.Apply(want)
	require.Equal(t, first, second)
	require.Equal(t, before, readReclaimRecord(t, base))
	require.Equal(t, []string{"a", "b"}, m.Names())
}

func TestReclaimDeleteAfterRelease(t *testing.T) {
	m, base := newEmptyVM(t)
	advance := withClock(m)
	dir := filepath.Join(base, "volumes", "u")
	m.Apply([]api.DesiredVolume{dv("u", "exclusive", dsl.ReclaimDelete)})
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644))
	_, rel, err := m.Reserve("r1", []string{"u"})
	require.NoError(t, err)

	require.Empty(t, m.Apply(nil))
	require.DirExists(t, dir, "a held volume is not reclaimed")
	require.Equal(t, map[string]string{"u": "Delete"}, readReclaimRecord(t, base))

	advance(reclaimGrace)
	require.Empty(t, m.Apply(nil))
	require.DirExists(t, dir, "past the grace, but still held")

	rel()
	require.Empty(t, m.Apply(nil))
	require.NoDirExists(t, dir)
	require.Empty(t, readReclaimRecord(t, base), "the record is dropped")
	m.waitTrash()
	require.Empty(t, trashEntries(t, base), "the moved-aside data is deleted in the background")
}

func TestReclaimRetainKeeps(t *testing.T) {
	m, base := newEmptyVM(t)
	dir := filepath.Join(base, "volumes", "u")
	m.Apply([]api.DesiredVolume{dv("u", "exclusive", dsl.ReclaimRetain)})
	m.Apply(nil)
	m.Apply(nil)
	require.DirExists(t, dir)
	require.Empty(t, readReclaimRecord(t, base))
}

func TestReclaimSurvivesRestart(t *testing.T) {
	m, base := newEmptyVM(t)
	dir := filepath.Join(base, "volumes", "u")
	m.Apply([]api.DesiredVolume{dv("u", "exclusive", dsl.ReclaimDelete)})
	require.DirExists(t, dir)

	m2, err := NewVolumeManager(base)
	require.NoError(t, err)
	advance := withClock(m2)
	require.Empty(t, m2.Apply(nil))
	require.DirExists(t, dir, "the grace starts when the volume is first seen absent")
	advance(reclaimGrace)
	require.Empty(t, m2.Apply(nil))
	require.NoDirExists(t, dir)
}

// Absent for less than the grace: kept. At the grace: reclaimed.
func TestReclaimWaitsForGrace(t *testing.T) {
	m, base := newEmptyVM(t)
	advance := withClock(m)
	dir := filepath.Join(base, "volumes", "u")
	m.Apply([]api.DesiredVolume{dv("u", "exclusive", dsl.ReclaimDelete)})

	require.Empty(t, m.Apply(nil))
	require.DirExists(t, dir)
	require.Empty(t, m.Names(), "not offered while waiting")
	require.Contains(t, readLeftSetAt(t, base), "u")

	advance(reclaimGrace - time.Second)
	require.Empty(t, m.Apply(nil))
	require.DirExists(t, dir, "absent for less than the grace")

	advance(time.Second)
	require.Empty(t, m.Apply(nil))
	require.NoDirExists(t, dir, "absent for the full grace")
}

// A volume that comes back within the grace keeps its data, and its clock
// restarts from zero the next time it leaves.
func TestReclaimGraceResetsWhenBack(t *testing.T) {
	m, base := newEmptyVM(t)
	advance := withClock(m)
	dir := filepath.Join(base, "volumes", "u")
	m.Apply([]api.DesiredVolume{dv("u", "exclusive", dsl.ReclaimDelete)})
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644))

	m.Apply(nil)
	advance(reclaimGrace - time.Second)
	sts := m.Apply([]api.DesiredVolume{dv("u", "exclusive", dsl.ReclaimDelete)})
	require.True(t, sts[0].Ready)
	require.Empty(t, readLeftSetAt(t, base), "the clock is cleared when it is delivered again")

	m.Apply(nil)
	advance(reclaimGrace - time.Second)
	require.Empty(t, m.Apply(nil))
	require.FileExists(t, filepath.Join(dir, "f"), "the clock restarted when it left again")
	advance(time.Second)
	m.Apply(nil)
	require.NoDirExists(t, dir)
}

// A restart in the middle of the grace keeps the clock: the new process does
// not start it again from zero.
func TestReclaimGraceSurvivesRestart(t *testing.T) {
	m, base := newEmptyVM(t)
	advance := withClock(m)
	dir := filepath.Join(base, "volumes", "u")
	m.Apply([]api.DesiredVolume{dv("u", "exclusive", dsl.ReclaimDelete)})
	m.Apply(nil)
	advance(reclaimGrace / 2)

	m2, err := NewVolumeManager(base)
	require.NoError(t, err)
	m2.now = m.now
	require.Empty(t, m2.Apply(nil))
	require.DirExists(t, dir)
	advance(reclaimGrace / 2)
	require.Empty(t, m2.Apply(nil))
	require.NoDirExists(t, dir, "the recorded clock carried over the restart")
}

// Reclaim only moves the data aside: Apply returns while the slow delete is
// still running, and the name can be served again at once from a fresh, empty
// directory.
func TestReclaimDoesNotBlockOnASlowDelete(t *testing.T) {
	m, base := newEmptyVM(t)
	advance := withClock(m)
	dir := filepath.Join(base, "volumes", "u")
	m.Apply([]api.DesiredVolume{dv("u", "exclusive", dsl.ReclaimDelete)})
	require.NoError(t, os.WriteFile(filepath.Join(dir, "old"), []byte("x"), 0o644))

	started := make(chan string, 1)
	unblock := make(chan struct{})
	m.removeAll = func(p string) error {
		started <- p
		<-unblock
		return os.RemoveAll(p)
	}
	t.Cleanup(func() {
		select {
		case <-unblock:
		default:
			close(unblock)
		}
		m.waitTrash()
	})

	m.Apply(nil)
	advance(reclaimGrace)
	done := make(chan []api.AgentVolumeStatus, 1)
	go func() { done <- m.Apply(nil) }()
	select {
	case sts := <-done:
		require.Empty(t, sts)
	case <-time.After(5 * time.Second):
		t.Fatal("Apply waited for the background delete")
	}
	var trash string
	select {
	case trash = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the background delete never started")
	}
	require.True(t, strings.HasPrefix(filepath.Base(trash), reclaimTrashPrefix+"u-"), trash)
	require.Equal(t, filepath.Join(base, "volumes"), filepath.Dir(trash))
	require.NoDirExists(t, dir)

	sts := m.Apply([]api.DesiredVolume{dv("u", "exclusive", dsl.ReclaimDelete)})
	require.Equal(t, []api.AgentVolumeStatus{{Name: "u", Ready: true}}, sts)
	require.DirExists(t, dir)
	require.NoFileExists(t, filepath.Join(dir, "old"), "a fresh, empty directory")
	require.DirExists(t, trash, "the old tree is still being deleted")

	close(unblock)
	m.waitTrash()
	require.NoDirExists(t, trash)
	require.DirExists(t, dir)
}

// Moved-aside data a previous process did not finish deleting is removed in
// the background at start; nothing else under volumes/ is touched.
func TestReclaimLeftoversRemovedAtStart(t *testing.T) {
	base := t.TempDir()
	vols := filepath.Join(base, "volumes")
	leftover := filepath.Join(vols, reclaimTrashPrefix+"u-123")
	require.NoError(t, os.MkdirAll(filepath.Join(leftover, "deep"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(leftover, "deep", "f"), []byte("x"), 0o644))
	keep := filepath.Join(vols, "u")
	require.NoError(t, os.MkdirAll(keep, 0o755))
	plainFile := filepath.Join(vols, reclaimTrashPrefix+"file")
	require.NoError(t, os.WriteFile(plainFile, []byte("x"), 0o644))

	m, err := NewVolumeManager(base)
	require.NoError(t, err)
	m.waitTrash()
	require.NoDirExists(t, leftover)
	require.DirExists(t, keep)
	require.FileExists(t, plainFile, "only directories are taken")
}

// The background deleter refuses any path that is not moved-aside data.
func TestTrashDeleterRefusesOtherPaths(t *testing.T) {
	m, base := newEmptyVM(t)
	dir := filepath.Join(base, "volumes", "u")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	nested := filepath.Join(base, "volumes", "u", reclaimTrashPrefix+"x")
	require.NoError(t, os.MkdirAll(nested, 0o755))
	m.scheduleTrash(dir)
	m.scheduleTrash(nested)
	m.scheduleTrash(base)
	m.waitTrash()
	require.DirExists(t, dir)
	require.DirExists(t, nested)
}

func TestReclaimNotWhenBackInSet(t *testing.T) {
	m, base := newEmptyVM(t)
	dir := filepath.Join(base, "volumes", "u")
	m.Apply([]api.DesiredVolume{dv("u", "exclusive", dsl.ReclaimDelete)})
	_, rel, err := m.Reserve("r1", []string{"u"})
	require.NoError(t, err)

	m.Apply(nil)
	sts := m.Apply([]api.DesiredVolume{dv("u", "exclusive", dsl.ReclaimDelete)})
	require.True(t, sts[0].Ready)
	rel()
	sts = m.Apply([]api.DesiredVolume{dv("u", "exclusive", dsl.ReclaimDelete)})
	require.True(t, sts[0].Ready)
	require.DirExists(t, dir)
	require.Equal(t, []string{"u"}, m.Names())
}

func TestReclaimNotWhenDeliveredButInvalid(t *testing.T) {
	m, base := newEmptyVM(t)
	dir := filepath.Join(base, "volumes", "u")
	m.Apply([]api.DesiredVolume{dv("u", "exclusive", dsl.ReclaimDelete)})
	sts := m.Apply([]api.DesiredVolume{dv("u", "bogus", dsl.ReclaimDelete)})
	require.False(t, sts[0].Ready)
	require.DirExists(t, dir, "a still-declared volume is never deleted")
}

func TestReclaimFailureReported(t *testing.T) {
	m, base := newEmptyVM(t)
	advance := withClock(m)
	dir := filepath.Join(base, "volumes", "u")
	m.Apply([]api.DesiredVolume{dv("u", "exclusive", dsl.ReclaimDelete)})
	fail := true
	m.rename = func(from, to string) error {
		if fail {
			return errors.New("disk on fire")
		}
		return os.Rename(from, to)
	}

	m.Apply(nil)
	advance(reclaimGrace)
	sts := m.Apply(nil)
	require.Len(t, sts, 1)
	require.Equal(t, "u", sts[0].Name)
	require.False(t, sts[0].Ready)
	require.Contains(t, sts[0].Reason, "reclaim:")
	require.DirExists(t, dir)
	require.Equal(t, map[string]string{"u": "Delete"}, readReclaimRecord(t, base), "the record is kept")

	fail = false
	require.Empty(t, m.Apply(nil), "retried on the next Apply")
	require.NoDirExists(t, dir)
	require.Empty(t, readReclaimRecord(t, base))
}

// A symlink planted at the volume's path must never be followed by a reclaim:
// the target outside the workspace survives and the failure is reported.
func TestReclaimRefusesASymlink(t *testing.T) {
	m, base := newEmptyVM(t)
	advance := withClock(m)
	m.Apply([]api.DesiredVolume{dv("u", "exclusive", dsl.ReclaimDelete)})
	dir := filepath.Join(base, "volumes", "u")
	require.NoError(t, os.Remove(dir))
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "precious"), []byte("x"), 0o644))
	if err := os.Symlink(outside, dir); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	m.Apply(nil)
	advance(reclaimGrace)
	sts := m.Apply(nil)
	require.Len(t, sts, 1)
	require.False(t, sts[0].Ready)
	require.Contains(t, sts[0].Reason, "reclaim:")
	require.FileExists(t, filepath.Join(outside, "precious"))
	require.Equal(t, map[string]string{"u": "Delete"}, readReclaimRecord(t, base), "kept, so it is retried and stays visible")
}

func TestReclaimRefusesAPlainFileAndHostileNames(t *testing.T) {
	m, base := newEmptyVM(t)
	require.NoError(t, os.MkdirAll(filepath.Join(base, "volumes"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(base, "volumes", "f"), []byte("x"), 0o644))
	require.Error(t, m.reclaimVolume("f"))
	require.FileExists(t, filepath.Join(base, "volumes", "f"))

	sentinel := filepath.Join(base, "sentinel")
	require.NoError(t, os.MkdirAll(sentinel, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(base, "volumes", ".hidden"), 0o755))
	for _, name := range []string{"", ".", "..", "../sentinel", `..\sentinel`, "a/b", ".hidden"} {
		require.Error(t, m.reclaimVolume(name), "name %q", name)
	}
	require.DirExists(t, sentinel)
	require.DirExists(t, filepath.Join(base, "volumes"))
	require.DirExists(t, filepath.Join(base, "volumes", ".hidden"))
}

// A Delete-to-Retain change delivered while the volume cannot be applied is
// still recorded, so removing the volume afterwards never deletes it.
func TestReclaimRetainFlipRecordedWhileApplyFails(t *testing.T) {
	m, base := newEmptyVM(t)
	advance := withClock(m)
	dir := filepath.Join(base, "volumes", "u")
	m.Apply([]api.DesiredVolume{dv("u", "exclusive", dsl.ReclaimDelete)})

	sts := m.Apply([]api.DesiredVolume{dv("u", "bogus", dsl.ReclaimRetain)})
	require.False(t, sts[0].Ready)
	require.Equal(t, map[string]string{"u": "Retain"}, readReclaimRecord(t, base))

	m.Apply(nil)
	advance(reclaimGrace)
	m.Apply(nil)
	require.DirExists(t, dir)
}

func TestReclaimIgnoresACorruptRecord(t *testing.T) {
	base := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(base, volumeReclaimFile), []byte("{not json"), 0o644))
	dir := filepath.Join(base, "volumes", "u")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	m, err := NewVolumeManager(base)
	require.NoError(t, err)
	require.Empty(t, m.Apply(nil))
	require.DirExists(t, dir)
}

func TestLinkCleanupAfterRemoval(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native directory links need a junction on Windows; covered by the unix run")
	}
	m, base := newEmptyVM(t)
	m.Apply([]api.DesiredVolume{dv("u", "exclusive", dsl.ReclaimRetain)})
	_, rel, err := m.Reserve("r1", []string{"u"})
	require.NoError(t, err)

	link := filepath.Join(t.TempDir(), "cache")
	require.NoError(t, m.installLink(link, m.PathOf("u")))

	m.Apply(nil) // the volume leaves while the run still uses it
	require.NoError(t, m.removeOurLink(link))
	_, err = os.Lstat(link)
	require.True(t, os.IsNotExist(err), "the link is removed, not left as 'replaced by something else'")
	rel()
	m.Apply(nil)
	require.DirExists(t, filepath.Join(base, "volumes", "u"))
}

func TestOwnsLinkTargetBeforeFirstApply(t *testing.T) {
	m, base := newEmptyVM(t)
	require.True(t, m.ownsLinkTarget(filepath.Join(base, "volumes", "old", "sub")),
		"the startup sweep runs before Apply and must still recognise links into volumes")
	require.False(t, m.ownsLinkTarget(filepath.Join(base, "elsewhere")))
}

func TestApplyNilManager(t *testing.T) {
	var m *VolumeManager
	require.Nil(t, m.Apply([]api.DesiredVolume{dv("u", "exclusive", dsl.ReclaimRetain)}))
	require.Empty(t, m.Paths())
}

func TestStoragePathsFollowLiveVolumes(t *testing.T) {
	m, base := newEmptyVM(t)
	a := &Agent{Volumes: m}
	m.Apply([]api.DesiredVolume{dv("u", "exclusive", dsl.ReclaimRetain)})
	dir := filepath.Join(base, "volumes", "u")
	require.Equal(t, []string{base, dir}, a.storagePaths(base))
	_, rel, err := m.Reserve("r1", []string{"u"})
	require.NoError(t, err)

	m.Apply(nil) // retiring, still held
	require.Equal(t, []string{base, dir}, a.storagePaths(base), "a retiring volume is still sampled")
	rel()
	m.Apply(nil)
	require.Equal(t, []string{base}, a.storagePaths(base), "dropped once forgotten")
}

// A run already waiting for an exclusive volume must keep waiting, not fail,
// when a pending mode switch makes the volume briefly not offered.
func TestApplyModeSwitchDoesNotFailWaiters(t *testing.T) {
	m, _ := newEmptyVM(t)
	m.Apply([]api.DesiredVolume{dv("u", "exclusive", dsl.ReclaimRetain)})
	_, rel, err := m.Reserve("r1", []string{"u"})
	require.NoError(t, err)

	type res struct {
		names []string
		err   error
	}
	ch := make(chan res, 1)
	go func() {
		n, r, err := m.ReserveSet(context.Background(), "r2", [][]string{{"u"}}, nil, nil)
		if r != nil {
			defer r()
		}
		ch <- res{n, err}
	}()
	requireNoResult := func() {
		select {
		case r := <-ch:
			t.Fatalf("waiter returned early: %v %v", r.names, r.err)
		case <-time.After(150 * time.Millisecond):
		}
	}
	requireNoResult()
	m.Apply([]api.DesiredVolume{dv("u", "shared", dsl.ReclaimRetain)}) // switch pending
	requireNoResult()

	rel()
	// Nothing hands the volume out between the release and the switch.
	requireNoResult()
	m.Apply([]api.DesiredVolume{dv("u", "shared", dsl.ReclaimRetain)})
	// The waiter only wakes on a release, so poke it: a fresh holder cycle.
	_, r3, err := m.Reserve("poke", []string{"u"})
	require.NoError(t, err)
	r3()
	select {
	case r := <-ch:
		require.NoError(t, r.err)
		require.Equal(t, []string{"u"}, r.names)
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter did not proceed after the switch completed")
	}
}
