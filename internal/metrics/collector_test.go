package metrics

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeCounts struct {
	runs         map[api.RunStatus]int
	alive, stale int
	err          error
	storage      map[string]api.AgentStorage
	storageErr   error
	// gotStaleAfter records the window ListAgentStorage was last called with.
	gotStaleAfter time.Duration
}

func (f *fakeCounts) ListAgentStorage(ctx context.Context, staleAfter time.Duration) (map[string]api.AgentStorage, error) {
	f.gotStaleAfter = staleAfter
	return f.storage, f.storageErr
}

func (f *fakeCounts) CountRunsByStatus(ctx context.Context) (map[api.RunStatus]int, error) {
	return f.runs, f.err
}

func (f *fakeCounts) CountAgentsByLiveness(ctx context.Context, staleAfter time.Duration) (int, int, error) {
	return f.alive, f.stale, f.err
}

func TestDBCollectorExportsGauges(t *testing.T) {
	m := New()
	m.RegisterDBCollector(&fakeCounts{
		runs:  map[api.RunStatus]int{api.RunPending: 2, api.RunRunning: 1},
		alive: 3, stale: 1,
	}, 90*time.Second)

	expected := `
# HELP unifiedcd_agents Registered agents, by heartbeat liveness.
# TYPE unifiedcd_agents gauge
unifiedcd_agents{state="alive"} 3
unifiedcd_agents{state="stale"} 1
# HELP unifiedcd_runs_current Current number of non-terminal runs, by status.
# TYPE unifiedcd_runs_current gauge
unifiedcd_runs_current{status="Pending"} 2
unifiedcd_runs_current{status="Queued"} 0
unifiedcd_runs_current{status="Running"} 1
`
	require.NoError(t, testutil.GatherAndCompare(m.reg, strings.NewReader(expected),
		"unifiedcd_runs_current", "unifiedcd_agents"))
}

func TestDBCollectorErrorsIncrementCounterAndOmitFamilies(t *testing.T) {
	m := New()
	m.RegisterDBCollector(&fakeCounts{err: errors.New("db down")}, 90*time.Second)

	families, err := m.reg.Gather()
	require.NoError(t, err) // scrape itself must not fail
	for _, f := range families {
		assert.NotEqual(t, "unifiedcd_runs_current", f.GetName())
		assert.NotEqual(t, "unifiedcd_agents", f.GetName())
	}
	assert.Equal(t, 2.0, testutil.ToFloat64(m.collectorErrors)) // one per failed query
}

func u64(v uint64) *uint64 { return &v }
func bptr(v bool) *bool    { return &v }

// storageFamilies are the series this collector adds for agent disk headroom.
var storageFamilies = []string{
	"unifiedcd_agent_workspace_filesystem_free_bytes",
	"unifiedcd_agent_workspace_filesystem_size_bytes",
	"unifiedcd_agent_volume_filesystem_free_bytes",
	"unifiedcd_agent_volume_filesystem_size_bytes",
	"unifiedcd_agent_volume_measured_timestamp_seconds",
	"unifiedcd_agent_min_free_disk_bytes",
	"unifiedcd_agent_claim_blocked_by_disk",
}

