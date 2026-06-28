package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/unified-cd/unified-cd/internal/api"
	"github.com/unified-cd/unified-cd/internal/store"
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
	updated     map[string]time.Time
	createErr   error
}

func (m *mockScheduleFireStore) ListSchedules(_ context.Context) ([]store.Schedule, error) {
	return m.schedules, nil
}

func (m *mockScheduleFireStore) CreateRun(_ context.Context, jobName string, params map[string]string, _ []byte, _ []string, triggeredBy string) (*api.Run, error) {
	if m.createErr != nil {
		return nil, m.createErr
	}
	r := &api.Run{JobName: jobName, TriggeredBy: triggeredBy}
	m.created = append(m.created, r)
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
	m := &mockScheduleFireStore{
		schedules: []store.Schedule{
			{Name: "daily", Cron: "0 10 * * *", JobName: "build", LastFiredAt: &lastFired},
		},
	}
	checkAndFireSchedules(context.Background(), m, testNow)

	require.Len(t, m.created, 1)
	assert.Equal(t, "build", m.created[0].JobName)
	assert.Equal(t, "schedule:daily", m.created[0].TriggeredBy)
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

	assert.Empty(t, m.created)             // Run is not created
	assert.NotNil(t, m.updated["old"])     // last_fired_at is advanced
}

func TestCheckAndFireSchedules_NullLastFiredAt(t *testing.T) {
	// cron "30 10 * * *", no last_fired_at
	// base = now - 1h = 10:00 → Next(10:00) = today 10:30
	// 10:30 ∈ [10:00, 11:00] → fires
	m := &mockScheduleFireStore{
		schedules: []store.Schedule{
			{Name: "new", Cron: "30 10 * * *", JobName: "build", LastFiredAt: nil},
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

func TestCheckAndFireSchedules_CreateRunError_DoesNotUpdateLastFiredAt(t *testing.T) {
	// CreateRun fails → last_fired_at is not updated (retry on next tick).
	lastFired := testNow.Add(-25 * time.Hour)
	m := &mockScheduleFireStore{
		schedules: []store.Schedule{
			{Name: "daily", Cron: "0 10 * * *", JobName: "missing-job", LastFiredAt: &lastFired},
		},
		createErr: fmt.Errorf("job not found"),
	}
	checkAndFireSchedules(context.Background(), m, testNow)

	assert.Empty(t, m.created)
	assert.Empty(t, m.updated) // not updated — allows retry on the next tick
}
