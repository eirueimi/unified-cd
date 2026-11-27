package e2e

import (
	"context"
	"fmt"
	"testing"

	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/eirueimi/unified-cd/internal/controller"
	"github.com/eirueimi/unified-cd/internal/store"
)

// testAdminAccount is the service account behind the fixed bearer token the
// e2e tests send.
const testAdminAccount = "test-admin"

// adminConfig returns cfg with the test-admin service account (admin bundle)
// declared, as a controller config with a serviceAccounts block would.
func adminConfig(cfg controller.Config) controller.Config {
	cfg.ServiceAccountGrants = map[string]authz.Grants{
		testAdminAccount: authz.RoleGrants("admin", "sa:"+testAdminAccount),
	}
	return cfg
}

// mustSeedAdminPAT reproduces what production does once an operator has declared
// a service account and minted a PAT for it: it syncs the service-account rows
// and stores a PAT with the given token for test-admin. Pair it with a server
// built from adminConfig, because ServerAuth resolves every PAT through its
// account (there is no static-token branch).
func mustSeedAdminPAT(t *testing.T, pg *store.Postgres, token string) error {
	t.Helper()
	ctx := context.Background()
	if err := pg.SyncServiceAccounts(ctx, []string{testAdminAccount}); err != nil {
		return err
	}
	accts, err := pg.ListAccounts(ctx)
	if err != nil {
		return err
	}
	for _, a := range accts {
		if a.Kind == store.AccountKindService && a.Name == testAdminAccount {
			_, err := pg.CreateAccountPAT(ctx, store.NewPAT{Name: testAdminAccount, TokenHash: controller.HashToken(token), AccountID: a.ID})
			return err
		}
	}
	return fmt.Errorf("service account %q not found after sync", testAdminAccount)
}
