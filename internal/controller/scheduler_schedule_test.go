package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockScheduleFireStore satisfies store.Store via nil embedding.
// Implements only ListSchedules, CreateRun, and UpdateScheduleLastFiredAt.
// Other method calls will panic.
type mockScheduleFireStore struct {
	store.Store // nil embedding — other method calls will panic
	schedules   []store.Schedule
	created     []*api.Run
	// createdRequiredCaps parallels created — the requiredCaps slice
	// checkAndFireSchedules passed to CreateRun for each fired schedule, so
	// tests can assert capability inference without a real Postgres
	// required_caps column to read back.
	createdRequiredCaps [][]string
	// createdSpecs parallels created — the spec []byte checkAndFireSchedules
	// passed to CreateRun for each fired schedule, so tests can assert the
	// job's actual spec is used instead of a literal `{}`.
	createdSpecs [][]byte
	// createdAgentSelectors parallels created — the agentSelector slice
	// checkAndFireSchedules passed to CreateRun for each fired schedule, so
	// tests can assert the job's agentSelector (expanded with schedule
	// params) is propagated instead of nil.
	createdAgentSelectors [][]string
	updated               map[string]time.Time
	createErr             error
	jobs                  map[string]*api.Job // optional; GetJob returns "not found" when absent
}

func (m *mockScheduleFireStore) ListSchedules(_ context.Context) ([]store.Schedule, error) {
	return m.schedules, nil
}

// GetJob returns the job whose spec supplies the run's SPEC, its input defs
// (for param validation), and its capability inference. Returns an error
// when the job isn't registered in the mock, which checkAndFireSchedules
// handles by skipping the fire entirely (see the "job spec unavailable"
// subtest below).
func (m *mockScheduleFireStore) GetJob(_ context.Context, name string) (*api.Job, error) {
	if job, ok := m.jobs[name]; ok {
		return job, nil
	}
	return nil, fmt.Errorf("job not found: %s", name)
}

func (m *mockScheduleFireStore) CreateRun(_ context.Context, jobName string, params map[string]string, spec []byte, agentSelector []string, requiredCaps []string, triggeredBy string) (*api.Run, error) {
	if m.createErr != nil {
		return nil, m.createErr
	}
	r := &api.Run{JobName: jobName, TriggeredBy: triggeredBy}
	m.created = append(m.created, r)
	m.createdRequiredCaps = append(m.createdRequiredCaps, requiredCaps)
	m.createdSpecs = append(m.createdSpecs, spec)
	m.createdAgentSelectors = append(m.createdAgentSelectors, agentSelector)
	return r, nil
}

func (m *mockScheduleFireStore) UpdateScheduleLastFiredAt(_ context.Context, name string, firedAt time.Time) error {
	if m.updated == nil {
		m.updated = map[string]time.Time{}
	}
	m.updated[name] = firedAt
	return nil
}

// now = 2026-06-16 11:00:00 UTC
var testNow = time.Date(2026, 6, 16, 11, 0, 0, 0, time.UTC)

func TestCheckAndFireSchedules_FiresWhenDue(t *testing.T) {
	// cron "0 10 * * *" fires at 10:00 UTC every day
	// last_fired_at = yesterday 10:00 → next = today 10:00 → within [now-1h, now] → fires
	lastFired := testNow.Add(-25 * time.Hour) // yesterday 10:00 (25 hours ago)
	jobSpec := []byte(`{"steps":[{"name":"s","run":"echo hi"}]}`)
	m := &mockScheduleFireStore{
		schedules: []store.Schedule{
			{Name: "daily", Cron: "0 10 * * *", JobName: "build", LastFiredAt: &lastFired},
		},
		jobs: map[string]*api.Job{
			"build": {Name: "build", Spec: jobSpec},
		},
	}
	checkAndFireSchedules(context.Background(), m, testNow)

	require.Len(t, m.created, 1)
	assert.Equal(t, "build", m.created[0].JobName)
	assert.Equal(t, "schedule:daily", m.created[0].TriggeredBy)
	require.Len(t, m.createdSpecs, 1)
	assert.Equal(t, jobSpec, m.createdSpecs[0], "the run's spec must be the job's spec, not an empty {}")
	require.NotNil(t, m.updated["daily"])
}