func TestCollector_ExportsWorkspaceVolumeAndThreshold(t *testing.T) {
	m := New()
	m.RegisterDBCollector(&fakeCounts{storage: map[string]api.AgentStorage{
		"agent-1": {
			AgentName:          "build-host",
			Workspace:          &api.FilesystemStats{FreeBytes: 700, TotalBytes: 1000},
			Volumes:            map[string]api.VolumeStats{"unity": {FilesystemStats: api.FilesystemStats{FreeBytes: 40, TotalBytes: 100}, MeasuredAt: time.Unix(1700000000, 0)}},
			MinFreeDiskBytes:   u64(500),
			ClaimBlockedByDisk: bptr(true),
		},
	}}, 90*time.Second)

	expected := `
# HELP unifiedcd_agent_claim_blocked_by_disk 1 if the agent is currently declining to claim work because free disk is below its minFreeDisk threshold, else 0. Absent for an agent that performs no claim-time disk check.
# TYPE unifiedcd_agent_claim_blocked_by_disk gauge
unifiedcd_agent_claim_blocked_by_disk{agent="agent-1"} 1
# HELP unifiedcd_agent_min_free_disk_bytes The agent's configured minFreeDisk threshold in bytes; 0 means the claim-time disk check is disabled. Absent for an agent with no such setting.
# TYPE unifiedcd_agent_min_free_disk_bytes gauge
unifiedcd_agent_min_free_disk_bytes{agent="agent-1"} 500
# HELP unifiedcd_agent_volume_filesystem_free_bytes Free bytes on the filesystem backing a named volume, as last measured by the agent.
# TYPE unifiedcd_agent_volume_filesystem_free_bytes gauge
unifiedcd_agent_volume_filesystem_free_bytes{agent="agent-1",agent_name="build-host",volume="unity"} 40
# HELP unifiedcd_agent_volume_filesystem_size_bytes Total bytes of the filesystem backing a named volume, as last measured by the agent.
# TYPE unifiedcd_agent_volume_filesystem_size_bytes gauge
unifiedcd_agent_volume_filesystem_size_bytes{agent="agent-1",agent_name="build-host",volume="unity"} 100
# HELP unifiedcd_agent_volume_measured_timestamp_seconds Unix time at which the agent last measured a named volume's filesystem.
# TYPE unifiedcd_agent_volume_measured_timestamp_seconds gauge
unifiedcd_agent_volume_measured_timestamp_seconds{agent="agent-1",agent_name="build-host",volume="unity"} 1.7e+09
# HELP unifiedcd_agent_workspace_filesystem_free_bytes Free bytes on the agent's workspace filesystem, as last measured by the agent.
# TYPE unifiedcd_agent_workspace_filesystem_free_bytes gauge
unifiedcd_agent_workspace_filesystem_free_bytes{agent="agent-1"} 700
# HELP unifiedcd_agent_workspace_filesystem_size_bytes Total bytes of the agent's workspace filesystem, as last measured by the agent.
# TYPE unifiedcd_agent_workspace_filesystem_size_bytes gauge
unifiedcd_agent_workspace_filesystem_size_bytes{agent="agent-1"} 1000
`
	require.NoError(t, testutil.GatherAndCompare(m.reg, strings.NewReader(expected), storageFamilies...))
}

// The design's central promise: a deleted or reaped agent's series vanish
// rather than freezing at their last healthy value. The collector reads the
// store at scrape time, so the same registry must stop exporting the agent the
// moment the store stops returning it.
func TestCollector_ADeletedAgentHasNoSeries(t *testing.T) {
	fc := &fakeCounts{storage: map[string]api.AgentStorage{
		"agent-1": {
			AgentName:          "build-host",
			Workspace:          &api.FilesystemStats{FreeBytes: 700, TotalBytes: 1000},
			Volumes:            map[string]api.VolumeStats{"unity": {MeasuredAt: time.Unix(1700000000, 0)}},
			MinFreeDiskBytes:   u64(500),
			ClaimBlockedByDisk: bptr(false),
		},
	}}
	m := New()
	m.RegisterDBCollector(fc, 90*time.Second)

	before := exportedFamilies(t, m)
	for _, name := range storageFamilies {
		require.True(t, before[name], "precondition: %s is exported while the agent exists", name)
	}

	// The agent is deleted (or reaped, or goes stale): the store no longer
	// returns it.
	fc.storage = map[string]api.AgentStorage{}
	after := exportedFamilies(t, m)
	for _, name := range storageFamilies {
		assert.False(t, after[name], "series %s for a deleted agent must disappear", name)
	}
}

// A Kubernetes agent has no workspace filesystem and no minFreeDisk, and
// performs no claim-time check. Exporting a zero for any of them would say
// "plenty of space" or "not blocked" about something it never reported.
func TestCollector_OmitsSeriesTheAgentDidNotReport(t *testing.T) {
	m := New()
	m.RegisterDBCollector(&fakeCounts{storage: map[string]api.AgentStorage{
		"k8s-1": {
			AgentName: "k8s-agent",
			Volumes:   map[string]api.VolumeStats{"cache": {FilesystemStats: api.FilesystemStats{FreeBytes: 10, TotalBytes: 20}, MeasuredAt: time.Unix(1700000000, 0)}},
		},
	}}, 90*time.Second)

	got := exportedFamilies(t, m)
	assert.True(t, got["unifiedcd_agent_volume_filesystem_free_bytes"])
	assert.True(t, got["unifiedcd_agent_volume_filesystem_size_bytes"])
	assert.True(t, got["unifiedcd_agent_volume_measured_timestamp_seconds"])
	assert.False(t, got["unifiedcd_agent_workspace_filesystem_free_bytes"])
	assert.False(t, got["unifiedcd_agent_workspace_filesystem_size_bytes"])
	assert.False(t, got["unifiedcd_agent_min_free_disk_bytes"])
	assert.False(t, got["unifiedcd_agent_claim_blocked_by_disk"])
}

