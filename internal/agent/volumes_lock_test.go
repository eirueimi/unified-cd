package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/config"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

// asOtherProcess makes m present itself as a different agent process (pid), as
// two real processes on one machine would, and have every pid it is asked about
// be running unless dead says otherwise.
func asOtherProcess(m *VolumeManager, pid int, dead ...int) {
	m.locks.pid = pid
	m.locks.alive = func(p int) bool {
		for _, d := range dead {
			if p == d {
				return false
			}
		}
		return true
	}
}

func lockFiles(t *testing.T, base, volume string) []string {
	t.Helper()
	entries, err := os.ReadDir(volumeLockDir(base, volume))
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func twoProcessesOneBase(t *testing.T, cfgs ...config.VolumeConfig) (a, b *VolumeManager, base string) {
	t.Helper()
	base = t.TempDir()
	var err error
	a, err = NewVolumeManager(base, cfgs)
	require.NoError(t, err)
	b, err = NewVolumeManager(base, cfgs)
	require.NoError(t, err)
	asOtherProcess(a, 4101)
	asOtherProcess(b, 4102)
	return a, b, base
}

// Two agent processes on one workspaceDir each keep their own in-process table,
// so both hand out the same exclusive volume. The second says so, to the run,
// and does not refuse it: the diagnostic decides nothing.
func TestVolumeLocks_TwoProcessesOnOneVolumeDirectoryWarn(t *testing.T) {
	a, b, base := twoProcessesOneBase(t, exclusive("unity"))
	ctx := context.Background()

	_, relA, err := a.ReserveSet(ctx, "run-a", [][]string{{"unity"}}, nil, nil)
	require.NoError(t, err)
	assert.Empty(t, a.TakeWarnings("run-a"), "the first holder has nothing to be told")

	_, relB, err := b.ReserveSet(ctx, "run-b", [][]string{{"unity"}}, nil, nil)
	require.NoError(t, err, "the diagnostic is not an authority: the second run still gets the volume")
	warn := b.TakeWarnings("run-b")
	require.Len(t, warn, 1)
	assert.Contains(t, warn[0], `volume "unity" is also held by another agent process`)
	assert.Contains(t, warn[0], "pid 4101")
	assert.Contains(t, warn[0], "run-a")
	assert.Contains(t, warn[0], "workspaceDir")
	assert.Empty(t, b.TakeWarnings("run-b"), "a warning is shown once")

	assert.Len(t, lockFiles(t, base, "unity"), 2, "each process records its own hold")

	// The other direction too: A takes it again while B still holds it.
	relA()
	_, relA2, err := a.ReserveSet(ctx, "run-a2", [][]string{{"unity"}}, nil, nil)
	require.NoError(t, err)
	assert.Len(t, a.TakeWarnings("run-a2"), 1, "A sees B, however the two holds overlap in time")

	relA2()
	relB()
	assert.Empty(t, lockFiles(t, base, "unity"), "a release removes the record")
}

// A record of a process that is no longer running is not a conflict.
func TestVolumeLocks_ADeadProcessIsNotAConflict(t *testing.T) {
	a, b, _ := twoProcessesOneBase(t, exclusive("unity"))
	asOtherProcess(b, 4102, 4101) // 4101 is gone
	_, _, err := a.ReserveSet(context.Background(), "run-a", [][]string{{"unity"}}, nil, nil)
	require.NoError(t, err)
	_, rel, err := b.ReserveSet(context.Background(), "run-b", [][]string{{"unity"}}, nil, nil)
	require.NoError(t, err)
	defer rel()
	assert.Empty(t, b.TakeWarnings("run-b"))
}

// The PID of a crashed agent is routinely handed to its replacement (PID 1 in a
// container), so the same PID under another instance is stale, not a conflict.
func TestVolumeLocks_TheSamePidUnderAnotherInstanceIsStale(t *testing.T) {
	a, b, _ := twoProcessesOneBase(t, exclusive("unity"))
	asOtherProcess(b, 4101) // the replacement got the crashed agent's PID
	_, _, err := a.ReserveSet(context.Background(), "run-a", [][]string{{"unity"}}, nil, nil)
	require.NoError(t, err)
	_, rel, err := b.ReserveSet(context.Background(), "run-b", [][]string{{"unity"}}, nil, nil)
	require.NoError(t, err)
	defer rel()
	assert.Empty(t, b.TakeWarnings("run-b"))
}

// The record is a file under the workspace base, never in the volume, which
// holds exactly what the job put there. A shared volume is held by many runs at
// once by design and leaves no record.
func TestVolumeLocks_RecordsStayOutOfTheVolumeAndSkipSharedVolumes(t *testing.T) {
	m, base := newTestVolumes(t, exclusive("unity"), shared("cache"))
	_, rel, err := m.ReserveSet(context.Background(), "run-1", [][]string{{"unity"}, {"cache"}}, nil, nil)
	require.NoError(t, err)

	files := lockFiles(t, base, "unity")
	require.Len(t, files, 1)
	assert.Empty(t, lockFiles(t, base, "cache"), "a shared volume leaves no record")
	entries, err := os.ReadDir(m.PathOf("unity"))
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing is written into the volume")

	b, err := os.ReadFile(filepath.Join(volumeLockDir(base, "unity"), files[0]))
	require.NoError(t, err)
	var rec volumeLockRecord
	require.NoError(t, json.Unmarshal(b, &rec))
	assert.Equal(t, os.Getpid(), rec.PID)
	assert.Equal(t, "run-1", rec.RunID)
	assert.WithinDuration(t, time.Now(), rec.Since, time.Minute)

	rel()
	assert.Empty(t, lockFiles(t, base, "unity"))
}

// A volume's name reaches a path, so it is run through sanitizeJobName: a
// volume called "nul" must not name a Windows device.
func TestVolumeLocks_TheVolumeNameIsSanitizedIntoTheRecordPath(t *testing.T) {
	base := filepath.Join("x", "ws")
	assert.Equal(t, filepath.Join(base, ".ucd-volume-locks", "job_nul"), volumeLockDir(base, "nul"))
	assert.Equal(t, filepath.Join(base, ".ucd-volume-locks", "a-b"), volumeLockDir(base, "a/b"))

	m, mbase := newTestVolumes(t, exclusive("nul"))
	_, rel, err := m.ReserveSet(context.Background(), "r", [][]string{{"nul"}}, nil, nil)
	require.NoError(t, err)
	assert.Len(t, lockFiles(t, mbase, "nul"), 1)
	rel()
}

// A hold taken and released again, by the same process, leaves nothing: the
// record is removed before the in-process hold is let go, so the run that takes
// the volume next is never robbed of its own record.
func TestVolumeLocks_SuccessiveRunsEachKeepTheirOwnRecord(t *testing.T) {
	m, base := newTestVolumes(t, exclusive("unity"))
	_, rel1, err := m.ReserveSet(context.Background(), "run-1", [][]string{{"unity"}}, nil, nil)
	require.NoError(t, err)
	rel1()
	_, rel2, err := m.ReserveSet(context.Background(), "run-2", [][]string{{"unity"}}, nil, nil)
	require.NoError(t, err)
	assert.Len(t, lockFiles(t, base, "unity"), 1)
	rel1() // idempotent: must not remove run-2's record
	assert.Len(t, lockFiles(t, base, "unity"), 1)
	rel2()
	assert.Empty(t, lockFiles(t, base, "unity"))

	// Reserve (the single-volume entry point) records too.
	_, rel3, err := m.Reserve("run-3", []string{"unity"})
	require.NoError(t, err)
	assert.Len(t, lockFiles(t, base, "unity"), 1)
	rel3()
	assert.Empty(t, lockFiles(t, base, "unity"))
}

// At startup the records of processes that are gone are swept, and those of
// processes that are still running (the thing the records are for) are kept.
func TestVolumeLocks_StartupSweepRemovesOnlyStaleRecords(t *testing.T) {
	a, b, base := twoProcessesOneBase(t, exclusive("unity"))
	_, _, err := a.ReserveSet(context.Background(), "run-a", [][]string{{"unity"}}, nil, nil)
	require.NoError(t, err)

	dir := volumeLockDir(base, "unity")
	foreign := volumeLockRecord{PID: 1, Host: "another-machine", Instance: "ffff", RunID: "run-x", Since: time.Now()}
	raw, _ := json.Marshal(foreign)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "1-ffff.json"), raw, 0o644))
	dead := volumeLockRecord{PID: 4999, Host: a.locks.host, Instance: "dead", RunID: "run-d", Since: time.Now()}
	raw, _ = json.Marshal(dead)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "4999-dead.json"), raw, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "garbage.json"), []byte("{"), 0o644))

	asOtherProcess(b, 4102, 4999)
	b.sweepStaleVolumeLocks()

	assert.ElementsMatch(t, []string{"1-ffff.json", filepath.Base(a.locks.ownFile(dir))}, lockFiles(t, base, "unity"),
		"the dead process's record and the unreadable one go; a live process's and another machine's stay")
}

// The warning reaches the run's own log, where its owner will read it, without
// failing the run.
func TestReserveClaimVolumes_WarnsTheRunThatSharesAVolumeDirectory(t *testing.T) {
	const agentID = "lock-agent"
	h := newIsolatedHarness()
	srv := newIsolatedServer(t, agentID, h)

	a, b, _ := twoProcessesOneBase(t, exclusive("unity"))
	_, _, err := a.ReserveSet(context.Background(), "run-a", [][]string{{"unity"}}, nil, nil)
	require.NoError(t, err)

	ag := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok"), Volumes: b}
	c := api.ClaimResponse{RunID: "run-b", JobName: "j", Volumes: []dsl.VolumeMount{vmount("unity", "u", ".")}}
	release, err := ag.reserveClaimVolumes(context.Background(), context.Background(), &c)
	require.NoError(t, err, "the run is not failed")
	defer release()

	h.mu.Lock()
	defer h.mu.Unlock()
	require.Len(t, h.logsByStep[-1], 1)
	assert.Contains(t, h.logsByStep[-1][0].Line, "also held by another agent process")
}