func TestCheckAndFireSchedules_SkipsWhenTooOld(t *testing.T) {
	// cron "0 8 * * *" (fires at 08:00)
	// last_fired_at = 2 days ago → next = yesterday 08:00 → before now-1h=10:00 → skip, advance last_fired_at
	lastFired := testNow.Add(-49 * time.Hour) // 2 days ago 10:00
	m := &mockScheduleFireStore{
		schedules: []store.Schedule{
			{Name: "old", Cron: "0 8 * * *", JobName: "build", LastFiredAt: &lastFired},
		},
	}
	checkAndFireSchedules(context.Background(), m, testNow)

	assert.Empty(t, m.created)         // Run is not created
	assert.NotNil(t, m.updated["old"]) // last_fired_at is advanced
}

func TestCheckAndFireSchedules_NullLastFiredAt(t *testing.T) {
	// cron "30 10 * * *", no last_fired_at
	// base = now - 1h = 10:00 → Next(10:00) = today 10:30
	// 10:30 ∈ [10:00, 11:00] → fires
	m := &mockScheduleFireStore{
		schedules: []store.Schedule{
			{Name: "new", Cron: "30 10 * * *", JobName: "build", LastFiredAt: nil},
		},
		jobs: map[string]*api.Job{
			"build": {Name: "build", Spec: []byte(`{"steps":[{"name":"s","run":"echo hi"}]}`)},
		},
	}
	checkAndFireSchedules(context.Background(), m, testNow)

	require.Len(t, m.created, 1, "10:30 is within [10:00, 11:00] so it should fire")
	assert.Equal(t, "schedule:new", m.created[0].TriggeredBy)
}

func TestCheckAndFireSchedules_NoFireWhenNotDue(t *testing.T) {
	// cron "0 12 * * *" fires at 12:00
	// last_fired_at = today 10:00 (1 hour ago) → next = today 12:00 > now=11:00 → not yet due
	lastFired := testNow.Add(-time.Hour) // today 10:00 UTC
	m := &mockScheduleFireStore{
		schedules: []store.Schedule{
			{Name: "future", Cron: "0 12 * * *", JobName: "build", LastFiredAt: &lastFired},
		},
	}
	checkAndFireSchedules(context.Background(), m, testNow)

	assert.Empty(t, m.created)
	assert.Empty(t, m.updated) // nothing is updated since next > now
}

func TestCheckAndFireSchedules_InjectsDefaultParam(t *testing.T) {
	// The job declares a `tag` input with a default; the schedule doesn't set it.
	lastFired := testNow.Add(-25 * time.Hour)
	m := &mockScheduleFireStore{
		schedules: []store.Schedule{
			{Name: "daily", Cron: "0 10 * * *", JobName: "build", LastFiredAt: &lastFired},
		},
		jobs: map[string]*api.Job{
			"build": {Name: "build", Spec: []byte(`{"params":{"inputs":[{"name":"tag","type":"string","default":"latest"}]}}`)},
		},
	}
	checkAndFireSchedules(context.Background(), m, testNow)

	require.Len(t, m.created, 1)
	require.NotNil(t, m.updated["daily"])
}

func TestCheckAndFireSchedules_MissingRequiredParam_SkipsAndDoesNotAdvance(t *testing.T) {
	// The job declares a required `image` input with no default; the schedule
	// doesn't supply it, so the Run must not be created and last_fired_at must
	// not advance (so the next tick can retry once the schedule/job is fixed).
	lastFired := testNow.Add(-25 * time.Hour)
	m := &mockScheduleFireStore{
		schedules: []store.Schedule{
			{Name: "daily", Cron: "0 10 * * *", JobName: "build", LastFiredAt: &lastFired},
		},
		jobs: map[string]*api.Job{
			"build": {Name: "build", Spec: []byte(`{"params":{"inputs":[{"name":"image","type":"string","required":true}]}}`)},
		},
	}
	checkAndFireSchedules(context.Background(), m, testNow)

	assert.Empty(t, m.created)
	assert.Empty(t, m.updated)
}

