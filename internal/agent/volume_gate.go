package agent

import (
	"log/slog"
	"sync"

	"github.com/eirueimi/unified-cd/internal/config"
)

// volumeVerdict is the gate's view of one declared volume.
type volumeVerdict struct {
	name        string
	free, total uint64
	threshold   uint64 // the threshold resolved against total
	// gated: free space is below the threshold, so runs that need the volume
	// must not be claimed.
	gated bool
	// unsatisfiable: the threshold is not smaller than the volume itself, so it
	// could never be met; the volume is left ungated (design D3).
	unsatisfiable bool
}

// evaluateVolumes judges every declared volume against th using only what the
// background sampler last published; it never measures anything itself (a
// statfs on a dead mount cannot be interrupted, see storageSampler and
// rulings #8). A volume with no fresh reading is omitted, which fails open: it
// is not gated.
func evaluateVolumes(vm *VolumeManager, sampler *storageSampler, th config.DiskThreshold) []volumeVerdict {
	if sampler == nil {
		return nil
	}
	var out []volumeVerdict
	for _, name := range vm.Names() {
		free, total, _, ok := sampler.read(vm.PathOf(name))
		if !ok {
			continue
		}
		v := volumeVerdict{name: name, free: free, total: total, threshold: th.Resolve(total)}
		if v.threshold == 0 { // off, unset, or a percentage of nothing
			continue
		}
		if v.threshold >= total {
			v.unsatisfiable = true
		} else {
			v.gated = th.Below(free, total)
		}
		out = append(out, v)
	}
	return out
}

// gatedVolumes returns the names (sorted) of the declared volumes whose fresh
// reading is below th, and separately those the threshold cannot apply to
// because it is not smaller than the volume. It is pure: logging lives in
// volumeGate.
func gatedVolumes(vm *VolumeManager, sampler *storageSampler, th config.DiskThreshold) (gated, unsatisfiable []string) {
	for _, v := range evaluateVolumes(vm, sampler, th) {
		switch {
		case v.gated:
			gated = append(gated, v.name)
		case v.unsatisfiable:
			unsatisfiable = append(unsatisfiable, v.name)
		}
	}
	return gated, unsatisfiable
}

// volumeGate remembers, across claim-loop iterations and slots, which volumes
// are currently gated and which have already had their "threshold not smaller
// than the volume" warning, so each transition is logged once instead of every
// two seconds. The zero value is ready to use.
type volumeGate struct {
	mu     sync.Mutex
	gated  map[string]bool
	warned map[string]bool
}

// names returns the volumes to withhold from this claim (the same list as
// gatedVolumes) and logs the changes since the previous call: a volume entering
// the gated state, leaving it (recovered, or its reading went missing, which
// fails open), and, once per volume per process, a threshold that cannot apply.
func (g *volumeGate) names(vm *VolumeManager, sampler *storageSampler, th config.DiskThreshold) []string {
	verdicts := evaluateVolumes(vm, sampler, th)

	g.mu.Lock()
	if g.gated == nil {
		g.gated = make(map[string]bool)
		g.warned = make(map[string]bool)
	}
	now := make(map[string]bool, len(verdicts))
	var names []string
	var entered, unsat []volumeVerdict
	for _, v := range verdicts {
		switch {
		case v.gated:
			now[v.name] = true
			names = append(names, v.name)
			if !g.gated[v.name] {
				entered = append(entered, v)
			}
		case v.unsatisfiable:
			if !g.warned[v.name] {
				g.warned[v.name] = true
				unsat = append(unsat, v)
			}
		}
	}
	var left []string
	for name := range g.gated {
		if !now[name] {
			left = append(left, name)
		}
	}
	g.gated = now
	g.mu.Unlock()

	// Log after the unlock: a stderr that applies backpressure must not hold
	// the mutex every claim slot takes.
	for _, v := range unsat {
		slog.Warn("minFreeDisk is not smaller than the volume, so the volume is not gated",
			"volume", v.name, "thresholdBytes", v.threshold, "totalBytes", v.total)
	}
	for _, v := range entered {
		slog.Warn("volume below minFreeDisk, not claiming runs that mount it",
			"volume", v.name, "freeBytes", v.free, "totalBytes", v.total, "thresholdBytes", v.threshold)
	}
	for _, name := range left {
		slog.Info("volume no longer below minFreeDisk (or no longer measured), claiming runs that mount it again", "volume", name)
	}
	return names
}

// withGated appends the gated volume names to busy, skipping any busy already
// lists. busy keeps its order (held exclusive volumes, then reserved paths, as
// VolumeManager.Busy returns them) and the gated names follow in the order
// given (name order), so the list is deterministic.
func withGated(busy, gated []string) []string {
	if len(gated) == 0 {
		return busy
	}
	have := make(map[string]bool, len(busy))
	for _, b := range busy {
		have[b] = true
	}
	for _, g := range gated {
		if !have[g] {
			busy = append(busy, g)
			have[g] = true
		}
	}
	return busy
}
