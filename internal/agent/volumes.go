package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Volume modes, as spelled in the agent config and returned by ModeOf.
const (
	// VolumeModeExclusive: at most one run on this agent holds the volume.
	VolumeModeExclusive = "exclusive"
	// VolumeModeShared: any number of runs may hold the volume at once.
	VolumeModeShared = "shared"
)

// volume is one live entry of the agent's volume set.
type volume struct {
	mode string
	path string
	// retiring: the volume left the desired set while runs still hold it. It is
	// not offered or assigned; it is forgotten when the last holder releases.
	retiring bool
	// switching: the desired mode differs from mode and holders must release
	// first. The volume is not offered until the switch completes.
	switching bool
}

// offered reports whether new claims may be given the volume.
func (v *volume) offered() bool { return !v.retiring && !v.switching }

// VolumeManager owns the agent's named volumes: it applies the set the
// controller delivers (Apply), creates the backing directories, and enforces
// the exclusion the declarations promise. Holders are not persisted: every
// volume is free when the agent starts, because the agent has lost all its
// runs. Only the reclaim policy of served volumes is recorded (see
// volumes_apply.go).
//
// A volume name exists once per agent, so two agents declaring the same name
// hold two independent copies; the controller stores no volume state.
type VolumeManager struct {
	base string // absolute workspace dir

	mu sync.Mutex
	// vols holds every live entry, offered or retiring. Guarded by mu.
	vols map[string]*volume
	// holders maps a volume name to the set of runs currently holding it. An
	// exclusive volume has at most one; a shared one any number.
	holders map[string]map[string]struct{}
	// paths holds the absolute native mount paths currently reserved.
	paths map[string]struct{}
	// changed is closed (and replaced) on every release, waking every waiter
	// to rescan. Waiters are few and a release is rare, so a broadcast is
	// cheaper than tracking who is waiting for what.
	changed chan struct{}

	// gated holds the volumes the disk gate currently withholds from new claims
	// (see SetGated). Guarded by mu, like holders, so an assignment reads the
	// same gate state the claim advertised.
	gated map[string]struct{}

	// pinned holds absolute native mount paths whose link could not be
	// removed: their reservation is never released (see pinPath).
	pinned map[string]struct{}

	// removeLink deletes a link; os.Remove, a field so a test can inject the
	// failure a real unlink only produces on a damaged or busy filesystem.
	removeLink func(string) error

	// removeAll deletes a reclaimed volume's directory (after it was moved
	// aside, in the background); os.RemoveAll, a field so a test can inject a
	// failure or a slow delete.
	removeAll func(string) error
	// rename moves a reclaimed volume's directory aside; os.Rename, a field so
	// a test can inject a failure.
	rename func(oldpath, newpath string) error
	// now is time.Now; a field so a test can move the reclaim grace clock.
	now func() time.Time

	// linkMu serialises edits to the on-disk registry of links native runs
	// have created (see volumes_mount.go), which every claim slot writes.
	linkMu sync.Mutex

	// locks is the diagnostic that notices another agent process on the same
	// volume directory (volumes_lock.go). It decides nothing.
	locks *volumeLockState

	// applyMu serialises Apply and guards reclaim.
	applyMu sync.Mutex
	// reclaim maps every served volume's name to its last known reclaim policy;
	// it mirrors <base>/.ucd-volume-reclaim.json. Guarded by applyMu.
	reclaim map[string]string
	// leftSetAt holds, for a Delete volume that is no longer delivered, when
	// it was first seen absent; it is reclaimed only once reclaimGrace has
	// passed since. Persisted with reclaim. Guarded by applyMu.
	leftSetAt map[string]time.Time

	// trash is the background deleter's queue of moved-aside reclaimed
	// directories (volumes_apply.go). Guarded by trashMu.
	trashMu   sync.Mutex
	trash     []string
	trashBusy bool
	trashWG   sync.WaitGroup
}