// Presence is the pointer being non-nil, never the value: a reported zero
// threshold means "the check is disabled" and a reported false means "not
// blocked". Both must be published.
func TestCollector_PublishesReportedZeroAndFalse(t *testing.T) {
	m := New()
	m.RegisterDBCollector(&fakeCounts{storage: map[string]api.AgentStorage{
		"agent-1": {
			AgentName:          "build-host",
			Workspace:          &api.FilesystemStats{FreeBytes: 0, TotalBytes: 0},
			MinFreeDiskBytes:   u64(0),
			ClaimBlockedByDisk: bptr(false),
		},
	}}, 90*time.Second)

	expected := `
# HELP unifiedcd_agent_claim_blocked_by_disk 1 if the agent is currently declining to claim work because free disk is below its minFreeDisk threshold, else 0. Absent for an agent that performs no claim-time disk check.
# TYPE unifiedcd_agent_claim_blocked_by_disk gauge
unifiedcd_agent_claim_blocked_by_disk{agent="agent-1"} 0
# HELP unifiedcd_agent_min_free_disk_bytes The agent's configured minFreeDisk threshold in bytes; 0 means the claim-time disk check is disabled. Absent for an agent with no such setting.
# TYPE unifiedcd_agent_min_free_disk_bytes gauge
unifiedcd_agent_min_free_disk_bytes{agent="agent-1"} 0
# HELP unifiedcd_agent_workspace_filesystem_free_bytes Free bytes on the agent's workspace filesystem, as last measured by the agent.
# TYPE unifiedcd_agent_workspace_filesystem_free_bytes gauge
unifiedcd_agent_workspace_filesystem_free_bytes{agent="agent-1"} 0
# HELP unifiedcd_agent_workspace_filesystem_size_bytes Total bytes of the agent's workspace filesystem, as last measured by the agent.
# TYPE unifiedcd_agent_workspace_filesystem_size_bytes gauge
unifiedcd_agent_workspace_filesystem_size_bytes{agent="agent-1"} 0
`
	require.NoError(t, testutil.GatherAndCompare(m.reg, strings.NewReader(expected), storageFamilies...))
}

// Replicas of one Kubernetes agent share PVCs (hence agent_name and volume)
// but enrol under their own agent IDs. Two series for one physical volume is
// intended; operators aggregate with min by (agent_name, volume).
func TestCollector_TwoReplicasSharingAVolumeExportTwoSeries(t *testing.T) {
	vol := map[string]api.VolumeStats{"cache": {FilesystemStats: api.FilesystemStats{FreeBytes: 10, TotalBytes: 20}, MeasuredAt: time.Unix(1700000000, 0)}}
	m := New()
	m.RegisterDBCollector(&fakeCounts{storage: map[string]api.AgentStorage{
		"k8s-a": {AgentName: "k8s-agent", Volumes: vol},
		"k8s-b": {AgentName: "k8s-agent", Volumes: vol},
	}}, 90*time.Second)

	expected := `
# HELP unifiedcd_agent_volume_filesystem_free_bytes Free bytes on the filesystem backing a named volume, as last measured by the agent.
# TYPE unifiedcd_agent_volume_filesystem_free_bytes gauge
unifiedcd_agent_volume_filesystem_free_bytes{agent="k8s-a",agent_name="k8s-agent",volume="cache"} 10
unifiedcd_agent_volume_filesystem_free_bytes{agent="k8s-b",agent_name="k8s-agent",volume="cache"} 10
`
	require.NoError(t, testutil.GatherAndCompare(m.reg, strings.NewReader(expected),
		"unifiedcd_agent_volume_filesystem_free_bytes"))
}

// An agent that reported no name still has a unique (agent, volume) pair, so
// the scrape must neither fail nor drop it.
func TestCollector_AnEmptyAgentNameStillExports(t *testing.T) {
	vol := map[string]api.VolumeStats{"cache": {FilesystemStats: api.FilesystemStats{FreeBytes: 10, TotalBytes: 20}, MeasuredAt: time.Unix(1700000000, 0)}}
	m := New()
	m.RegisterDBCollector(&fakeCounts{storage: map[string]api.AgentStorage{
		"a": {Volumes: vol},
		"b": {Volumes: vol},
	}}, 90*time.Second)

	expected := `
# HELP unifiedcd_agent_volume_filesystem_free_bytes Free bytes on the filesystem backing a named volume, as last measured by the agent.
# TYPE unifiedcd_agent_volume_filesystem_free_bytes gauge
unifiedcd_agent_volume_filesystem_free_bytes{agent="a",agent_name="",volume="cache"} 10
unifiedcd_agent_volume_filesystem_free_bytes{agent="b",agent_name="",volume="cache"} 10
`
	require.NoError(t, testutil.GatherAndCompare(m.reg, strings.NewReader(expected),
		"unifiedcd_agent_volume_filesystem_free_bytes"))
}

