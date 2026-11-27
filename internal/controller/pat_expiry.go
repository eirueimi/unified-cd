package controller

import (
	"context"
	"log/slog"
	"time"

	"github.com/eirueimi/unified-cd/internal/metrics"
	"github.com/eirueimi/unified-cd/internal/store"
)

const (
	// patExpiryWarnWindow is how far ahead a PAT expiry is reported.
	patExpiryWarnWindow = 7 * 24 * time.Hour
	// patExpiryCheckInterval is how often RunPATExpiryCheck looks.
	patExpiryCheckInterval = 24 * time.Hour
)

// CheckExpiringPATs counts the PATs that expire within the next 7 days (not
// yet expired), publishes the count on the unifiedcd_pats_expiring_soon gauge
// and logs a warning when it is above zero. It reads only the database, so it
// needs no coordination between replicas: every replica reports.
func CheckExpiringPATs(ctx context.Context, st store.Store, m *metrics.Metrics, now time.Time, log *slog.Logger) (int, error) {
	pats, err := st.ListPATs(ctx)
	if err != nil {
		return 0, err
	}
	accts, err := st.ListAccounts(ctx)
	if err != nil {
		return 0, err
	}
	disabled := make(map[string]bool, len(accts))
	for _, a := range accts {
		if a.DisabledAt != nil {
			disabled[a.ID] = true
		}
	}
	var ids []string
	horizon := now.Add(patExpiryWarnWindow)
	for _, p := range pats {
		// A PAT of a disabled account cannot be used, and a recovery PAT is
		// short-lived by design; neither is worth a warning. Bootstrap PATs
		// are 720h operator PATs and still count.
		if disabled[p.AccountID] || p.CreatedBy == "recovery" {
			continue
		}
		if p.ExpiresAt != nil && p.ExpiresAt.After(now) && !p.ExpiresAt.After(horizon) {
			ids = append(ids, p.ID)
		}
	}
	m.SetPATsExpiringSoon(len(ids))
	if len(ids) > 0 {
		log.Warn("personal access tokens expire within 7 days", "count", len(ids), "ids", ids)
	}
	return len(ids), nil
}

// RunPATExpiryCheck runs CheckExpiringPATs once at startup and then every 24
// hours until ctx is done.
func RunPATExpiryCheck(ctx context.Context, st store.Store, m *metrics.Metrics) {
	check := func() {
		if _, err := CheckExpiringPATs(ctx, st, m, time.Now(), slog.Default()); err != nil && ctx.Err() == nil {
			slog.Warn("check expiring personal access tokens", "error", err)
		}
	}
	check()
	ticker := time.NewTicker(patExpiryCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		}
	}
}
