package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigration024DropsRoleDefaults(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	for _, table := range []string{"pats", "sessions"} {
		var def *string
		var nullable string
		err := pg.pool.QueryRow(ctx, `SELECT column_default, is_nullable FROM information_schema.columns
			WHERE table_name = $1 AND column_name = 'role'`, table).Scan(&def, &nullable)
		require.NoError(t, err, table)
		assert.Nil(t, def, "%s.role must have no default", table)
		assert.Equal(t, "NO", nullable, "%s.role must stay NOT NULL", table)
	}
}

func TestAccountPATRoundTrip(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	accountID := testPATAccountID(t, pg)
	created, err := pg.CreatePAT(ctx, NewPAT{
		Name: "owned", TokenHash: "h1", Role: "", AccountID: accountID,
		Permissions: []string{"job.read"},
	})
	require.NoError(t, err)
	assert.Equal(t, accountID, created.AccountID)

	got, err := pg.GetPATByHash(ctx, "h1")
	require.NoError(t, err)
	assert.Equal(t, accountID, got.AccountID)
	assert.Equal(t, []string{"job.read"}, got.Permissions)
	assert.Equal(t, "", got.Role)

	list, err := pg.ListPATs(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, []string{"job.read"}, list[0].Permissions)
}
func TestPATWithoutPermissionsHasNilCeiling(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	createTestPAT(t, pg, "legacy", "h1", "developer", "")
	got, err := pg.GetPATByHash(ctx, "h1")
	require.NoError(t, err)
	assert.Nil(t, got.Permissions)
}
func TestSessionClaimsNullVsEmpty(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	exp := time.Now().Add(time.Hour)

	s1, err := pg.CreateSession(ctx, testPATAccountID(t, pg), "h1", "s1", "a@example.com", "viewer", nil, []byte{}, []byte{}, exp)
	require.NoError(t, err)
	assert.Nil(t, s1.Claims)
	got, err := pg.GetSessionByHash(ctx, "h1")
	require.NoError(t, err)
	assert.Nil(t, got.Claims)

	s2, err := pg.CreateSession(ctx, testPATAccountID(t, pg), "h2", "s2", "b@example.com", "viewer", []string{}, []byte{}, []byte{}, exp)
	require.NoError(t, err)
	assert.NotNil(t, s2.Claims)
	assert.Empty(t, s2.Claims)
	got, err = pg.GetSessionByHash(ctx, "h2")
	require.NoError(t, err)
	assert.NotNil(t, got.Claims)

	require.NoError(t, pg.UpdateSessionClaims(ctx, s1.ID, []string{"g1"}))
	got, err = pg.GetSessionByHash(ctx, "h1")
	require.NoError(t, err)
	assert.Equal(t, []string{"g1"}, got.Claims)
}

func TestRunJobName(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "team-a/build", "unified-cd/v1", []byte(`{}`), JobMeta{})
	require.NoError(t, err)
	run, err := pg.CreateRun(ctx, "team-a/build", nil, []byte(`{}`), nil, nil, "", "", RunOrigin{})
	require.NoError(t, err)

	job, found, err := pg.RunJobName(ctx, run.ID)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "team-a/build", job)

	_, found, err = pg.RunJobName(ctx, "00000000-0000-0000-0000-000000000000")
	require.NoError(t, err)
	assert.False(t, found)

	_, found, err = pg.RunJobName(ctx, "not-a-uuid")
	require.NoError(t, err)
	assert.False(t, found)
}