// A volume with no measurement time is dropped whole: free and size with no age
// are a figure whose staleness nobody can judge, and a timestamp of zero would
// read as 1970. A stamped volume beside it is unaffected.
func TestCollector_AZeroMeasuredAtDropsTheWholeVolume(t *testing.T) {
	m := New()
	m.RegisterDBCollector(&fakeCounts{storage: map[string]api.AgentStorage{
		"a": {AgentName: "n", Volumes: map[string]api.VolumeStats{
			"unstamped": {FilesystemStats: api.FilesystemStats{FreeBytes: 1, TotalBytes: 2}},
			"stamped":   {FilesystemStats: api.FilesystemStats{FreeBytes: 3, TotalBytes: 4}, MeasuredAt: time.Unix(1700000000, 0)},
		}},
	}}, 90*time.Second)

	expected := `
# HELP unifiedcd_agent_volume_filesystem_free_bytes Free bytes on the filesystem backing a named volume, as last measured by the agent.
# TYPE unifiedcd_agent_volume_filesystem_free_bytes gauge
unifiedcd_agent_volume_filesystem_free_bytes{agent="a",agent_name="n",volume="stamped"} 3
# HELP unifiedcd_agent_volume_filesystem_size_bytes Total bytes of the filesystem backing a named volume, as last measured by the agent.
# TYPE unifiedcd_agent_volume_filesystem_size_bytes gauge
unifiedcd_agent_volume_filesystem_size_bytes{agent="a",agent_name="n",volume="stamped"} 4
# HELP unifiedcd_agent_volume_measured_timestamp_seconds Unix time at which the agent last measured a named volume's filesystem.
# TYPE unifiedcd_agent_volume_measured_timestamp_seconds gauge
unifiedcd_agent_volume_measured_timestamp_seconds{agent="a",agent_name="n",volume="stamped"} 1.7e+09
`
	require.NoError(t, testutil.GatherAndCompare(m.reg, strings.NewReader(expected),
		"unifiedcd_agent_volume_filesystem_free_bytes",
		"unifiedcd_agent_volume_filesystem_size_bytes",
		"unifiedcd_agent_volume_measured_timestamp_seconds"))
}

// MinFreeDiskBytes and ClaimBlockedByDisk are presence-keyed on their own
// pointers, not on Workspace. An agent with no workspace reading that still
// reported a disabled threshold and "not blocked" must publish both (at 0) and
// no workspace series. This is the guard against using Workspace != nil as a
// proxy for "host agent".
func TestCollector_ThresholdAndBlockedDoNotDependOnWorkspace(t *testing.T) {
	m := New()
	m.RegisterDBCollector(&fakeCounts{storage: map[string]api.AgentStorage{
		"agent-1": {
			AgentName:          "build-host",
			Workspace:          nil,
			MinFreeDiskBytes:   u64(0),
			ClaimBlockedByDisk: bptr(false),
		},
	}}, 90*time.Second)

	expected := `
# HELP unifiedcd_agent_claim_blocked_by_disk 1 if the agent is currently declining to claim work because free disk is below its minFreeDisk threshold, else 0. Absent for an agent that performs no claim-time disk check.
# TYPE unifiedcd_agent_claim_blocked_by_disk gauge
unifiedcd_agent_claim_blocked_by_disk{agent="agent-1"} 0
# HELP unifiedcd_agent_min_free_disk_bytes The agent's configured minFreeDisk threshold in bytes; 0 means the claim-time disk check is disabled. Absent for an agent with no such setting.
# TYPE unifiedcd_agent_min_free_disk_bytes gauge
unifiedcd_agent_min_free_disk_bytes{agent="agent-1"} 0
`
	require.NoError(t, testutil.GatherAndCompare(m.reg, strings.NewReader(expected), storageFamilies...))
}

// The collector must pass its configured liveness window to the store, not a
// literal; otherwise stale agents would be reported (or live ones dropped).
func TestCollector_PassesItsStaleAfterWindowToTheStore(t *testing.T) {
	fc := &fakeCounts{}
	m := New()
	m.RegisterDBCollector(fc, 37*time.Second)
	exportedFamilies(t, m)
	assert.Equal(t, 37*time.Second, fc.gotStaleAfter)
}

func TestCollector_AStoreErrorIncrementsCollectorErrors(t *testing.T) {
	m := New()
	m.RegisterDBCollector(&fakeCounts{
		alive: 3, stale: 1,
		storage:    map[string]api.AgentStorage{"agent-1": {Workspace: &api.FilesystemStats{}}},
		storageErr: errors.New("db down"),
	}, 90*time.Second)

	got := exportedFamilies(t, m)
	for _, name := range storageFamilies {
		assert.False(t, got[name], name)
	}
	assert.True(t, got["unifiedcd_agents"], "an unrelated family must survive a storage error")
	assert.Equal(t, 1.0, testutil.ToFloat64(m.collectorErrors))
}