// NewVolumeManager returns an empty manager for the workspace directory base;
// volumes arrive through Apply. It loads the reclaim record a previous process
// left, so the first Apply can reclaim volumes that left the set meanwhile, and
// schedules the background removal of any reclaimed data a previous process
// moved aside but did not finish deleting.
func NewVolumeManager(base string) (*VolumeManager, error) {
	absBase, err := filepath.Abs(base)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace directory %q: %w", base, err)
	}
	m := &VolumeManager{
		vols:    make(map[string]*volume),
		base:    absBase,
		holders: make(map[string]map[string]struct{}),
		paths:   make(map[string]struct{}),
		pinned:  make(map[string]struct{}),

		removeLink: os.Remove,
		removeAll:  os.RemoveAll,
		rename:     os.Rename,
		now:        time.Now,
		changed:    make(chan struct{}),
		locks:      newVolumeLockState(),
	}
	m.reclaim, m.leftSetAt = m.loadReclaimRecord()
	m.scheduleLeftoverTrash()
	return m, nil
}

// validateVolumeConfigName accepts a single path segment: the name becomes a
// directory name in the default path, and is advertised verbatim.
func validateVolumeConfigName(name string) error {
	switch {
	case name == "":
		return errors.New("a volume in the agent config has no name")
	case name == "." || name == "..":
		return fmt.Errorf("volume name %q is not a valid directory name", name)
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("volume name %q must not contain a path separator", name)
	}
	return nil
}