func TestAuditLogDecisionColumns(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	require.NoError(t, pg.InsertAuditLog(ctx, AuditEntry{Actor: "alice", Method: "POST", Path: "/api/v1/runs", Action: "run.trigger",
		Resource: "team-a/build", Status: 200, Permission: "run.trigger", Scope: "team-a", GrantedBy: "group:my-org:team-a"}))
	require.NoError(t, pg.InsertAuditLog(ctx, AuditEntry{Actor: "bob", Method: "DELETE", Path: "/x", Action: "job.delete", Status: 204}))

	list, err := pg.ListAuditLogs(ctx, 10, 0)
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "bob", list[0].Actor)
	assert.Equal(t, "", list[0].Permission)
	assert.Equal(t, "", list[0].Scope)
	assert.Equal(t, "", list[0].GrantedBy)
	assert.Equal(t, "run.trigger", list[1].Permission)
	assert.Equal(t, "team-a", list[1].Scope)
	assert.Equal(t, "group:my-org:team-a", list[1].GrantedBy)

	var nulls int
	require.NoError(t, pg.pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE actor='bob'
		AND permission IS NULL AND scope IS NULL AND granted_by IS NULL`).Scan(&nulls))
	assert.Equal(t, 1, nulls)
}

func TestLatestSessionClaims(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	exp := time.Now().Add(time.Hour)

	_, found, err := pg.LatestSessionClaims(ctx, "a@example.com")
	require.NoError(t, err)
	assert.False(t, found)

	// A NULL-claims session is not a snapshot.
	_, err = pg.CreateSession(ctx, testPATAccountID(t, pg), "h0", "sub-a", "a@example.com", "viewer", nil, []byte{}, []byte{}, exp)
	require.NoError(t, err)
	_, found, err = pg.LatestSessionClaims(ctx, "a@example.com")
	require.NoError(t, err)
	assert.False(t, found)

	_, err = pg.CreateSession(ctx, testPATAccountID(t, pg), "h1", "sub-a", "a@example.com", "viewer", []string{"g-old"}, []byte{}, []byte{}, exp)
	require.NoError(t, err)
	_, err = pg.CreateSession(ctx, testPATAccountID(t, pg), "h2", "sub-a", "a@example.com", "viewer", []string{"g-new"}, []byte{}, []byte{}, exp)
	require.NoError(t, err)
	// A newer session without claims does not hide the older snapshot.
	_, err = pg.CreateSession(ctx, testPATAccountID(t, pg), "h3", "sub-a", "a@example.com", "viewer", nil, []byte{}, []byte{}, exp)
	require.NoError(t, err)
	_, err = pg.CreateSession(ctx, testPATAccountID(t, pg), "h4", "sub-b", "b@example.com", "viewer", []string{}, []byte{}, []byte{}, exp)
	require.NoError(t, err)

	for _, user := range []string{"a@example.com", "sub-a"} {
		claims, found, err := pg.LatestSessionClaims(ctx, user)
		require.NoError(t, err)
		assert.True(t, found, user)
		assert.Equal(t, []string{"g-new"}, claims, user)
	}

	claims, found, err := pg.LatestSessionClaims(ctx, "b@example.com")
	require.NoError(t, err)
	assert.True(t, found, "an empty claim list is still a snapshot")
	assert.NotNil(t, claims)
	assert.Empty(t, claims)
}

func TestMigration025AddsPATEnrollLabels(t *testing.T) {
	dsn := NewEmptyTestDatabase(t)
	require.NoError(t, (&Postgres{}).MigrateTo(dsn, 24))
	require.NoError(t, (&Postgres{}).MigrateTo(dsn, 25))
	pg, err := NewPostgres(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(pg.Close)

	var typ, nullable string
	require.NoError(t, pg.pool.QueryRow(context.Background(), `SELECT data_type, is_nullable FROM information_schema.columns
		WHERE table_name = 'pats' AND column_name = 'enroll_labels'`).Scan(&typ, &nullable))
	assert.Equal(t, "ARRAY", typ)
	assert.Equal(t, "YES", nullable)
}

func TestPATEnrollLabelsRoundTrip(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, err := pg.CreatePAT(ctx, NewPAT{
		Name: "owned", TokenHash: "h1", AccountID: testPATAccountID(t, pg),
		Permissions:  []string{"agent.enroll"},
		EnrollLabels: []string{"kind:linux"},
	})
	require.NoError(t, err)
	got, err := pg.GetPATByHash(ctx, "h1")
	require.NoError(t, err)
	assert.Equal(t, []string{"kind:linux"}, got.EnrollLabels)

	createTestPAT(t, pg, "legacy", "h2", "developer", "")
	legacy, err := pg.GetPATByHash(ctx, "h2")
	require.NoError(t, err)
	assert.Nil(t, legacy.EnrollLabels)

	list, err := pg.ListPATs(ctx)
	require.NoError(t, err)
	byName := map[string]PAT{}
	for _, p := range list {
		byName[p.Name] = p
	}
	assert.Equal(t, []string{"kind:linux"}, byName["owned"].EnrollLabels)
}
