package k8sagent

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/config"
)

// volumeGateMaxAge is how old a volume reading may be and still gate claims.
// A volume is only measured by a run that mounts it, so a gated volume is never
// re-measured: without this bound the gate could not lift even after the
// operator freed space. A reading older than this is treated as unknown, which
// lets one probe run through; its pre-run measurement re-arms the gate if the
// volume is still low (design D6). Constant, not configurable in v1.
const volumeGateMaxAge = 10 * time.Minute

// volumeVerdict is the gate's view of one volume reading.
type volumeVerdict struct {
	free, total, threshold uint64
	// gated: a fresh reading is below the threshold.
	gated bool
	// unsatisfiable: the threshold is not smaller than the volume itself, so
	// it could never be met; the volume is left ungated.
	unsatisfiable bool
}

// judgeVolume decides whether stats gate claims under th at time now. It fails
// open: a never-measured or stale reading, an off threshold and an
// unsatisfiable one all leave the volume ungated.
func judgeVolume(stats api.VolumeStats, th config.DiskThreshold, now time.Time) volumeVerdict {
	if stats.MeasuredAt.IsZero() || stats.TotalBytes == 0 || now.Sub(stats.MeasuredAt) > volumeGateMaxAge {
		return volumeVerdict{}
	}
	v := volumeVerdict{free: stats.FreeBytes, total: stats.TotalBytes, threshold: th.Resolve(stats.TotalBytes)}
	if v.threshold == 0 { // off, unset, or a percentage of nothing
		return v
	}
	if v.threshold >= v.total {
		v.unsatisfiable = true
		return v
	}
	v.gated = th.Below(stats.FreeBytes, stats.TotalBytes)
	return v
}

// volumeGated reports whether stats keep a volume out of the agent's claims.
func volumeGated(stats api.VolumeStats, th config.DiskThreshold, now time.Time) bool {
	return judgeVolume(stats, th, now).gated
}

// k8sVolumeGate remembers which volumes are gated, and which have had their
// "threshold not smaller than the volume" warning, so each transition is logged
// once rather than on every claim poll. The zero value is ready to use.
type k8sVolumeGate struct {
	mu     sync.Mutex
	gated  map[string]bool
	warned map[string]bool
}

// names returns the declared volumes to withhold from a claim, in name order,
// and logs the changes since the previous call.
func (g *k8sVolumeGate) names(declared []config.VolumeConfig, readings map[string]api.VolumeStats, th config.DiskThreshold, now time.Time) []string {
	byName := make([]string, 0, len(declared))
	for _, c := range declared {
		byName = append(byName, c.Name)
	}
	sort.Strings(byName)

	current := make(map[string]bool)
	var names []string
	var entered, unsat []volumeVerdictFor
	g.mu.Lock()
	if g.gated == nil {
		g.gated = make(map[string]bool)
		g.warned = make(map[string]bool)
	}
	for _, name := range byName {
		stats, ok := readings[name]
		if !ok || current[name] {
			continue
		}
		v := judgeVolume(stats, th, now)
		switch {
		case v.gated:
			current[name] = true
			names = append(names, name)
			if !g.gated[name] {
				entered = append(entered, volumeVerdictFor{name, v})
			}
		case v.unsatisfiable && !g.warned[name]:
			g.warned[name] = true
			unsat = append(unsat, volumeVerdictFor{name, v})
		}
	}
	var left []string
	for name := range g.gated {
		if !current[name] {
			left = append(left, name)
		}
	}
	sort.Strings(left)
	g.gated = current
	g.mu.Unlock()

	// Log after the unlock: a blocked stderr must not hold the mutex.
	for _, e := range unsat {
		slog.Warn("minFreeDisk is not smaller than the volume, so the volume is not gated",
			"volume", e.name, "thresholdBytes", e.threshold, "totalBytes", e.total)
	}
	for _, e := range entered {
		slog.Warn("volume below minFreeDisk, not claiming runs that mount it",
			"volume", e.name, "freeBytes", e.free, "totalBytes", e.total, "thresholdBytes", e.threshold)
	}
	for _, name := range left {
		slog.Info("volume no longer below minFreeDisk (or its reading is too old to trust), claiming runs that mount it again", "volume", name)
	}
	return names
}

type volumeVerdictFor struct {
	name string
	volumeVerdict
}

// gatedNames lists the volumes the claim must withhold, logging transitions.
func (a *K8sAgent) gatedNames(now time.Time) []string {
	return a.volGate.names(a.cfg.Volumes, a.volumeStats.snapshot(), a.cfg.EffectiveMinFreeDisk(), now)
}

// gatedSet is the set of declared volumes whose cached reading gates claims at
// now (the same judgement as gatedNames, without its logging). It only reads
// the cache.
func (a *K8sAgent) gatedSet(now time.Time) map[string]bool {
	th := a.cfg.EffectiveMinFreeDisk()
	set := map[string]bool{}
	for name, stats := range a.volumeStats.snapshot() {
		if volumeGated(stats, th, now) {
			set[name] = true
		}
	}
	return set
}

// withGated appends gated to busy, skipping names busy already lists.
func withGated(busy, gated []string) []string {
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

// warnLowVolumes tells a run, in its own log, about each of its volumes whose
// reading (taken just before the run) is below minFreeDisk. The gate lets a
// probe run through a full volume once its reading is stale; this is where the
// job author hears about it.
func (a *K8sAgent) warnLowVolumes(ctx context.Context, runID string, vols []PodVolume) {
	th := a.cfg.EffectiveMinFreeDisk()
	stats := a.volumeStats.snapshot()
	now := time.Now().UTC()
	seen := map[string]bool{}
	for _, v := range vols {
		s, ok := stats[v.Name]
		if !ok || seen[v.Name] || !volumeGated(s, th, now) {
			continue
		}
		seen[v.Name] = true
		a.warnRun(ctx, runID, fmt.Sprintf(
			"volume %s has %s free (%.1f%%), below minFreeDisk %s; this run may fail for lack of space",
			v.Name, formatBytesIEC(s.FreeBytes), float64(s.FreeBytes)*100/float64(s.TotalBytes), th))
	}
}

// formatBytesIEC renders n with one decimal and a binary unit ("1.2GiB").
func formatBytesIEC(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	f := float64(n)
	for _, u := range []string{"KiB", "MiB", "GiB", "TiB", "PiB"} {
		f /= unit
		if f < unit || u == "PiB" {
			return fmt.Sprintf("%.1f%s", f, u)
		}
	}
	return ""
}
