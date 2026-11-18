package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

// volumeReclaimFile records, per served volume, the reclaim policy the
// controller last delivered, and for a Delete volume that left the set when it
// left. It lives in the workspace directory under a dot-prefixed name, which
// checkOutOfGCReach already keeps volumes away from.
const volumeReclaimFile = ".ucd-volume-reclaim.json"

// reclaimGrace is how long a Delete volume must stay continuously absent from
// every delivered set before its data is reclaimed. A volume that comes back
// within it (an AgentVolume moved between AppSources, deleted and re-applied, a
// controller database restored, a label edit reverted) keeps its data. A
// variable so tests can shorten it.
var reclaimGrace = 10 * time.Minute

// reclaimTrashPrefix starts the name a reclaimed volume directory is renamed to
// under <base>/volumes before its background removal. A volume name is a DNS
// label and never starts with a dot, so the name cannot collide with a volume.
const reclaimTrashPrefix = ".reclaim-"

type reclaimRecord struct {
	Volumes map[string]string `json:"volumes"`
	// LeftSetAt maps a Delete volume that is no longer delivered to when it
	// was first seen absent (RFC 3339).
	LeftSetAt map[string]string `json:"leftSetAt,omitempty"`
}

func (m *VolumeManager) reclaimFilePath() string {
	return filepath.Join(m.base, volumeReclaimFile)
}

// loadReclaimRecord reads the record a previous process wrote. A missing file
// is an empty record; an unreadable or corrupt one is too, with a warning:
// the safe failure is to delete nothing, and the next Apply rewrites the file.
// An unparseable leftSetAt is dropped, which restarts that volume's grace.
func (m *VolumeManager) loadReclaimRecord() (map[string]string, map[string]time.Time) {
	out := make(map[string]string)
	left := make(map[string]time.Time)
	b, err := os.ReadFile(m.reclaimFilePath())
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("could not read the volume reclaim record; volumes that left the set before this start will not be reclaimed", "path", m.reclaimFilePath(), "error", err)
		}
		return out, left
	}
	var rec reclaimRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		slog.Warn("the volume reclaim record is corrupt and was ignored; volumes that left the set before this start will not be reclaimed", "path", m.reclaimFilePath(), "error", err)
		return out, left
	}
	for n, p := range rec.Volumes {
		out[n] = p
	}
	for n, at := range rec.LeftSetAt {
		if _, ok := out[n]; !ok {
			continue
		}
		t, perr := time.Parse(time.RFC3339, at)
		if perr != nil {
			continue
		}
		left[n] = t
	}
	return out, left
}