// TestCheckAndFireSchedules_PersistsRequiredCaps verifies that a fired
// schedule infers dsl.RequiredCaps from the job spec loaded via GetJob and
// passes it into CreateRun, mirroring the direct-trigger and webhook paths
// (see api_runs.go's handleTriggerRun and api_webhooks.go's
// handleWebhookIngress). Before this fix checkAndFireSchedules always passed
// nil for required_caps, so a scheduled run of a native-only or
// kubernetes-only-podTemplate job could be wrongly claimed by any agent
// regardless of its advertised capabilities — required_caps='{}' is a
// subset of every agent's capabilities.
func TestCheckAndFireSchedules_PersistsRequiredCaps(t *testing.T) {
	t.Run("native job infers native capability", func(t *testing.T) {
		lastFired := testNow.Add(-25 * time.Hour)
		jobSpec := []byte(`{"native":true,"steps":[{"name":"s","run":"echo x"}]}`)
		m := &mockScheduleFireStore{
			schedules: []store.Schedule{
				{Name: "native-sched", Cron: "0 10 * * *", JobName: "nativejob", LastFiredAt: &lastFired},
			},
			jobs: map[string]*api.Job{
				"nativejob": {Name: "nativejob", Spec: jobSpec},
			},
		}
		checkAndFireSchedules(context.Background(), m, testNow)

		require.Len(t, m.created, 1)
		require.Len(t, m.createdRequiredCaps, 1)
		assert.Equal(t, []string{"native"}, m.createdRequiredCaps[0])
		require.Len(t, m.createdSpecs, 1)
		assert.Equal(t, jobSpec, m.createdSpecs[0])
	})

	t.Run("kubernetes-only podTemplate infers pod capability", func(t *testing.T) {
		lastFired := testNow.Add(-25 * time.Hour)
		podSpec := []byte(`{"podTemplate":{"spec":{"containers":[{"name":"job","image":"busybox","volumeMounts":[]}]}},` +
			`"steps":[{"name":"s","run":"echo x"}]}`)
		m := &mockScheduleFireStore{
			schedules: []store.Schedule{
				{Name: "pod-sched", Cron: "0 10 * * *", JobName: "podjob", LastFiredAt: &lastFired},
			},
			jobs: map[string]*api.Job{
				"podjob": {Name: "podjob", Spec: podSpec},
			},
		}
		checkAndFireSchedules(context.Background(), m, testNow)

		require.Len(t, m.created, 1)
		require.Len(t, m.createdRequiredCaps, 1)
		assert.Equal(t, []string{"pod"}, m.createdRequiredCaps[0])
		require.Len(t, m.createdSpecs, 1)
		assert.Equal(t, podSpec, m.createdSpecs[0])
	})

	t.Run("plain job infers container capability", func(t *testing.T) {
		lastFired := testNow.Add(-25 * time.Hour)
		jobSpec := []byte(`{"steps":[{"name":"s","run":"echo x"}]}`)
		m := &mockScheduleFireStore{
			schedules: []store.Schedule{
				{Name: "plain-sched", Cron: "0 10 * * *", JobName: "plainjob", LastFiredAt: &lastFired},
			},
			jobs: map[string]*api.Job{
				"plainjob": {Name: "plainjob", Spec: jobSpec},
			},
		}
		checkAndFireSchedules(context.Background(), m, testNow)

		require.Len(t, m.created, 1)
		require.Len(t, m.createdRequiredCaps, 1)
		assert.Equal(t, []string{"container"}, m.createdRequiredCaps[0])
		require.Len(t, m.createdSpecs, 1)
		assert.Equal(t, jobSpec, m.createdSpecs[0])
	})

	t.Run("job spec unavailable skips firing entirely", func(t *testing.T) {
		// GetJob fails (job not in the mock's jobs map) — checkAndFireSchedules
		// can no longer fire best-effort with nil required_caps, because the
		// job spec is also the run's SPEC: firing without it would create a
		// Run with an empty {} spec that buildClaimResponse (api_agent.go)
		// turns into zero stages/steps, silently running nothing. So it skips
		// the fire and leaves last_fired_at untouched, allowing a retry once
		// the job/schedule is fixed.
		lastFired := testNow.Add(-25 * time.Hour)
		m := &mockScheduleFireStore{
			schedules: []store.Schedule{
				{Name: "daily", Cron: "0 10 * * *", JobName: "build", LastFiredAt: &lastFired},
			},
		}
		checkAndFireSchedules(context.Background(), m, testNow)

		assert.Empty(t, m.created)
		assert.Empty(t, m.createdRequiredCaps)
		assert.Empty(t, m.createdSpecs)
		assert.Empty(t, m.updated)
	})
}

func TestCheckAndFireSchedules_CreateRunError_DoesNotUpdateLastFiredAt(t *testing.T) {
	// The job loads fine (so this exercises the CreateRun failure path, not
	// the GetJob-failure skip path), but CreateRun itself fails →
	// last_fired_at is not updated (retry on next tick).
	lastFired := testNow.Add(-25 * time.Hour)
	m := &mockScheduleFireStore{
		schedules: []store.Schedule{
			{Name: "daily", Cron: "0 10 * * *", JobName: "build", LastFiredAt: &lastFired},
		},
		jobs: map[string]*api.Job{
			"build": {Name: "build", Spec: []byte(`{"steps":[{"name":"s","run":"echo hi"}]}`)},
		},
		createErr: fmt.Errorf("db unavailable"),
	}
	checkAndFireSchedules(context.Background(), m, testNow)

	assert.Empty(t, m.created)
	assert.Empty(t, m.updated) // not updated — allows retry on the next tick
}