// pathWithin reports whether child is parent or lies beneath it.
func pathWithin(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// checkOutOfGCReach refuses a volume path the workspace GC could delete.
//
// gcWorkspaces (workspace_gc.go) globs <base>/working* and removes aged job
// directories two levels down. The default volume path <base>/volumes/<name>
// is outside its reach only by the spelling of that glob, so the rule is
// enforced here rather than left to the glob: a volume that is, or sits under,
// a working* directory, or that contains the whole workspace (and so the
// working* directories inside it), could be deleted for going unused past
// workspaceRetentionDays, destroying exactly the data the operator declared a
// volume to keep.
func checkOutOfGCReach(base, p string) error {
	if pathWithin(p, base) {
		return fmt.Errorf("path %s is, or contains, the workspace directory %s, so the workspace GC could delete the volume's data", p, base)
	}
	if !pathWithin(base, p) {
		return nil
	}
	rel, err := filepath.Rel(base, p)
	if err != nil {
		return nil
	}
	first := strings.Split(rel, string(filepath.Separator))[0]
	// Case-folded on purpose: the glob is case-insensitive on Windows, and
	// refusing a harmless spelling elsewhere costs nothing.
	if strings.HasPrefix(strings.ToLower(first), "working") {
		return fmt.Errorf("path %s is under %s, which the workspace GC sweeps (it removes aged directories below %s/working*); choose a path outside any working* directory", p, filepath.Join(base, first), base)
	}
	// The agent also owns <base>/detached/<runID>, which it removes after every
	// detached run, and keeps its tooling (the ucd-sh shim in .ucd-tools, the
	// volume link registry) in dot-prefixed entries. A volume placed in either
	// would be destroyed or would collide with the agent's own files.
	if strings.EqualFold(first, "detached") || strings.HasPrefix(first, ".") {
		return fmt.Errorf("path %s is under %s, which the agent manages itself (per-run detached workspaces are deleted after the run; dot-prefixed entries hold agent tooling); choose another path", p, filepath.Join(base, first))
	}
	return nil
}

// Every method below is safe on a nil *VolumeManager, which is what an agent
// that declares no volumes holds (Agent.Volumes): it advertises nothing,
// reports nothing busy, and cannot reserve anything.

// Names returns the offered volume names, sorted: ready volumes only, not
// retiring ones and not ones waiting to switch mode. This is what the agent
// advertises as capabilities.
func (m *VolumeManager) Names() []string {
	if m == nil {
		return []string{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.vols))
	for n, v := range m.vols {
		if v.offered() {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

// Paths returns the host directory of every live volume, offered or retiring,
// sorted. The storage sampler measures these.
func (m *VolumeManager) Paths() []string {
	if m == nil {
		return []string{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.vols))
	for _, v := range m.vols {
		out = append(out, v.path)
	}
	sort.Strings(out)
	return out
}

// ModeOf returns the volume's mode, or "" for a name this agent does not
// declare.
func (m *VolumeManager) ModeOf(name string) string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.vols[name]; ok {
		return v.mode
	}
	return ""
}

// PathOf returns the volume's host directory, or "" for a name this agent does
// not declare.
func (m *VolumeManager) PathOf(name string) string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.vols[name]; ok {
		return v.path
	}
	return ""
}

// Reserve picks the first free volume among names (in order), holds it on
// behalf of runID, and returns its name with the func that releases it. It
// blocks while every known candidate is held by another run. The scan and the
// hold happen under one lock, so two concurrent slots cannot pick the same
// free exclusive volume.
//
// Reserve is re-entrant per run: if runID already holds one of names, that
// same name is returned with a no-op release and nothing is waited for. One
// job may name a volume in several entries (mounting one volume at two paths
// is legal), and a per-entry reservation would otherwise block the second
// entry on a volume its own run holds. The volume is released by the release
// func of the call that actually took it, so the caller must run that one last
// (at the end of the run), not the no-op ones.
//
// Names this agent does not declare are skipped. It is an error, rather than
// an endless wait, for none of names to be declared. The returned release is
// idempotent.
//
// Reserve cannot be interrupted; a caller that must be able to give up (run
// cancelled, agent draining) uses ReserveContext.
func (m *VolumeManager) Reserve(runID string, names []string) (string, func(), error) {
	return m.ReserveContext(context.Background(), runID, names)
}

// ReserveContext is Reserve with an abandonable wait: when ctx is done before a
// candidate frees, it returns an error wrapping ctx.Err() and holds nothing.
func (m *VolumeManager) ReserveContext(ctx context.Context, runID string, names []string) (string, func(), error) {
	return m.reserve(ctx, runID, names, nil)
}

// reserve is ReserveContext plus onWait, called once, without m.mu held, the
// first time the call finds nothing free and is about to block. The claim loop
// uses it to tell the run's log why it is not starting.
func (m *VolumeManager) reserve(ctx context.Context, runID string, names []string, onWait func()) (string, func(), error) {
	if m == nil {
		return "", nil, fmt.Errorf("reserve volume: this agent declares no volumes (wanted one of %q)", names)
	}
	if runID == "" {
		return "", nil, errors.New("reserve volume: run ID is required")
	}
	for {
		m.mu.Lock()
		known := m.usableLocked(runID, names)
		if len(known) == 0 {
			m.mu.Unlock()
			return "", nil, fmt.Errorf("reserve volume: this agent declares none of %q", names)
		}
		for _, n := range known {
			if _, ok := m.holders[n][runID]; ok {
				m.mu.Unlock()
				return n, func() {}, nil
			}
		}
		for _, n := range known {
			if v := m.vols[n]; !v.switching && (v.mode == VolumeModeShared || len(m.holders[n]) == 0) {
				if m.holders[n] == nil {
					m.holders[n] = make(map[string]struct{})
				}
				m.holders[n][runID] = struct{}{}
				m.mu.Unlock()
				unmark := m.markHeld(runID, []string{n})
				release := m.releaser(func() { m.dropHolder(n, runID) })
				return n, func() { unmark(); release() }, nil
			}
		}
		wait := m.changed
		m.mu.Unlock()
		if onWait != nil {
			onWait()
			onWait = nil
		}
		select {
		case <-wait:
		case <-ctx.Done():
			return "", nil, fmt.Errorf("waiting for volume %q: %w", known, ctx.Err())
		}
	}
}

// usableLocked filters names to the volumes runID may be a candidate for: all but
// retiring ones, plus any retiring one the run already holds. A volume waiting
// to switch mode stays a candidate (so a run already waiting for it keeps
// waiting) but is not free, see assignLocked and reserve. m.mu must be held.
func (m *VolumeManager) usableLocked(runID string, names []string) []string {
	var out []string
	for _, n := range names {
		v, ok := m.vols[n]
		if !ok {
			continue
		}
		if _, mine := m.holders[n][runID]; mine || !v.retiring {
			out = append(out, n)
		}
	}
	return out
}

// ReserveSet acquires everything a run needs in one atomic step: one volume for
// each entry of volumeSets (each entry lists its candidate names) and every
// absolute native mount path in absPaths. It returns the volume chosen for each
// entry, in order, and one release func for the lot.
//
// All or nothing, and a run that has to wait holds nothing. A run that took its
// resources one at a time and blocked on the next would hold some while waiting
// for others, and two such runs can each wait for what the other holds
// (hold-and-wait), whatever order the entries are taken in. Here the whole
// assignment is computed under the manager's lock; if it cannot be completed
// now, nothing is taken and the call waits for a release and tries again, so no
// cycle can form. ctx ends the wait, still holding nothing. onWait, if non-nil,
// is called once, outside the lock, the first time the call has to wait.
//
// The assignment is a backtracking search, not a greedy pass: [a b] with [a c]
// while c is busy elsewhere is satisfied by (b, a), not by taking a first and
// waiting for c. It is complete for any realistic job but bounded (see
// maxVolumeSearchNodes), because it runs under m.mu: a crafted job that would
// need an exhaustive search is logged and left waiting, not searched to the end.
//
// Two entries get distinct volumes whenever the candidates allow it. One run
// reuses a volume for a second entry only when every candidate that entry
// declares is already chosen by this run, which is the legal "one volume mounted
// at two paths" case (a single name written twice); it does not alias to avoid
// waiting. Whether distinct volumes are possible is judged on what the agent
// declares, not on what is free right now: if they are possible but busy, the
// call waits.
//
// A candidate this agent does not declare is not an alternative. It is an error,
// not a wait, for an entry to have no declared candidate. Reserve is separate
// and keeps its per-run re-entrancy.
func (m *VolumeManager) ReserveSet(ctx context.Context, runID string, volumeSets [][]string, absPaths []string, onWait func()) ([]string, func(), error) {
	if m == nil {
		return nil, nil, errors.New("reserve volumes: this agent declares no volumes")
	}
	if runID == "" {
		return nil, nil, errors.New("reserve volumes: run ID is required")
	}
	var keys []string
	seen := map[string]bool{}
	for _, p := range absPaths {
		slashed := filepath.ToSlash(p)
		if !filepath.IsAbs(p) && !strings.HasPrefix(slashed, "/") {
			return nil, nil, fmt.Errorf("reserve path: %q is not absolute", p)
		}
		if k := path.Clean(slashed); !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}

	warnedAbandoned := false
	for {
		m.mu.Lock()
		// Recomputed on every pass: a volume can leave the set while the call
		// waits, and the wait must then fail rather than go on for a volume that
		// is gone.
		declared := make([][]string, len(volumeSets))
		for i, set := range volumeSets {
			declared[i] = m.usableLocked(runID, set)
			if len(declared[i]) == 0 {
				m.mu.Unlock()
				return nil, nil, fmt.Errorf("reserve volumes: this agent declares none of %q", set)
			}
		}
		chosen, ok, abandoned := m.assignLocked(runID, declared, keys)
		if ok {
			var added []string
			for _, n := range chosen {
				if _, held := m.holders[n][runID]; held {
					continue
				}
				if m.holders[n] == nil {
					m.holders[n] = make(map[string]struct{})
				}
				m.holders[n][runID] = struct{}{}
				added = append(added, n)
			}
			for _, k := range keys {
				m.paths[k] = struct{}{}
			}
			m.mu.Unlock()
			unmark := m.markHeld(runID, added)
			release := m.releaser(func() {
				for _, n := range added {
					m.dropHolder(n, runID)
				}
				for _, k := range keys {
					if _, pinned := m.pinned[k]; !pinned {
						delete(m.paths, k)
					}
				}
			})
			return chosen, func() { unmark(); release() }, nil
		}
		wait := m.changed
		m.mu.Unlock()
		// Logged here, not in assignLocked: m.mu is released, so a log write
		// that blocks cannot stall the claims that share it. Once per call; the
		// search is re-run on every wake-up and would say the same each time.
		if abandoned && !warnedAbandoned {
			warnedAbandoned = true
			slog.Warn(VolumeSearchAbandonedMsg(len(volumeSets)), "runId", runID)
		}
		if onWait != nil {
			onWait()
			onWait = nil
		}
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, nil, fmt.Errorf("waiting for volumes %q and paths %q: %w", volumeSets, keys, ctx.Err())
		}
	}
}

// assignLocked decides whether the whole request can be satisfied right now and,
// if so, with which volumes. It changes nothing. m.mu must be held.
//
// abandoned is ChooseVolumes's: the search ran out of its budget. It is returned
// rather than logged here because the caller holds m.mu, and a log write that
// blocks (a stderr nobody is draining) must not stall every claim on the agent.
func (m *VolumeManager) assignLocked(runID string, declared [][]string, pathKeys []string) (chosen []string, ok, abandoned bool) {
	for _, k := range pathKeys {
		if _, held := m.paths[k]; held {
			return nil, false, false
		}
	}
	free := func(n string) bool {
		v := m.vols[n]
		if v == nil {
			return false
		}
		_, mine := m.holders[n][runID]
		return mine || (!v.switching && (v.mode == VolumeModeShared || len(m.holders[n]) == 0))
	}
	return ChooseVolumesAvoiding(declared, free, func(n string) bool {
		_, g := m.gated[n]
		return g
	})
}

// SetGated records which volumes the disk gate currently withholds (the same
// list the claim reports as busy). It only steers the choice among an entry's
// candidates, see ChooseVolumesAvoiding; it never makes a volume unusable.
// Safe on a nil manager.
func (m *VolumeManager) SetGated(names []string) {
	if m == nil {
		return
	}
	g := make(map[string]struct{}, len(names))
	for _, n := range names {
		g[n] = struct{}{}
	}
	m.mu.Lock()
	m.gated = g
	m.mu.Unlock()
}

// maxVolumeSearchNodes bounds one pass of the assignment search. The caller
// runs it under a lock that every claim on the agent shares, and the search is
// exponential when no distinct assignment exists (about x11 per extra entry), so
// an input that is merely unlucky must cost a bounded, small amount of time.
//
// 20,000 nodes is a few milliseconds at most per pass (three passes per call).
// A job that has a distinct assignment finds it in about one node per entry
// (each entry takes its first candidate that is free and not yet taken), and
// even one that needs to backtrack through a busy volume or two needs a few
// hundred, so no realistic job comes within two orders of magnitude of it.
const maxVolumeSearchNodes = 20_000

// VolumeSearchAbandonedMsg is what the host and Kubernetes agents log when
// ChooseVolumes reports that it abandoned its search (see there). entries is the
// number of volume entries in the job.
func VolumeSearchAbandonedMsg(entries int) string {
	return fmt.Sprintf("volume assignment search abandoned after %d nodes for a job with %d volume entries; the run will wait instead of starting. Reduce the number of volume entries, or make each entry's candidate list name enough distinct volumes",
		maxVolumeSearchNodes, entries)
}

// ChooseVolumes decides which volume each entry of declared (each entry lists
// its candidate names, all of which the agent declares) a run takes right now,
// given free, which says whether a volume may be taken. It reports false when
// the run has to wait. It changes nothing.
//
// The host agent and the Kubernetes agent both call it, with different notions
// of "free" (an in-process holder table, and the live pods that mount the
// volume), so a job's candidate list resolves the same way on either: entries
// get distinct volumes whenever the candidates allow it, a run waits rather than
// alias two entries onto one volume that could have been two, and one name
// written twice is the legal "one volume mounted at two paths" case.
//
// The search is bounded (maxVolumeSearchNodes per pass). abandoned reports that
// a pass hit the bound and the answer therefore rests on a search that was cut
// short: the run is told to wait, never given an aliased volume it could not
// prove was unavoidable, and the caller should say so, outside any lock, so an
// operator can see why a job is not starting. A crafted entry list is the
// usual cause (some subset of entries referencing too few volumes between
// them, which only an exhaustive search discovers); a wait is recoverable by
// fixing the job, an agent stalled inside this function is not.
func ChooseVolumes(declared [][]string, free func(string) bool) (chosen []string, ok, abandoned bool) {
	// One volume per entry, no volume twice, needs at least as many distinct
	// names as entries. With fewer, both distinct passes below are bound to
	// fail, but only after exhausting a search tree that grows by an order of
	// magnitude per extra entry; go straight to the aliasing reading, which is
	// the answer they would have led to.
	names := map[string]struct{}{}
	for _, set := range declared {
		for _, n := range set {
			names[n] = struct{}{}
		}
	}
	if len(declared) > len(names) {
		a, ab := assignDistinct(declared, free, true)
		return a, a != nil, ab
	}
	a, freeAbandoned := assignDistinct(declared, free, false)
	if a != nil {
		return a, true, false
	}
	// Distinct volumes are not free now. If they exist at all, wait for them
	// rather than alias.
	exists, ab := assignDistinct(declared, func(string) bool { return true }, false)
	if exists != nil {
		// freeAbandoned: the wait may be needless, a free distinct assignment
		// may exist that the cut-short search did not reach.
		return nil, false, freeAbandoned
	}
	if ab {
		// Whether distinct volumes exist is unknown, so aliasing would be a
		// guess; wait.
		return nil, false, true
	}
	// Distinct volumes cannot exist: aliasing is the only reading of the job.
	a, ab = assignDistinct(declared, free, true)
	return a, a != nil, ab
}

// ChooseVolumesAvoiding is ChooseVolumes that prefers volumes avoid does not
// name (the ones the disk gate withholds). The controller only hands out a run
// that has a non-gated candidate for every entry it could not otherwise serve,
// so an entry listing a gated and a healthy volume must land on the healthy
// one. If no assignment exists without the avoided volumes, it falls back to
// plain ChooseVolumes: the run was already claimed, and the gate steers, it
// does not fail runs. With nothing avoided it is exactly ChooseVolumes.
func ChooseVolumesAvoiding(declared [][]string, free, avoid func(string) bool) (chosen []string, ok, abandoned bool) {
	anyAvoided := false
	for _, set := range declared {
		for _, n := range set {
			if avoid(n) {
				anyAvoided = true
			}
		}
	}
	if !anyAvoided {
		return ChooseVolumes(declared, free)
	}
	chosen, ok, _ = ChooseVolumes(declared, func(n string) bool { return free(n) && !avoid(n) })
	if ok {
		return chosen, true, false
	}
	return ChooseVolumes(declared, free)
}

// assignDistinct searches for one volume per entry, trying each entry's
// candidates in the order listed, with no volume chosen twice. With alias, an
// entry whose every candidate is already chosen reuses its first. usable says
// which volumes may be chosen. It returns nil if there is no assignment, and
// abandoned if it gave up after maxVolumeSearchNodes without finding one or
// exhausting the search (the nil then means "not found", not "does not exist").
func assignDistinct(sets [][]string, usable func(string) bool, alias bool) (assignment []string, abandoned bool) {
	if !alias {
		// The same counting argument as in ChooseVolumes, over the volumes that
		// may be chosen: fewer of them than entries and no distinct assignment
		// exists. This is the common way for a pass to fail (a run that wants
		// twelve volumes while two are busy), and without it that failure costs
		// the whole search, which would be mistaken for a pathological input.
		judged, nUsable := map[string]struct{}{}, 0
		for _, set := range sets {
			for _, n := range set {
				if _, seen := judged[n]; !seen {
					judged[n] = struct{}{}
					if usable(n) {
						nUsable++
					}
				}
			}
		}
		if len(sets) > nUsable {
			return nil, false
		}
	}
	chosen := make([]string, len(sets))
	taken := map[string]bool{}
	nodes := 0
	var rec func(i int) bool
	rec = func(i int) bool {
		if nodes++; nodes > maxVolumeSearchNodes {
			abandoned = true
		}
		if abandoned {
			return false
		}
		if i == len(sets) {
			return true
		}
		anyUnchosen := false
		for _, n := range sets[i] {
			if taken[n] {
				continue
			}
			anyUnchosen = true
			if !usable(n) {
				continue
			}
			taken[n], chosen[i] = true, n
			if rec(i + 1) {
				return true
			}
			if abandoned {
				return false
			}
			delete(taken, n)
		}
		if !anyUnchosen && alias {
			chosen[i] = sets[i][0]
			return rec(i + 1)
		}
		return false
	}
	if !rec(0) {
		return nil, abandoned
	}
	return chosen, false
}

// ReservePath reserves an absolute host path that a native run symlinks a
// volume into. Unlike a relative mount inside the per-run workspace, such a
// path is machine-wide: two concurrent runs cannot point it at two different
// volumes, so it is held exclusively like an exclusive volume (spec section
// 4.6). It blocks while another holder has it, and the returned release is
// idempotent.
//
// The path is the DSL's POSIX mountPath, and it is keyed and reported (Busy) in
// that spelling on every OS: path.Clean(filepath.ToSlash(p)). filepath.Clean
// would turn /go/cache into \go\cache on Windows, which the controller could
// never match against the job's own string.
func (m *VolumeManager) ReservePath(hostPath string) (func(), error) {
	return m.ReservePathContext(context.Background(), hostPath)
}

// ReservePathContext is ReservePath with an abandonable wait.
func (m *VolumeManager) ReservePathContext(ctx context.Context, hostPath string) (func(), error) {
	return m.reservePath(ctx, hostPath, nil)
}

func (m *VolumeManager) reservePath(ctx context.Context, hostPath string, onWait func()) (func(), error) {
	if m == nil {
		return nil, fmt.Errorf("reserve path %q: this agent declares no volumes", hostPath)
	}
	slashed := filepath.ToSlash(hostPath)
	if !filepath.IsAbs(hostPath) && !strings.HasPrefix(slashed, "/") {
		return nil, fmt.Errorf("reserve path: %q is not absolute", hostPath)
	}
	key := path.Clean(slashed)

	for {
		m.mu.Lock()
		if _, held := m.paths[key]; !held {
			m.paths[key] = struct{}{}
			m.mu.Unlock()
			return m.releaser(func() {
				if _, pinned := m.pinned[key]; !pinned {
					delete(m.paths, key)
				}
			}), nil
		}
		wait := m.changed
		m.mu.Unlock()
		if onWait != nil {
			onWait()
			onWait = nil
		}
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for path %q: %w", key, ctx.Err())
		}
	}
}