// saveReclaimRecord writes the record atomically (temp file, then rename). A
// failure is logged, not returned: a stale record can only make the agent
// reclaim less (a Delete volume it forgot) or retry a reclaim, never delete
// something it should not. Called with applyMu held.
func (m *VolumeManager) saveReclaimRecord() {
	rec := reclaimRecord{Volumes: m.reclaim}
	if len(m.leftSetAt) > 0 {
		rec.LeftSetAt = make(map[string]string, len(m.leftSetAt))
		for n, t := range m.leftSetAt {
			rec.LeftSetAt[n] = t.UTC().Format(time.RFC3339)
		}
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err == nil {
		var tmp *os.File
		tmp, err = os.CreateTemp(m.base, volumeReclaimFile+".tmp-*")
		if err == nil {
			_, werr := tmp.Write(b)
			cerr := tmp.Close()
			if err = errors.Join(werr, cerr); err == nil {
				err = os.Rename(tmp.Name(), m.reclaimFilePath())
			}
			if err != nil {
				_ = os.Remove(tmp.Name())
			}
		}
	}
	if err != nil {
		slog.Warn("could not write the volume reclaim record", "path", m.reclaimFilePath(), "error", err)
	}
}

// validDesiredVolume checks one desired volume and returns its advisory size
// in bytes and normalised reclaim policy.
func (m *VolumeManager) validDesiredVolume(d api.DesiredVolume) (size int64, policy string, err error) {
	if err := validateVolumeConfigName(d.Name); err != nil {
		return 0, "", err
	}
	if strings.HasPrefix(d.Name, ".") {
		return 0, "", fmt.Errorf("volume name %q must not start with a dot", d.Name)
	}
	switch d.Mode {
	case VolumeModeExclusive, VolumeModeShared:
	default:
		return 0, "", fmt.Errorf("mode must be %q or %q, got %q", VolumeModeExclusive, VolumeModeShared, d.Mode)
	}
	if d.Size != "" {
		q, perr := resource.ParseQuantity(d.Size)
		if perr != nil || q.Value() <= 0 {
			return 0, "", fmt.Errorf("size %q is not a positive quantity such as 200Gi", d.Size)
		}
		size = q.Value()
	}
	policy, err = parseReclaimPolicy(d.ReclaimPolicy)
	if err != nil {
		return 0, "", err
	}
	p := m.defaultVolumePath(d.Name)
	if err := checkOutOfGCReach(m.base, p); err != nil {
		return 0, "", err
	}
	return size, policy, nil
}

// parseReclaimPolicy normalises a delivered reclaim policy ("" is Retain).
func parseReclaimPolicy(p string) (string, error) {
	switch p {
	case "", dsl.ReclaimRetain:
		return dsl.ReclaimRetain, nil
	case dsl.ReclaimDelete:
		return dsl.ReclaimDelete, nil
	default:
		return "", fmt.Errorf("reclaimPolicy must be %q or %q, got %q", dsl.ReclaimRetain, dsl.ReclaimDelete, p)
	}
}

func (m *VolumeManager) defaultVolumePath(name string) string {
	return filepath.Join(m.base, "volumes", name)
}

// Apply makes the live volume set match desired and returns one status per
// desired name, plus one for any volume whose reclaim failed (it is no longer
// desired, but the operator needs to see why its data is still there), sorted
// by name.
//
// It is idempotent and meant to run on every successful heartbeat: a removal
// or mode switch that had to wait for holders completes at the first Apply
// after the last one released, so no release callback is needed.
//
//   - A new volume is validated and its directory created (existing contents
//     are kept); one that fails is reported not ready and does not block the
//     rest.
//   - A volume that left the set stops being offered at once. While a run holds
//     it, it stays as a retiring entry that only its holders can use; when the
//     last holder is gone it is forgotten. If its recorded reclaim policy is
//     Delete, its directory is removed (see reclaimVolume) once it has also
//     been absent from every delivered set for reclaimGrace; reappearing in a
//     set resets that clock.
//   - The reclaim policy of a known volume is updated whenever it is delivered
//     with a valid policy, even if the volume cannot be applied right now, so
//     a Delete-to-Retain change is never lost.
//   - A mode change is applied only when no run holds the volume; until then
//     the volume is not offered.
//   - A size change applies immediately.
func (m *VolumeManager) Apply(desired []api.DesiredVolume) []api.AgentVolumeStatus {
	if m == nil {
		return nil
	}
	m.applyMu.Lock()
	defer m.applyMu.Unlock()

	statuses := make(map[string]api.AgentVolumeStatus, len(desired))
	// mentioned is every name the controller delivered, valid or not. None of
	// them is ever reclaimed: a volume that is still declared is not deleted
	// because this agent could not apply it right now.
	mentioned := make(map[string]struct{}, len(desired))
	keep := make(map[string]struct{}, len(desired))
	changed := false

	for _, d := range desired {
		if _, dup := mentioned[d.Name]; dup {
			statuses[d.Name] = api.AgentVolumeStatus{Name: d.Name, Reason: "invalid: volume delivered more than once"}
			continue
		}
		mentioned[d.Name] = struct{}{}
		// Delivered again: the absence clock restarts from zero.
		if _, ok := m.leftSetAt[d.Name]; ok {
			delete(m.leftSetAt, d.Name)
			changed = true
		}
		_, known := m.reclaim[d.Name]
		size, policy, err := m.validDesiredVolume(d)
		if err != nil {
			statuses[d.Name] = api.AgentVolumeStatus{Name: d.Name, Reason: fmt.Sprintf("invalid: %v", err)}
			// The volume cannot be applied, but a policy change for one this
			// agent already served is still recorded.
			if p, perr := parseReclaimPolicy(d.ReclaimPolicy); perr == nil && known && m.reclaim[d.Name] != p {
				m.reclaim[d.Name] = p
				changed = true
			}
			continue
		}
		st, ok := m.applyOne(d, size)
		statuses[d.Name] = st
		if ok {
			keep[d.Name] = struct{}{}
		}
		if (ok || known) && m.reclaim[d.Name] != policy {
			m.reclaim[d.Name] = policy
			changed = true
		}
	}

	// Everything live that is not kept leaves the set.
	var reclaimNow []string
	m.mu.Lock()
	for name, v := range m.vols {
		if _, ok := keep[name]; ok {
			continue
		}
		if len(m.holders[name]) > 0 {
			v.retiring = true
			continue
		}
		delete(m.vols, name)
	}
	m.mu.Unlock()

	// Names not delivered. Retain forgets the record once no run holds the
	// volume. Delete starts the absence clock the first time the name is
	// missing (even while held) and reclaims once the clock reaches
	// reclaimGrace and no run holds it. A name that was delivered but failed
	// to apply is never reclaimed.
	var failed []api.AgentVolumeStatus

	now := m.now()
	for name, policy := range m.reclaim {
		if _, ok := mentioned[name]; ok {
			continue
		}
		m.mu.Lock()
		_, live := m.vols[name]
		m.mu.Unlock()
		if policy != dsl.ReclaimDelete {
			if !live {
				delete(m.reclaim, name)
				delete(m.leftSetAt, name)
				changed = true
			}
			continue
		}
		left, ok := m.leftSetAt[name]
		if !ok || left.After(now) {
			// First seen absent (or a recorded time in the future, from a clock
			// change): start the clock now, which can only delay a reclaim.
			left = now
			m.leftSetAt[name] = now
			changed = true
			if !ok {
				slog.Info(fmt.Sprintf("volume left the set; reclaim scheduled after %s", reclaimGrace), "volume", name)
			}
		}
		if live || now.Sub(left) < reclaimGrace {
			continue
		}
		reclaimNow = append(reclaimNow, name)
	}
	// Nothing can start using these names while they are reclaimed: they have
	// no live entry, and only Apply (serialised by applyMu) adds one. The data
	// is only moved aside here; the slow delete runs in the background.
	sort.Strings(reclaimNow)
	for _, name := range reclaimNow {
		if err := m.reclaimVolume(name); err != nil {
			slog.Warn("volume reclaim failed; it will be retried on the next heartbeat", "volume", name, "error", err)
			failed = append(failed, api.AgentVolumeStatus{Name: name, Reason: fmt.Sprintf("reclaim: %v", err)})
			continue
		}
		slog.Info("reclaimed volume", "volume", name)
		delete(m.reclaim, name)
		delete(m.leftSetAt, name)
		changed = true
	}
	if changed {
		m.saveReclaimRecord()
	}

	out := make([]api.AgentVolumeStatus, 0, len(statuses)+len(failed))
	for _, st := range statuses {
		out = append(out, st)
	}
	out = append(out, failed...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// applyOne brings one valid desired volume in line: it creates, updates, or
// (while holders remain) holds back the entry. ok is true when an entry exists
// that Apply must keep (even a not-ready one waiting for a mode switch).
func (m *VolumeManager) applyOne(d api.DesiredVolume, size int64) (api.AgentVolumeStatus, bool) {
	m.mu.Lock()
	v, exists := m.vols[d.Name]
	if exists {
		defer m.mu.Unlock()
		v.retiring = false
		v.size = size
		if v.mode == d.Mode {
			v.switching = false
			return api.AgentVolumeStatus{Name: d.Name, Ready: true}, true
		}
		if n := len(m.holders[d.Name]); n > 0 {
			v.switching = true
			return api.AgentVolumeStatus{Name: d.Name, Reason: fmt.Sprintf("waiting for %d run(s) to release before switching to %s", n, d.Mode)}, true
		}
		v.mode, v.switching = d.Mode, false
		return api.AgentVolumeStatus{Name: d.Name, Ready: true}, true
	}
	m.mu.Unlock()

	// A new volume. Directory work happens outside m.mu.
	p := m.defaultVolumePath(d.Name)
	for _, other := range m.livePaths() {
		if pathWithinFold(other, p) || pathWithinFold(p, other) {
			return api.AgentVolumeStatus{Name: d.Name, Reason: fmt.Sprintf("invalid: path %s overlaps another volume at %s; two volumes must not share data", p, other)}, false
		}
	}
	if err := os.MkdirAll(p, 0o755); err != nil {
		return api.AgentVolumeStatus{Name: d.Name, Reason: fmt.Sprintf("create directory: %v", err)}, false
	}
	// The lexical check cannot see a symlink that leads into a working<N>
	// directory; check the resolved location too.
	rp, rerr := filepath.EvalSymlinks(p)
	rb, berr := filepath.EvalSymlinks(m.base)
	if rerr == nil && berr == nil {
		if err := checkOutOfGCReach(rb, rp); err != nil {
			return api.AgentVolumeStatus{Name: d.Name, Reason: fmt.Sprintf("invalid: %s resolves to %s: %v", p, rp, err)}, false
		}
	}
	m.mu.Lock()
	m.vols[d.Name] = &volume{mode: d.Mode, path: p, size: size}
	m.mu.Unlock()
	return api.AgentVolumeStatus{Name: d.Name, Ready: true}, true
}

func (m *VolumeManager) livePaths() []string { return m.Paths() }

// reclaimVolume deletes the data of a volume that left the set. It is the only
// place the agent deletes volume data, so it is deliberately narrow: it takes
// exactly <base>/volumes/<name> for a single-segment name, and refuses (rather
// than deletes) anything that is not a plain directory, so a symlink planted
// at that path can never lead the delete somewhere else.
//
// The directory is renamed to <base>/volumes/.reclaim-<name>-<unixnano> and
// removed in the background, so a large volume never holds up Apply (and with
// it the heartbeat). The name is free again as soon as this returns. A rename
// that fails is returned and retried on the next Apply.
func (m *VolumeManager) reclaimVolume(name string) error {
	if err := validateVolumeConfigName(name); err != nil {
		return fmt.Errorf("refusing to delete: %w", err)
	}
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("refusing to delete: volume name %q starts with a dot", name)
	}
	p := m.defaultVolumePath(name)
	fi, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", p, err)
	}
	if !fi.IsDir() { // a symlink or junction reports as not a directory under Lstat
		return fmt.Errorf("%s is not a plain directory (a link or a file); not deleting it", p)
	}
	trash := filepath.Join(m.volumesDir(), fmt.Sprintf("%s%s-%d", reclaimTrashPrefix, name, m.now().UnixNano()))
	if err := m.rename(p, trash); err != nil {
		return fmt.Errorf("move %s aside for deletion: %w", p, err)
	}
	m.scheduleTrash(trash)
	return nil
}

func (m *VolumeManager) volumesDir() string { return filepath.Join(m.base, "volumes") }

// isTrashPath reports whether p is a moved-aside reclaimed directory: a direct
// child of <base>/volumes whose name starts with reclaimTrashPrefix. The
// background deleter removes nothing else.
func (m *VolumeManager) isTrashPath(p string) bool {
	return filepath.Dir(p) == m.volumesDir() && strings.HasPrefix(filepath.Base(p), reclaimTrashPrefix)
}

// scheduleLeftoverTrash queues the moved-aside directories a previous process
// did not finish deleting. Only plain directories are taken; anything else
// with the prefix is left alone.
func (m *VolumeManager) scheduleLeftoverTrash() {
	entries, err := os.ReadDir(m.volumesDir())
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("could not list the volumes directory for leftover reclaimed data", "path", m.volumesDir(), "error", err)
		}
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), reclaimTrashPrefix) {
			continue
		}
		p := filepath.Join(m.volumesDir(), e.Name())
		if fi, err := os.Lstat(p); err != nil || !fi.IsDir() {
			continue
		}
		m.scheduleTrash(p)
	}
}