// TestCheckAndFireSchedules_PropagatesAgentSelector covers the bug this
// change fixes: before this fix, checkAndFireSchedules always passed nil for
// agentSelector to CreateRun, so a scheduled run of a job with
// `agentSelector: [pool:build]` could be claimed by ANY agent — the job
// author's explicit routing was silently dropped. It now mirrors the
// API-trigger and webhook paths (api_runs.go's handleTriggerRun,
// api_webhooks.go's handleWebhookIngress): parse the job spec, expand
// agentSelector with the resolved params, and pass the expanded selector to
// CreateRun.
func TestCheckAndFireSchedules_PropagatesAgentSelector(t *testing.T) {
	t.Run("static selector is passed through unchanged", func(t *testing.T) {
		lastFired := testNow.Add(-25 * time.Hour)
		jobSpec := []byte(`{"agentSelector":["pool:build","kind:linux"],"steps":[{"name":"s","run":"echo hi"}]}`)
		m := &mockScheduleFireStore{
			schedules: []store.Schedule{
				{Name: "daily", Cron: "0 10 * * *", JobName: "build", LastFiredAt: &lastFired},
			},
			jobs: map[string]*api.Job{
				"build": {Name: "build", Spec: jobSpec},
			},
		}
		checkAndFireSchedules(context.Background(), m, testNow)

		require.Len(t, m.created, 1)
		require.Len(t, m.createdAgentSelectors, 1)
		assert.Equal(t, []string{"pool:build", "kind:linux"}, m.createdAgentSelectors[0])
		require.NotNil(t, m.updated["daily"])
	})

	t.Run("templated selector is expanded with schedule params", func(t *testing.T) {
		lastFired := testNow.Add(-25 * time.Hour)
		jobSpec := []byte(`{"agentSelector":["pool:{{ .Params.pool }}"],` +
			`"params":{"inputs":[{"name":"pool","type":"string","default":"default-pool"}]},` +
			`"steps":[{"name":"s","run":"echo hi"}]}`)
		m := &mockScheduleFireStore{
			schedules: []store.Schedule{
				{Name: "daily", Cron: "0 10 * * *", JobName: "build", LastFiredAt: &lastFired, Params: map[string]string{"pool": "gpu-pool"}},
			},
			jobs: map[string]*api.Job{
				"build": {Name: "build", Spec: jobSpec},
			},
		}
		checkAndFireSchedules(context.Background(), m, testNow)

		require.Len(t, m.created, 1)
		require.Len(t, m.createdAgentSelectors, 1)
		assert.Equal(t, []string{"pool:gpu-pool"}, m.createdAgentSelectors[0], "the EXPANDED selector must be used, not the raw template")
		require.NotNil(t, m.updated["daily"])
	})

	// A selector template that fails to expand (malformed {{ }} syntax) is
	// the only way dsl.ExpandAgentSelector returns an error: a *missing*
	// param key does not error (text/template's `missingkey=zero` option
	// renders it as an empty string — see
	// internal/dsl/template_test.go's TestExpandAgentSelector_PropagatesTemplateError
	// vs. TestExpandConcurrency_MissingParamKeyExpandsToEmpty for the same
	// distinction on a sibling expansion function). A merely-missing param
	// therefore renders a degenerate selector element (e.g. "pool:") — which
	// the scheduler ALSO skips (see the degenerate-element case below):
	// unlike the API/webhook paths, where the caller immediately sees the
	// Queued run, an unattended schedule would deterministically produce a
	// reaper-failed run once per fire, forever.
	t.Run("selector template syntax error skips fire and does not advance last_fired_at", func(t *testing.T) {
		lastFired := testNow.Add(-25 * time.Hour)
		jobSpec := []byte(`{"agentSelector":["pool:{{ .Params.pool"],` + // unclosed template action — parse error
			`"steps":[{"name":"s","run":"echo hi"}]}`)
		m := &mockScheduleFireStore{
			schedules: []store.Schedule{
				{Name: "daily", Cron: "0 10 * * *", JobName: "build", LastFiredAt: &lastFired},
			},
			jobs: map[string]*api.Job{
				"build": {Name: "build", Spec: jobSpec},
			},
		}
		checkAndFireSchedules(context.Background(), m, testNow)

		assert.Empty(t, m.created, "no Run should be created when the agentSelector template fails to expand")
		assert.Empty(t, m.updated, "last_fired_at must not advance — allow retry on the next tick once the spec is fixed")
	})

	// Complements the syntax-error case above: a param referenced by the
	// selector template that simply isn't supplied (and has no default)
	// renders to an empty string rather than erroring — the element becomes
	// degenerate ("pool:"), which can never match an agent label, so the
	// scheduler skips the fire (warn + retry next tick) instead of producing
	// a run the queued-run reaper would deterministically fail, once per
	// fire, forever.
	t.Run("selector element expanding empty skips fire and does not advance last_fired_at", func(t *testing.T) {
		lastFired := testNow.Add(-25 * time.Hour)
		jobSpec := []byte(`{"agentSelector":["pool:{{ .Params.pool }}"],"steps":[{"name":"s","run":"echo hi"}]}`)
		m := &mockScheduleFireStore{
			schedules: []store.Schedule{
				{Name: "daily", Cron: "0 10 * * *", JobName: "build", LastFiredAt: &lastFired},
			},
			jobs: map[string]*api.Job{
				"build": {Name: "build", Spec: jobSpec},
			},
		}
		checkAndFireSchedules(context.Background(), m, testNow)

		assert.Empty(t, m.created, "no Run should be created for a selector element that expanded empty")
		assert.Empty(t, m.updated, "last_fired_at must not advance — warn and retry next tick")
	})

	// A statically-authored degenerate element ("pool:") is skipped the same
	// way — the guard looks at the EXPANDED selector, however it got there.
	t.Run("static empty-value selector element also skips fire", func(t *testing.T) {
		lastFired := testNow.Add(-25 * time.Hour)
		jobSpec := []byte(`{"agentSelector":["pool:"],"steps":[{"name":"s","run":"echo hi"}]}`)
		m := &mockScheduleFireStore{
			schedules: []store.Schedule{
				{Name: "daily", Cron: "0 10 * * *", JobName: "build", LastFiredAt: &lastFired},
			},
			jobs: map[string]*api.Job{
				"build": {Name: "build", Spec: jobSpec},
			},
		}
		checkAndFireSchedules(context.Background(), m, testNow)

		assert.Empty(t, m.created)
		assert.Empty(t, m.updated)
	})

	// Bare labels without a colon (e.g. "kubernetes") are NOT degenerate and
	// must keep firing normally.
	t.Run("bare label selector element fires normally", func(t *testing.T) {
		lastFired := testNow.Add(-25 * time.Hour)
		jobSpec := []byte(`{"agentSelector":["kubernetes"],"steps":[{"name":"s","run":"echo hi"}]}`)
		m := &mockScheduleFireStore{
			schedules: []store.Schedule{
				{Name: "daily", Cron: "0 10 * * *", JobName: "build", LastFiredAt: &lastFired},
			},
			jobs: map[string]*api.Job{
				"build": {Name: "build", Spec: jobSpec},
			},
		}
		checkAndFireSchedules(context.Background(), m, testNow)

		require.Len(t, m.created, 1)
		require.Len(t, m.createdAgentSelectors, 1)
		assert.Equal(t, []string{"kubernetes"}, m.createdAgentSelectors[0])
		require.NotNil(t, m.updated["daily"])
	})
}

