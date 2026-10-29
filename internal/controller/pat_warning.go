package controller

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/eirueimi/unified-cd/internal/store"
)

// WarnRoleOnlyPATs logs a warning when folder grants are configured and role-only
// personal access tokens with a role above viewer exist: those tokens carry a
// global role and so bypass folder grants until revoked. The break-glass row is
// not counted. It is a no-op without folder grants.
func WarnRoleOnlyPATs(ctx context.Context, st store.Store, pol *authz.Policy, log *slog.Logger) error {
	if !pol.HasFolderGrants() {
		return nil
	}
	n, err := st.CountRoleOnlyPATsAboveViewer(ctx, BootstrapPATName)
	if err != nil {
		return fmt.Errorf("count role-only PATs: %w", err)
	}
	if n > 0 {
		log.Warn("role-only personal access tokens with a role above viewer bypass folder grants until revoked", "count", n)
	}
	return nil
}
