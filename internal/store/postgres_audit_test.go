package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgres_Audit_InsertAndList(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	require.NoError(t, pg.InsertAuditLog(ctx, "alice", "POST", "/api/v1/secrets", "secret.set", "db-password", 204))
	require.NoError(t, pg.InsertAuditLog(ctx, "bob", "DELETE", "/api/v1/jobs/foo", "job.delete", "foo", 204))

	list, err := pg.ListAuditLogs(ctx, 100, 0)
	require.NoError(t, err)
	require.Len(t, list, 2)

	// newest first
	assert.Equal(t, "bob", list[0].Actor)
	assert.Equal(t, "job.delete", list[0].Action)
	assert.Equal(t, "foo", list[0].Resource)
	assert.Equal(t, 204, list[0].Status)

	assert.Equal(t, "alice", list[1].Actor)
	assert.Equal(t, "secret.set", list[1].Action)
	assert.Equal(t, "db-password", list[1].Resource)
}

func TestPostgres_Audit_ListPagination(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		require.NoError(t, pg.InsertAuditLog(ctx, "alice", "POST", "/api/v1/jobs", "job.apply", "j", 200))
	}

	list, err := pg.ListAuditLogs(ctx, 2, 0)
	require.NoError(t, err)
	require.Len(t, list, 2)

	list2, err := pg.ListAuditLogs(ctx, 2, 2)
	require.NoError(t, err)
	require.Len(t, list2, 2)

	assert.NotEqual(t, list[0].ID, list2[0].ID)
}

func TestPostgres_Audit_DeleteOlderThan(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	require.NoError(t, pg.InsertAuditLog(ctx, "alice", "POST", "/api/v1/jobs", "job.apply", "j", 200))

	// Nothing is older than now-1h yet.
	n, err := pg.DeleteAuditLogsOlderThan(ctx, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	// Everything is older than now+1h.
	n, err = pg.DeleteAuditLogsOlderThan(ctx, time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	list, err := pg.ListAuditLogs(ctx, 100, 0)
	require.NoError(t, err)
	assert.Len(t, list, 0)
}
