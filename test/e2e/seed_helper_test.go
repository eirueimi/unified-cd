package e2e

import (
	"context"
	"testing"

	"github.com/eirueimi/unified-cd/internal/controller"
	"github.com/eirueimi/unified-cd/internal/store"
)

// mustSeedBootstrapPAT reproduces the "sync UNIFIED_TOKEN to DB as a PAT" step that production
// main.go performs at startup, so that e2e test server setup also has it. Because ServerAuth has no
// branch dedicated to static tokens, the fixed token used in tests must be registered as a real PAT
// or the management API (/api/v1/jobs, etc.) will return 401.
func mustSeedBootstrapPAT(t *testing.T, pg *store.Postgres, token string) error {
	t.Helper()
	_, err := pg.UpsertBootstrapPAT(context.Background(), "test-bootstrap", controller.HashToken(token))
	return err
}
