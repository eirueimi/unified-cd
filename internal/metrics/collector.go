package metrics

import (
	"context"
	"log/slog"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/prometheus/client_golang/prometheus"
)

// Counts is the narrow store surface the scrape-time collector needs.
// *store.Postgres implements it.
type Counts interface {
	CountRunsByStatus(ctx context.Context) (map[api.RunStatus]int, error)
	CountAgentsByLiveness(ctx context.Context, staleAfter time.Duration) (alive, stale int, err error)
	// ListAgentStorage returns each live agent's last disk-headroom reading,
	// keyed by agent ID. Agents that never reported are absent.
	ListAgentStorage(ctx context.Context, staleAfter time.Duration) (map[string]api.AgentStorage, error)
}

// collectTimeout bounds the DB work done per scrape.
const collectTimeout = 3 * time.Second

var (
	runsCurrentDesc = prometheus.NewDesc("unifiedcd_runs_current",
		"Current number of non-terminal runs, by status.", []string{"status"}, nil)
	agentsDesc = prometheus.NewDesc("unifiedcd_agents",
		"Registered agents, by heartbeat liveness.", []string{"state"}, nil)

	// Agent disk headroom, read from the database at scrape time so a deleted
	// or reaped agent's series disappear instead of freezing at their last
	// value. Each series exists only when the agent reported it: an absent
	// series is honest, a zero would claim "plenty of space" or "not blocked".
	workspaceFreeDesc = prometheus.NewDesc("unifiedcd_agent_workspace_filesystem_free_bytes",
		"Free bytes on the agent's workspace filesystem, as last measured by the agent.",
		[]string{"agent"}, nil)
	workspaceSizeDesc = prometheus.NewDesc("unifiedcd_agent_workspace_filesystem_size_bytes",
		"Total bytes of the agent's workspace filesystem, as last measured by the agent.",
		[]string{"agent"}, nil)
	volumeFreeDesc = prometheus.NewDesc("unifiedcd_agent_volume_filesystem_free_bytes",
		"Free bytes on the filesystem backing a named volume, as last measured by the agent.",
		[]string{"agent", "agent_name", "volume"}, nil)
	volumeSizeDesc = prometheus.NewDesc("unifiedcd_agent_volume_filesystem_size_bytes",
		"Total bytes of the filesystem backing a named volume, as last measured by the agent.",
		[]string{"agent", "agent_name", "volume"}, nil)
	volumeMeasuredDesc = prometheus.NewDesc("unifiedcd_agent_volume_measured_timestamp_seconds",
		"Unix time at which the agent last measured a named volume's filesystem.",
		[]string{"agent", "agent_name", "volume"}, nil)
	minFreeDiskDesc = prometheus.NewDesc("unifiedcd_agent_min_free_disk_bytes",
		"The agent's configured minFreeDisk threshold in bytes; 0 means the claim-time disk check is disabled. Absent for an agent with no such setting.",
		[]string{"agent"}, nil)
	claimBlockedDesc = prometheus.NewDesc("unifiedcd_agent_claim_blocked_by_disk",
		"1 if the agent is currently declining to claim work because free disk is below its minFreeDisk threshold, else 0. Absent for an agent that performs no claim-time disk check.",
		[]string{"agent"}, nil)
)

type dbCollector struct {
	counts     Counts
	staleAfter time.Duration
	errors     prometheus.Counter
}

// RegisterDBCollector registers the scrape-time DB gauges on m's registry.
// staleAfter is the agent-liveness window (matches the stuck-run reaper).
func (m *Metrics) RegisterDBCollector(c Counts, staleAfter time.Duration) {
	m.reg.MustRegister(&dbCollector{counts: c, staleAfter: staleAfter, errors: m.collectorErrors})
}

func (d *dbCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- runsCurrentDesc
	ch <- agentsDesc
	ch <- workspaceFreeDesc
	ch <- workspaceSizeDesc
	ch <- volumeFreeDesc
	ch <- volumeSizeDesc
	ch <- volumeMeasuredDesc
	ch <- minFreeDiskDesc
	ch <- claimBlockedDesc
}

// Collect queries the DB with a bounded timeout. On error it increments the
// error counter and omits the affected family — the scrape never fails.
func (d *dbCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	if counts, err := d.counts.CountRunsByStatus(ctx); err != nil {
		d.errors.Inc()
		slog.Warn("metrics: count runs by status", "error", err)
	} else {
		for _, st := range []api.RunStatus{api.RunPending, api.RunQueued, api.RunRunning} {
			ch <- prometheus.MustNewConstMetric(runsCurrentDesc, prometheus.GaugeValue,
				float64(counts[st]), string(st))
		}
	}

	if alive, stale, err := d.counts.CountAgentsByLiveness(ctx, d.staleAfter); err != nil {
		d.errors.Inc()
		slog.Warn("metrics: count agents by liveness", "error", err)
	} else {
		ch <- prometheus.MustNewConstMetric(agentsDesc, prometheus.GaugeValue, float64(alive), "alive")
		ch <- prometheus.MustNewConstMetric(agentsDesc, prometheus.GaugeValue, float64(stale), "stale")
	}

	if storage, err := d.counts.ListAgentStorage(ctx, d.staleAfter); err != nil {
		d.errors.Inc()
		slog.Warn("metrics: list agent storage", "error", err)
	} else {
		for id, s := range storage {
			emitAgentStorage(ch, id, s)
		}
	}
}

// emitAgentStorage emits one agent's series. Presence follows the pointer being
// non-nil and nothing else: a reported zero is published (a zero
// min_free_disk_bytes means the claim check is disabled, which is itself a
// warning), while a value the agent never reported gets no series at all.
//
// (agent, volume) is unique because storage is keyed by agent ID and Volumes by
// volume name, so an empty agent_name cannot make two label sets collide.
func emitAgentStorage(ch chan<- prometheus.Metric, id string, s api.AgentStorage) {
	gauge := func(desc *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, v, labels...)
	}
	if s.Workspace != nil {
		gauge(workspaceFreeDesc, float64(s.Workspace.FreeBytes), id)
		gauge(workspaceSizeDesc, float64(s.Workspace.TotalBytes), id)
	}
	for name, v := range s.Volumes {
		gauge(volumeFreeDesc, float64(v.FreeBytes), id, s.AgentName, name)
		gauge(volumeSizeDesc, float64(v.TotalBytes), id, s.AgentName, name)
		// A zero time means "never stamped"; exporting it would read as a
		// measurement from 1970 rather than as no measurement.
		if !v.MeasuredAt.IsZero() {
			gauge(volumeMeasuredDesc, float64(v.MeasuredAt.Unix()), id, s.AgentName, name)
		}
	}
	if s.MinFreeDiskBytes != nil {
		gauge(minFreeDiskDesc, float64(*s.MinFreeDiskBytes), id)
	}
	if s.ClaimBlockedByDisk != nil {
		blocked := 0.0
		if *s.ClaimBlockedByDisk {
			blocked = 1
		}
		gauge(claimBlockedDesc, blocked, id)
	}
}
