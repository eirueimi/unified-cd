package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

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
	dir := filepath.Join(base, "volumes", "u")
	m.Apply([]api.DesiredVolume{dv("u", "exclusive", dsl.ReclaimDelete)})
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644))
	_, rel, err := m.Reserve("r1", []string{"u"})
	require.NoError(t, err)

	require.Empty(t, m.Apply(nil))
	require.DirExists(t, dir, "a held volume is not reclaimed")
	require.Equal(t, map[string]string{"u": "Delete"}, readReclaimRecord(t, base))

	rel()
	require.Empty(t, m.Apply(nil))
	require.NoDirExists(t, dir)
	require.Empty(t, readReclaimRecord(t, base), "the record is dropped")
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
	require.Empty(t, m2.Apply(nil))
	require.NoDirExists(t, dir)
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
	dir := filepath.Join(base, "volumes", "u")
	m.Apply([]api.DesiredVolume{dv("u", "exclusive", dsl.ReclaimDelete)})
	fail := true
	m.removeAll = func(p string) error {
		if fail {
			return errors.New("disk on fire")
		}
		return os.RemoveAll(p)
	}

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
	m.Apply([]api.DesiredVolume{dv("u", "exclusive", dsl.ReclaimDelete)})
	dir := filepath.Join(base, "volumes", "u")
	require.NoError(t, os.Remove(dir))
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "precious"), []byte("x"), 0o644))
	if err := os.Symlink(outside, dir); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

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
	for _, name := range []string{"", ".", "..", "../sentinel", `..\sentinel`, "a/b"} {
		require.Error(t, m.reclaimVolume(name), "name %q", name)
	}
	require.DirExists(t, sentinel)
	require.DirExists(t, filepath.Join(base, "volumes"))
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