// scheduleTrash queues p for background removal. A single goroutine drains the
// queue, so removals never run in parallel with each other, and nothing else
// touches these paths: they are not volumes, links, or sampled storage paths.
func (m *VolumeManager) scheduleTrash(p string) {
	m.trashMu.Lock()
	m.trash = append(m.trash, p)
	start := !m.trashBusy
	if start {
		m.trashBusy = true
		m.trashWG.Add(1)
	}
	m.trashMu.Unlock()
	if start {
		go m.drainTrash()
	}
}

func (m *VolumeManager) drainTrash() {
	defer m.trashWG.Done()
	for {
		m.trashMu.Lock()
		if len(m.trash) == 0 {
			m.trashBusy = false
			m.trashMu.Unlock()
			return
		}
		p := m.trash[0]
		m.trash = m.trash[1:]
		m.trashMu.Unlock()

		if !m.isTrashPath(p) {
			slog.Error("refusing to delete a path that is not reclaimed volume data", "path", p)
			continue
		}
		if err := m.removeAll(p); err != nil {
			slog.Warn("could not delete reclaimed volume data; it is retried at the next agent start", "path", p, "error", err)
			continue
		}
		slog.Info("deleted reclaimed volume data", "path", p)
	}
}

// waitTrash blocks until the background deleter is idle. For tests.
func (m *VolumeManager) waitTrash() { m.trashWG.Wait() }
