package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func accountByName(t *testing.T, pg *Postgres, name string) Account {
	t.Helper()
	all, err := pg.ListAccounts(context.Background())
	require.NoError(t, err)
	for _, a := range all {
		if a.Kind == AccountKindService && a.Name == name {
			return a
		}
	}
	t.Fatalf("service account %q not found", name)
	return Account{}
}

func TestSyncServiceAccounts(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	require.NoError(t, pg.SyncServiceAccounts(ctx, []string{"a", "b"}))
	a1 := accountByName(t, pg, "a")
	assert.Nil(t, a1.DisabledAt)

	require.NoError(t, pg.SyncServiceAccounts(ctx, []string{"b"}))
	assert.NotNil(t, accountByName(t, pg, "a").DisabledAt)
	assert.Nil(t, accountByName(t, pg, "b").DisabledAt)
	assert.Nil(t, accountByName(t, pg, "admin").DisabledAt)

	require.NoError(t, pg.SyncServiceAccounts(ctx, []string{"a", "b"}))
	a2 := accountByName(t, pg, "a")
	assert.Nil(t, a2.DisabledAt)
	assert.Equal(t, a1.ID, a2.ID)

	require.NoError(t, pg.SyncServiceAccounts(ctx, nil))
	assert.NotNil(t, accountByName(t, pg, "a").DisabledAt)
	assert.NotNil(t, accountByName(t, pg, "b").DisabledAt)
	assert.Nil(t, accountByName(t, pg, "admin").DisabledAt)
}

func TestInitialTokenClaimAndConsume(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	st, err := pg.GetBootstrapState(ctx)
	require.NoError(t, err)
	assert.False(t, st.Done)
	assert.Equal(t, "", st.InitialTokenHash)

	var wg sync.WaitGroup
	results := make([]bool, 2)
	for i, h := range []string{"hash-a", "hash-b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := pg.ClaimInitialToken(ctx, h)
			assert.NoError(t, err)
			results[i] = ok
		}()
	}
	wg.Wait()
	assert.NotEqual(t, results[0], results[1], "exactly one claim wins")
	st, err = pg.GetBootstrapState(ctx)
	require.NoError(t, err)
	winner := st.InitialTokenHash
	require.NotEmpty(t, winner)

	ok, err := pg.ConsumeInitialToken(ctx, "wrong")
	require.NoError(t, err)
	assert.False(t, ok)

	consumed := make([]bool, 2)
	for i := range consumed {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := pg.ConsumeInitialToken(ctx, winner)
			assert.NoError(t, err)
			consumed[i] = ok
		}()
	}
	wg.Wait()
	assert.NotEqual(t, consumed[0], consumed[1], "exactly one consume wins")

	st, err = pg.GetBootstrapState(ctx)
	require.NoError(t, err)
	assert.True(t, st.Done)
	assert.Equal(t, "", st.InitialTokenHash)

	ok, err = pg.ClaimInitialToken(ctx, "hash-c")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestSetAdminEnabledUntil(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	until := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	require.NoError(t, pg.SetAdminEnabledUntil(ctx, until))
	st, err := pg.GetBootstrapState(ctx)
	require.NoError(t, err)
	require.NotNil(t, st.AdminEnabledUntil)
	assert.True(t, st.AdminEnabledUntil.Equal(until))
}

func TestUpsertHumanAccountKeepsDisabled(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	a, err := pg.UpsertHumanAccount(ctx, "sub1", "https://issuer.example.com", "a@example.com", []string{"g1"})
	require.NoError(t, err)
	assert.Equal(t, AccountKindHuman, a.Kind)
	require.NoError(t, pg.SetAccountDisabled(ctx, a.ID, true))

	b, err := pg.UpsertHumanAccount(ctx, "sub1", "https://issuer.example.com", "b@example.com", []string{"g2"})
	require.NoError(t, err)
	assert.Equal(t, a.ID, b.ID)
	assert.NotNil(t, b.DisabledAt)
	assert.Equal(t, []string{"g2"}, b.Claims)
	assert.Equal(t, "b@example.com", b.Email)
	assert.NotNil(t, b.LastSeenAt)

	got, err := pg.GetAccount(ctx, a.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.NotNil(t, got.DisabledAt)
	missing, err := pg.GetAccount(ctx, "00000000-0000-0000-0000-000000000000")
	require.NoError(t, err)
	assert.Nil(t, missing)
}

func TestAccountPATsAndSessions(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	acc, err := pg.UpsertHumanAccount(ctx, "sub1", "", "a@example.com", nil)
	require.NoError(t, err)

	pat, err := pg.CreatePAT(ctx, NewPAT{Name: "p", TokenHash: "h", AccountID: acc.ID, Permissions: []string{"job.read"}, CreatedBy: "oidc:sub1"})
	require.NoError(t, err)
	assert.Equal(t, acc.ID, pat.AccountID)
	list, err := pg.ListPATsByAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)

	ok, err := pg.DeleteAccountPAT(ctx, pat.ID, "00000000-0000-0000-0000-000000000000")
	require.NoError(t, err)
	assert.False(t, ok)
	ok, err = pg.DeleteAccountPAT(ctx, pat.ID, acc.ID)
	require.NoError(t, err)
	assert.True(t, ok)

	s, err := pg.CreateSession(ctx, acc.ID, "th", "sub1", "a@example.com", "", []string{"g"}, []byte("d"), []byte("c"), time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, acc.ID, s.AccountID)
	got, err := pg.GetSessionByHash(ctx, "th")
	require.NoError(t, err)
	assert.Equal(t, acc.ID, got.AccountID)
}