// TestCheckAndFireSchedules_SpecParseFailure_FiresWithNilCapsAndSelector
// covers the degraded branch documented in checkAndFireSchedules: when the
// job's stored spec fails to parse as JSON, the fire proceeds best-effort
// (the raw spec bytes are still valid as the run's SPEC) but both
// requiredCaps and agentSelector fall back to nil, since neither can be
// derived without a parsed dsl.Spec.
func TestCheckAndFireSchedules_SpecParseFailure_FiresWithNilCapsAndSelector(t *testing.T) {
	lastFired := testNow.Add(-25 * time.Hour)
	m := &mockScheduleFireStore{
		schedules: []store.Schedule{
			{Name: "daily", Cron: "0 10 * * *", JobName: "build", LastFiredAt: &lastFired},
		},
		jobs: map[string]*api.Job{
			"build": {Name: "build", Spec: []byte(`not valid json`)},
		},
	}
	checkAndFireSchedules(context.Background(), m, testNow)

	require.Len(t, m.created, 1, "an unparseable spec still fires best-effort")
	require.Len(t, m.createdRequiredCaps, 1)
	assert.Empty(t, m.createdRequiredCaps[0])
	require.Len(t, m.createdAgentSelectors, 1)
	assert.Empty(t, m.createdAgentSelectors[0])
	require.NotNil(t, m.updated["daily"])
}
