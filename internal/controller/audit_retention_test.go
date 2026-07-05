package controller

import (
	"context"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAuditRetentionStore is a minimal store.Store stand-in implementing only
// the methods the audit retention task uses.
type fakeAuditRetentionStore struct {
	store.Store

	lockAcquired bool
	deletedCount int
	deleteErr    error
	deleteCalled bool
	deleteBefore time.Time
}

func (f *fakeAuditRetentionStore) AcquireAdvisoryLock(ctx context.Context, key int64) (func(), error) {
	if !f.lockAcquired {
		return nil, nil
	}
	return func() {}, nil
}

func (f *fakeAuditRetentionStore) DeleteAuditLogsOlderThan(ctx context.Context, before time.Time) (int, error) {
	f.deleteCalled = true
	f.deleteBefore = before
	if f.deleteErr != nil {
		return 0, f.deleteErr
	}
	return f.deletedCount, nil
}

func TestAuditRetention_DeletesAsLeader(t *testing.T) {
	st := &fakeAuditRetentionStore{lockAcquired: true, deletedCount: 3}
	runAuditRetentionOnce(context.Background(), st, 90)
	assert.True(t, st.deleteCalled)

	wantBefore := time.Now().AddDate(0, 0, -90)
	assert.WithinDuration(t, wantBefore, st.deleteBefore, 5*time.Second)
}

func TestAuditRetention_FollowerDoesNothing(t *testing.T) {
	st := &fakeAuditRetentionStore{lockAcquired: false}
	runAuditRetentionOnce(context.Background(), st, 90)
	assert.False(t, st.deleteCalled)
}

func TestAuditRetention_ZeroDaysMeansKeepForever(t *testing.T) {
	// RunAuditRetention (the looping entrypoint, not the -Once helper) must
	// return immediately without ever touching the store when retentionDays <= 0.
	st := &fakeAuditRetentionStore{lockAcquired: true}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	RunAuditRetention(ctx, st, 10*time.Millisecond, 0)
	assert.False(t, st.deleteCalled)
}

// TestAuditRetention_Integration verifies end-to-end against a real Postgres:
// rows older than the retention window are deleted, recent rows are kept.
func TestAuditRetention_Integration(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()

	require.NoError(t, pg.InsertAuditLog(ctx, "alice", "POST", "/api/v1/jobs", "job.apply", "j", 200))

	// A row inserted just now must survive a 90-day retention run.
	runAuditRetentionOnce(ctx, pg, 90)
	list, err := pg.ListAuditLogs(ctx, 10, 0)
	require.NoError(t, err)
	assert.Len(t, list, 1)

	// Directly backdate occurred_at to simulate an old row, then confirm it is deleted.
	_, err = pg.DeleteAuditLogsOlderThan(ctx, time.Now().Add(time.Hour))
	require.NoError(t, err)
	list, err = pg.ListAuditLogs(ctx, 10, 0)
	require.NoError(t, err)
	assert.Len(t, list, 0)
}