// pinPath keeps the reservation of an absolute native mount path for the life
// of the process: the run that held it could not remove its link, and that link
// is machine-wide, so handing the path to another run (which may mount a
// different volume there) would silently repoint a link nobody is cleaning up.
// A relative link is different: it lives in a per-run workspace and the next
// mount over it replaces a link this agent owns, so nothing needs pinning.
// A restart clears it (the startup sweep retries the unlink from the registry).
// The path must be currently reserved by the caller.
func (m *VolumeManager) pinPath(hostPath string) {
	key := path.Clean(filepath.ToSlash(hostPath))
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pinned[key] = struct{}{}
}

// releaser wraps drop into a once-only release func that also wakes waiters.
// drop runs with m.mu held.
func (m *VolumeManager) releaser(drop func()) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			drop()
			close(m.changed)
			m.changed = make(chan struct{})
		})
	}
}

// dropHolder removes runID from the volume's holders. m.mu must be held.
func (m *VolumeManager) dropHolder(name, runID string) {
	delete(m.holders[name], runID)
	if len(m.holders[name]) == 0 {
		delete(m.holders, name)
	}
}

// Busy is the claim-time report: the exclusive volumes currently held plus the
// absolute native mount paths currently reserved, each list sorted, volumes
// first. Shared volumes are never busy. It is never nil, so it serialises as
// [] rather than null.
//
// The report is an optimisation that stops the agent claiming a run it would
// then wait on; correctness lives in Reserve.
func (m *VolumeManager) Busy() []string {
	if m == nil {
		return []string{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	busy := []string{}
	for n, hs := range m.holders {
		// A held volume always has an entry (a retiring one is kept until its
		// last holder releases); the nil check is defence, not an expected case.
		if v := m.vols[n]; len(hs) > 0 && v != nil && v.mode == VolumeModeExclusive {
			busy = append(busy, n)
		}
	}
	sort.Strings(busy)
	paths := make([]string, 0, len(m.paths))
	for p := range m.paths {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return append(busy, paths...)
}
