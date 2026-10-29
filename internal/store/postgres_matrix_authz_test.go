package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testBootstrapPATName = "env:UNIFIED_TOKEN"

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

func TestOwnedPATRoundTrip(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	created, err := pg.CreateOwnedPAT(ctx, NewOwnedPAT{
		Name: "owned", TokenHash: "h1", Role: "",
		Owner:       PATOwner{Sub: "s1", Email: "a@example.com", Claims: nil},
		Permissions: []string{"job.read"},
	})
	require.NoError(t, err)
	require.NotNil(t, created.Owner)

	got, err := pg.GetPATByHash(ctx, "h1")
	require.NoError(t, err)
	require.NotNil(t, got.Owner)
	assert.Equal(t, "s1", got.Owner.Sub)
	assert.Equal(t, "a@example.com", got.Owner.Email)
	assert.NotNil(t, got.Owner.Claims)
	assert.Equal(t, []string{}, got.Owner.Claims)
	assert.Equal(t, []string{"job.read"}, got.Permissions)
	assert.Equal(t, "", got.Role)

	list, err := pg.ListPATs(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.NotNil(t, list[0].Owner)
	assert.Equal(t, []string{"job.read"}, list[0].Permissions)
}

func TestLegacyPATHasNoOwner(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, err := pg.CreatePAT(ctx, "legacy", "h1", "developer", nil)
	require.NoError(t, err)
	got, err := pg.GetPATByHash(ctx, "h1")
	require.NoError(t, err)
	assert.Nil(t, got.Owner)
	assert.Nil(t, got.Permissions)
}

func TestListPATsByOwner(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	for _, c := range []struct{ name, hash, sub string }{{"na", "ha", "s1"}, {"nb", "hb", "s2"}, {"nc", "hc", "s1"}} {
		_, err := pg.CreateOwnedPAT(ctx, NewOwnedPAT{Name: c.name, TokenHash: c.hash,
			Role: "viewer", Owner: PATOwner{Sub: c.sub, Claims: []string{"g"}}})
		require.NoError(t, err)
	}
	_, err := pg.CreatePAT(ctx, "legacy", "hl", "viewer", nil)
	require.NoError(t, err)
	got, err := pg.ListPATsByOwner(ctx, "s1")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "na", got[0].Name)
	assert.Equal(t, "nc", got[1].Name)
}

func TestDeleteOwnedPATOnlyDeletesOwn(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	owned, err := pg.CreateOwnedPAT(ctx, NewOwnedPAT{Name: "o", TokenHash: "h1", Role: "viewer", Owner: PATOwner{Sub: "s1"}})
	require.NoError(t, err)
	legacy, err := pg.CreatePAT(ctx, "legacy", "h2", "viewer", nil)
	require.NoError(t, err)

	ok, err := pg.DeleteOwnedPAT(ctx, owned.ID, "other")
	require.NoError(t, err)
	assert.False(t, ok)
	_, err = pg.GetPATByHash(ctx, "h1")
	require.NoError(t, err, "row must be kept")

	ok, err = pg.DeleteOwnedPAT(ctx, legacy.ID, "")
	require.NoError(t, err)
	assert.False(t, ok, "role-only row is never owned")
	_, err = pg.GetPATByHash(ctx, "h2")
	require.NoError(t, err)

	ok, err = pg.DeleteOwnedPAT(ctx, owned.ID, "s1")
	require.NoError(t, err)
	assert.True(t, ok)
	_, err = pg.GetPATByHash(ctx, "h1")
	require.Error(t, err)
}

func TestUpdatePATOwnerSnapshot(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	for _, c := range []struct{ name, hash, sub string }{{"a", "h1", "s1"}, {"b", "h2", "s1"}, {"c", "h3", "s2"}} {
		_, err := pg.CreateOwnedPAT(ctx, NewOwnedPAT{Name: c.name, TokenHash: c.hash, Role: "viewer",
			Owner: PATOwner{Sub: c.sub, Email: "old@example.com", Claims: []string{"old"}}})
		require.NoError(t, err)
	}
	n, err := pg.UpdatePATOwnerSnapshot(ctx, "s1", "new@example.com", []string{"g1", "g2"})
	require.NoError(t, err)
	assert.EqualValues(t, 2, n)

	for _, h := range []string{"h1", "h2"} {
		p, err := pg.GetPATByHash(ctx, h)
		require.NoError(t, err)
		assert.Equal(t, "new@example.com", p.Owner.Email)
		assert.Equal(t, []string{"g1", "g2"}, p.Owner.Claims)
	}
	other, err := pg.GetPATByHash(ctx, "h3")
	require.NoError(t, err)
	assert.Equal(t, "old@example.com", other.Owner.Email)
	assert.Equal(t, []string{"old"}, other.Owner.Claims)
}

func TestCountRoleOnlyPATsAboveViewer(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, err := pg.CreatePAT(ctx, "viewer", "h1", "viewer", nil)
	require.NoError(t, err)
	_, err = pg.CreatePAT(ctx, "dev", "h2", "developer", nil)
	require.NoError(t, err)
	_, err = pg.CreatePAT(ctx, "adm", "h3", "admin", nil)
	require.NoError(t, err)
	_, err = pg.UpsertBootstrapPAT(ctx, testBootstrapPATName, "h4")
	require.NoError(t, err)
	_, err = pg.CreateOwnedPAT(ctx, NewOwnedPAT{Name: "owned", TokenHash: "h5", Role: "developer", Owner: PATOwner{Sub: "s1"}})
	require.NoError(t, err)

	n, err := pg.CountRoleOnlyPATsAboveViewer(ctx, testBootstrapPATName)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
}

func TestSessionClaimsNullVsEmpty(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	exp := time.Now().Add(time.Hour)

	s1, err := pg.CreateSession(ctx, "h1", "s1", "a@example.com", "viewer", nil, []byte{}, []byte{}, exp)
	require.NoError(t, err)
	assert.Nil(t, s1.Claims)
	got, err := pg.GetSessionByHash(ctx, "h1")
	require.NoError(t, err)
	assert.Nil(t, got.Claims)

	s2, err := pg.CreateSession(ctx, "h2", "s2", "b@example.com", "viewer", []string{}, []byte{}, []byte{}, exp)
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
	_, err := pg.UpsertJob(ctx, "team-a/build", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(ctx, "team-a/build", nil, []byte(`{}`), nil, nil, "", "")
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
	_, err = pg.CreateSession(ctx, "h0", "sub-a", "a@example.com", "viewer", nil, []byte{}, []byte{}, exp)
	require.NoError(t, err)
	_, found, err = pg.LatestSessionClaims(ctx, "a@example.com")
	require.NoError(t, err)
	assert.False(t, found)

	_, err = pg.CreateSession(ctx, "h1", "sub-a", "a@example.com", "viewer", []string{"g-old"}, []byte{}, []byte{}, exp)
	require.NoError(t, err)
	_, err = pg.CreateSession(ctx, "h2", "sub-a", "a@example.com", "viewer", []string{"g-new"}, []byte{}, []byte{}, exp)
	require.NoError(t, err)
	// A newer session without claims does not hide the older snapshot.
	_, err = pg.CreateSession(ctx, "h3", "sub-a", "a@example.com", "viewer", nil, []byte{}, []byte{}, exp)
	require.NoError(t, err)
	_, err = pg.CreateSession(ctx, "h4", "sub-b", "b@example.com", "viewer", []string{}, []byte{}, []byte{}, exp)
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
