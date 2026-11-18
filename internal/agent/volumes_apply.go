package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

// volumeReclaimFile records, per served volume, the reclaim policy the
// controller last delivered. It lives in the workspace directory under a
// dot-prefixed name, which checkOutOfGCReach already keeps volumes away from.
const volumeReclaimFile = ".ucd-volume-reclaim.json"

type reclaimRecord struct {
	Volumes map[string]string `json:"volumes"`
}

func (m *VolumeManager) reclaimFilePath() string {
	return filepath.Join(m.base, volumeReclaimFile)
}

// loadReclaimRecord reads the record a previous process wrote. A missing file
// is an empty record; an unreadable or corrupt one is too, with a warning:
// the safe failure is to delete nothing, and the next Apply rewrites the file.
func (m *VolumeManager) loadReclaimRecord() map[string]string {
	out := make(map[string]string)
	b, err := os.ReadFile(m.reclaimFilePath())
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("could not read the volume reclaim record; volumes that left the set before this start will not be reclaimed", "path", m.reclaimFilePath(), "error", err)
		}
		return out
	}
	var rec reclaimRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		slog.Warn("the volume reclaim record is corrupt and was ignored; volumes that left the set before this start will not be reclaimed", "path", m.reclaimFilePath(), "error", err)
		return out
	}
	for n, p := range rec.Volumes {
		out[n] = p
	}
	return out
}

// saveReclaimRecord writes the record atomically (temp file, then rename). A
// failure is logged, not returned: a stale record can only make the agent
// reclaim less (a Delete volume it forgot) or retry a reclaim, never delete
// something it should not. Called with applyMu held.
func (m *VolumeManager) saveReclaimRecord() {
	b, err := json.MarshalIndent(reclaimRecord{Volumes: m.reclaim}, "", "  ")
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
	switch d.ReclaimPolicy {
	case "", dsl.ReclaimRetain:
		policy = dsl.ReclaimRetain
	case dsl.ReclaimDelete:
		policy = dsl.ReclaimDelete
	default:
		return 0, "", fmt.Errorf("reclaimPolicy must be %q or %q, got %q", dsl.ReclaimRetain, dsl.ReclaimDelete, d.ReclaimPolicy)
	}
	p := m.defaultVolumePath(d.Name)
	if err := checkOutOfGCReach(m.base, p); err != nil {
		return 0, "", err
	}
	return size, policy, nil
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
//     last holder is gone it is forgotten and, if its recorded reclaim policy
//     is Delete, its directory is removed (see reclaimVolume).
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
		size, policy, err := m.validDesiredVolume(d)
		if err != nil {
			statuses[d.Name] = api.AgentVolumeStatus{Name: d.Name, Reason: fmt.Sprintf("invalid: %v", err)}
			continue
		}
		if st, ok := m.applyOne(d, size); ok {
			keep[d.Name] = struct{}{}
			statuses[d.Name] = st
			if m.reclaim[d.Name] != policy {
				m.reclaim[d.Name] = policy
				changed = true
			}
		} else {
			statuses[d.Name] = st
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

	// Names with no live entry and not delivered: their last holder is gone
	// (or the process restarted). Retain forgets the record, Delete reclaims.
	// A name that was delivered but failed to apply keeps its record untouched.
	var failed []api.AgentVolumeStatus

	for name, policy := range m.reclaim {
		if _, ok := mentioned[name]; ok {
			continue
		}
		m.mu.Lock()
		_, live := m.vols[name]
		m.mu.Unlock()
		if live {
			continue
		}
		if policy == dsl.ReclaimDelete {
			reclaimNow = append(reclaimNow, name)
			continue
		}
		delete(m.reclaim, name)
		changed = true
	}
	// RemoveAll runs outside m.mu: it can take long on a large cache, and
	// claims share that lock. Nothing can start using these names meanwhile:
	// they have no live entry, and only Apply (serialised by applyMu) adds one.
	sort.Strings(reclaimNow)
	for _, name := range reclaimNow {
		if err := m.reclaimVolume(name); err != nil {
			slog.Warn("volume reclaim failed; it will be retried on the next heartbeat", "volume", name, "error", err)
			failed = append(failed, api.AgentVolumeStatus{Name: name, Reason: fmt.Sprintf("reclaim: %v", err)})
			continue
		}
		slog.Info("reclaimed volume", "volume", name)
		delete(m.reclaim, name)
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
// place the agent deletes volume data, so it is deliberately narrow: it removes
// exactly <base>/volumes/<name> for a single-segment name, and refuses (rather
// than deletes) anything that is not a plain directory, so a symlink planted
// at that path can never lead the delete somewhere else.
func (m *VolumeManager) reclaimVolume(name string) error {
	if err := validateVolumeConfigName(name); err != nil {
		return fmt.Errorf("refusing to delete: %w", err)
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
	if err := m.removeAll(p); err != nil {
		return fmt.Errorf("delete %s: %w", p, err)
	}
	return nil
}
