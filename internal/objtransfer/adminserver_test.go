package objtransfer_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/eirueimi/unified-cd/internal/controller"
	"github.com/eirueimi/unified-cd/internal/store"
)

// newAdminServer builds a controller over pg that knows the service account
// test-admin (admin bundle) and stores a PAT for it whose value is "secret".
func newAdminServer(t *testing.T, pg *store.Postgres) *controller.Server {
	t.Helper()
	ctx := t.Context()
	require.NoError(t, pg.SyncServiceAccounts(ctx, []string{"test-admin"}))
	accts, err := pg.ListAccounts(ctx)
	require.NoError(t, err)
	for _, a := range accts {
		if a.Kind == store.AccountKindService && a.Name == "test-admin" {
			_, err := pg.CreateAccountPAT(ctx, store.NewPAT{Name: "test-admin", TokenHash: controller.HashToken("secret"), AccountID: a.ID})
			require.NoError(t, err)
		}
	}
	return controller.NewServer(controller.Config{
		ServiceAccountGrants: map[string]authz.Grants{"test-admin": authz.RoleGrants("admin", "sa:test-admin")},
	}, pg)
}
