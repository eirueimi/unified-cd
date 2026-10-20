package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// volumeLocksDir is where an agent process records which exclusive volumes it
// holds, under the workspace base. Like volumeModesDir it is kept outside the
// volumes, so a volume's contents stay exactly what the job put there, and it
// is dot-prefixed so it can never collide with a volume or a job workspace.
//
// These records are a DIAGNOSTIC, not a lock. The authority on who holds a
// volume is the in-process table in VolumeManager, and nothing here changes
// what a run does. They exist because that table is per process, and two
// processes can end up on one volume directory without either knowing: one
// agent ID started twice with an overlapping restart, or (more likely) two
// agents on one machine sharing a workspaceDir, where both default to
// <workspaceDir>/volumes/<name>. Then "exclusive" silently stops being true and
// two runs share one checkout. The records make that visible.
//
// Layout: one directory per volume, one file per agent PROCESS that holds it.
// Per process rather than per volume so that each of two processes sees the
// other's file, however their holds overlap in time. The limit: only processes
// that share a workspaceDir see each other. Two agents with different
// workspaceDirs and the same explicit volume path cannot be told apart from
// here.
const volumeLocksDir = ".ucd-volume-locks"

// volumeLockRecord is the content of one record.
type volumeLockRecord struct {
	PID      int       `json:"pid"`
	Host     string    `json:"host"`
	Instance string    `json:"instance"` // one per VolumeManager, so two in one process differ
	RunID    string    `json:"runId"`
	Since    time.Time `json:"since"`
}

// volumeLockState is the VolumeManager's side of the diagnostic.
type volumeLockState struct {
	instance string
	host     string
	pid      int
	// alive says whether a process exists on this machine; processAlive, a
	// field so a test can name a process that is, or is not, running.
	alive func(pid int) bool

	mu       sync.Mutex
	warnings map[string][]string // runID -> warnings not yet shown to the run
}

func newVolumeLockState() *volumeLockState {
	host, _ := os.Hostname()
	var b [6]byte
	_, _ = rand.Read(b[:])
	return &volumeLockState{
		instance: hex.EncodeToString(b[:]),
		host:     host,
		pid:      os.Getpid(),
		alive:    processAlive,
		warnings: map[string][]string{},
	}
}

// volumeLockDir is where the records of volume name live.
func volumeLockDir(wsBase, name string) string {
	// sanitizeJobName, because a volume called "nul" must not become a Windows
	// device name.
	return filepath.Join(wsBase, volumeLocksDir, sanitizeJobName(name))
}

func (s *volumeLockState) ownFile(dir string) string {
	return filepath.Join(dir, fmt.Sprintf("%d-%s.json", s.pid, s.instance))
}

// stale reports whether rec belongs to a process that is certainly gone: one on
// this machine that is not running, or this very PID under another instance (the
// PID of a crashed agent handed to its replacement, which is routine when the
// agent runs as PID 1 in a container). A record from another host is never stale
// here: it is a live conflict on shared storage, or at worst an old one.
func (s *volumeLockState) stale(rec volumeLockRecord) bool {
	if rec.Host != s.host {
		return false
	}
	if rec.PID == s.pid {
		return rec.Instance != s.instance
	}
	return !s.alive(rec.PID)
}

func readVolumeLock(p string) (volumeLockRecord, bool) {
	var rec volumeLockRecord
	b, err := os.ReadFile(p)
	if err != nil || json.Unmarshal(b, &rec) != nil || rec.Instance == "" {
		return rec, false
	}
	return rec, true
}

// markHeld records, for each exclusive volume in names, that this process holds
// it on behalf of runID, and returns the func that removes the records. When a
// record of another live process is already there it queues a warning for the
// run (see TakeWarnings) and logs it. It never fails: a diagnostic that cannot
// be written is logged and the run proceeds. Shared volumes are skipped; being
// held by several runs at once is their meaning.
//
// The returned func must run BEFORE the in-process hold is released: the file
// name is per process and volume, so a run that took the volume the moment it
// was released would otherwise have its record removed by its predecessor.
func (m *VolumeManager) markHeld(runID string, names []string) func() {
	s := m.locks
	var written []string
	for _, n := range names {
		v := m.vols[n]
		if v == nil || v.mode != VolumeModeExclusive {
			continue
		}
		dir := volumeLockDir(m.base, n)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			slog.Warn("could not record that this process holds a volume; two agent processes sharing it would go unnoticed", "volume", n, "error", err)
			continue
		}
		own := s.ownFile(dir)
		if entries, err := os.ReadDir(dir); err == nil {
			for _, e := range entries {
				p := filepath.Join(dir, e.Name())
				if p == own {
					continue
				}
				rec, ok := readVolumeLock(p)
				if !ok {
					continue
				}
				if s.stale(rec) {
					_ = os.Remove(p)
					continue
				}
				msg := fmt.Sprintf("volume %q is also held by another agent process (pid %d on %s, run %s, since %s). "+
					"An exclusive volume is only exclusive within one agent process, so this run and that one may be using the same data at once. "+
					"Give each agent its own workspaceDir (or its own explicit path for this volume), and start each agent ID once. "+
					"This is a warning: the run continues. If no other agent is running on this directory (a crashed agent left the record), remove %s",
					n, rec.PID, rec.Host, rec.RunID, rec.Since.UTC().Format(time.RFC3339), p)
				slog.Warn(msg, "runId", runID, "volume", n)
				s.mu.Lock()
				s.warnings[runID] = append(s.warnings[runID], msg)
				s.mu.Unlock()
			}
		}
		b, _ := json.Marshal(volumeLockRecord{PID: s.pid, Host: s.host, Instance: s.instance, RunID: runID, Since: time.Now().UTC()})
		if err := os.WriteFile(own, b, 0o644); err != nil {
			slog.Warn("could not record that this process holds a volume; two agent processes sharing it would go unnoticed", "volume", n, "error", err)
			continue
		}
		written = append(written, own)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			for _, p := range written {
				_ = os.Remove(p)
			}
		})
	}
}

// TakeWarnings returns, and forgets, what the volume diagnostic has to say to
// runID about the volumes it just reserved.
func (m *VolumeManager) TakeWarnings(runID string) []string {
	if m == nil {
		return nil
	}
	s := m.locks
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.warnings[runID]
	delete(s.warnings, runID)
	return w
}

// sweepStaleVolumeLocks removes the records a process that died left behind. It
// runs once at startup. A record of a process that is still running, here or
// anywhere else, is left alone: that is the very thing the records are for.
func (m *VolumeManager) sweepStaleVolumeLocks() {
	if m == nil {
		return
	}
	root := filepath.Join(m.base, volumeLocksDir)
	dirs, err := os.ReadDir(root)
	if err != nil {
		return
	}
	removed := 0
	for _, d := range dirs {
		files, err := os.ReadDir(filepath.Join(root, d.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			p := filepath.Join(root, d.Name(), f.Name())
			if rec, ok := readVolumeLock(p); !ok || m.locks.stale(rec) {
				if os.Remove(p) == nil {
					removed++
				}
			}
		}
	}
	if removed > 0 {
		slog.Info("removed volume holder records left by an agent process that is gone", "count", removed)
	}
}
