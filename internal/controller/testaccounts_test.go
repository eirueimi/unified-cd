package controller

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/require"
)

// testAdminAccount is the service account behind the "secret" bearer token
// that most API tests send.
const testAdminAccount = "test-admin"

// withTestAdmin returns cfg with the test-admin service account added to its
// ServiceAccountGrants (admin bundle). The input map is not modified.
func withTestAdmin(cfg Config) Config {
	grants := make(map[string]authz.Grants, len(cfg.ServiceAccountGrants)+1)
	for name, g := range cfg.ServiceAccountGrants {
		grants[name] = g
	}
	if _, ok := grants[testAdminAccount]; !ok {
		grants[testAdminAccount] = authz.RoleGrants("admin", "sa:"+testAdminAccount)
	}
	cfg.ServiceAccountGrants = grants
	return cfg
}

// syncTestServiceAccounts makes the service-account rows match cfg.
func syncTestServiceAccounts(t testing.TB, st store.Store, cfg Config) {
	t.Helper()
	names := make([]string, 0, len(cfg.ServiceAccountGrants))
	for name := range cfg.ServiceAccountGrants {
		names = append(names, name)
	}
	sort.Strings(names)
	require.NoError(t, st.SyncServiceAccounts(context.Background(), names))
}

// serviceAccountByName returns the service account row called name.
func serviceAccountByName(t testing.TB, st store.Store, name string) *store.Account {
	t.Helper()
	accts, err := st.ListAccounts(context.Background())
	require.NoError(t, err)
	for i := range accts {
		if accts[i].Kind == store.AccountKindService && accts[i].Name == name {
			return &accts[i]
		}
	}
	t.Fatalf("service account %q not found", name)
	return nil
}

// seedServicePAT stores a PAT with the given token for the named service
// account (which must already be synced).
func seedServicePAT(t testing.TB, st store.Store, account, token string) *store.PAT {
	t.Helper()
	acct := serviceAccountByName(t, st, account)
	pat, err := st.CreatePAT(context.Background(), store.NewPAT{
		Name: account, TokenHash: HashToken(token), AccountID: acct.ID,
	})
	require.NoError(t, err)
	return pat
}

// newServerWithAdminPAT builds a server over pg whose config gains the
// test-admin service account (admin bundle) with a PAT whose value is
// "secret". The service accounts of cfg are synced as well.
func newServerWithAdminPAT(t testing.TB, pg store.Store, cfg Config) *Server {
	t.Helper()
	cfg = withTestAdmin(cfg)
	syncTestServiceAccounts(t, pg, cfg)
	seedServicePAT(t, pg, testAdminAccount, "secret")
	return NewServer(cfg, pg)
}

// addHumanAccount upserts the human account for sub.
func addHumanAccount(t testing.TB, st store.Store, sub, email string, claims []string) *store.Account {
	t.Helper()
	acct, err := st.UpsertHumanAccount(context.Background(), sub, authzTestIssuer, email, claims)
	require.NoError(t, err)
	return acct
}

// createOwnedPATForTest stores a PAT owned by the human account of in.Owner
// (created on demand), the shape every PAT minted by a person now has.
func createOwnedPATForTest(t testing.TB, st store.Store, in ownedPATInput) *store.PAT {
	t.Helper()
	acct := addHumanAccount(t, st, in.Owner.Sub, in.Owner.Email, in.Owner.Claims)
	pat, err := st.CreatePAT(context.Background(), store.NewPAT{
		Name: in.Name, TokenHash: in.TokenHash, AccountID: acct.ID, Role: in.Role,
		Permissions: in.Permissions, EnrollLabels: in.EnrollLabels, ExpiresAt: in.ExpiresAt, CreatedBy: in.CreatedBy,
	})
	require.NoError(t, err)
	return pat
}

// expiresIn returns a pointer to now+d.
func expiresIn(d time.Duration) *time.Time {
	t := time.Now().Add(d)
	return &t
}

// addRoleServicePAT adds a service account called account holding the global
// role bundle to the running server's config, syncs the account rows, and
// stores a PAT with the given token for it. It replaces the removed role-only
// PATs in tests that need a non-admin or differently named caller.
func addRoleServicePAT(t testing.TB, s *Server, st store.Store, account, token, role string) *store.PAT {
	t.Helper()
	cfg := withTestAdmin(s.cfg)
	if _, ok := s.cfg.ServiceAccountGrants[testAdminAccount]; !ok {
		delete(cfg.ServiceAccountGrants, testAdminAccount)
	}
	cfg.ServiceAccountGrants[account] = authz.RoleGrants(role, "role:"+role)
	s.cfg = cfg
	syncTestServiceAccounts(t, st, cfg)
	acct := serviceAccountByName(t, st, account)
	pat, err := st.CreatePAT(context.Background(), store.NewPAT{
		Name: account, TokenHash: HashToken(token), AccountID: acct.ID,
	})
	require.NoError(t, err)
	return pat
}

// accountOf returns the account that owns pat.
func accountOf(t testing.TB, st store.Store, pat *store.PAT) *store.Account {
	t.Helper()
	require.NotEmpty(t, pat.AccountID)
	acct, err := st.GetAccount(context.Background(), pat.AccountID)
	require.NoError(t, err)
	require.NotNil(t, acct)
	return acct
}

// ownerIdentity is the OIDC identity behind a test PAT.
type ownerIdentity struct {
	Sub    string
	Email  string
	Claims []string
}

// ownedPATInput describes a PAT minted by a person for createOwnedPATForTest.
type ownedPATInput struct {
	Name, TokenHash, Role string
	Owner                 ownerIdentity
	Permissions           []string
	EnrollLabels          []string
	ExpiresAt             *time.Time
	CreatedBy             string
}
