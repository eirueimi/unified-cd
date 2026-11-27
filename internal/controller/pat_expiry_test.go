package controller

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/metrics"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/eirueimi/unified-cd/internal/store/storetest"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPATExpiryWarning(t *testing.T) {
	pg := storetest.NewTestPostgres(t)
	cfg := withTestAdmin(Config{})
	syncTestServiceAccounts(t, pg, cfg)
	acct := serviceAccountByName(t, pg, testAdminAccount)
	now := time.Now()
	mk := func(name string, exp *time.Time) {
		_, err := pg.CreatePAT(t.Context(), store.NewPAT{Name: name, TokenHash: HashToken(name), AccountID: acct.ID, ExpiresAt: exp})
		require.NoError(t, err)
	}
	soon, later := now.Add(3*24*time.Hour), now.Add(10*24*time.Hour)
	past := now.Add(-time.Hour)
	mk("soon", &soon)
	mk("later", &later)
	mk("never", nil)
	mk("expired", &past) // already expired: not "expiring soon"

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	m := metrics.New()
	n, err := CheckExpiringPATs(t.Context(), pg, m, now, log)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, 1.0, testutil.ToFloat64(m.PATsExpiringSoonForTest()))
	out := buf.String()
	assert.Contains(t, out, "personal access tokens expire within 7 days")
	assert.Contains(t, out, "count=1")

	// Nothing expiring: gauge back to 0 and no warning.
	buf.Reset()
	later2 := now.Add(30 * 24 * time.Hour)
	require.NoError(t, pg.DeletePAT(t.Context(), mustPATID(t, pg, "soon")))
	mk("far", &later2)
	n, err = CheckExpiringPATs(t.Context(), pg, m, now, log)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	assert.Equal(t, 0.0, testutil.ToFloat64(m.PATsExpiringSoonForTest()))
	assert.False(t, strings.Contains(buf.String(), "expire within"))
}

func mustPATID(t *testing.T, pg store.Store, tokenName string) string {
	t.Helper()
	p, err := pg.GetPATByHash(t.Context(), HashToken(tokenName))
	require.NoError(t, err)
	require.NotNil(t, p)
	return p.ID
}
