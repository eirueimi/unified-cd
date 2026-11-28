package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/eirueimi/unified-cd/internal/store/storetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func bootstrapPost(t testing.TB, s *Server, initial, name string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"initialToken": initial, "name": name})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/bootstrap", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec
}

func newBootstrapServer(t *testing.T, cfg Config, initial string) (*Server, store.Store) {
	t.Helper()
	pg := storetest.NewTestPostgres(t)
	cfg.AdminEnabled = true
	if cfg.TokenMaxLifetime == 0 {
		cfg.TokenMaxLifetime = 8760 * time.Hour
	}
	if initial != "" {
		stored, err := pg.ClaimInitialToken(t.Context(), HashToken(initial))
		require.NoError(t, err)
		require.True(t, stored)
	}
	s := NewServer(cfg, pg)
	t.Cleanup(s.Close)
	return s, pg
}

func TestBootstrapExchangeOnce(t *testing.T) {
	s, pg := newBootstrapServer(t, Config{}, "uci_testvalue")
	s.bootstrapLimiter = newRateLimiter(nil, time.Nanosecond, 1000)

	const n = 20
	recs := make([]*httptest.ResponseRecorder, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			recs[i] = bootstrapPost(t, s, "uci_testvalue", "")
		}(i)
	}
	wg.Wait()

	var created, rejected int
	var resp api.CreatePATResponse
	for _, rec := range recs {
		switch rec.Code {
		case http.StatusCreated:
			created++
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		case http.StatusUnauthorized:
			rejected++
			assert.Equal(t, "invalid or already used initial token\n", rec.Body.String())
		default:
			t.Fatalf("unexpected status %d: %s", rec.Code, rec.Body.String())
		}
	}
	assert.Equal(t, 1, created)
	assert.Equal(t, n-1, rejected)
	assert.Equal(t, "bootstrap", resp.Name)

	// The PAT authenticates as the admin account.
	rec := doAs(t, s, http.MethodGet, "/api/v1/auth/me", resp.Token, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "admin")

	pats, err := pg.ListPATs(t.Context())
	require.NoError(t, err)
	var found *store.PAT
	for i := range pats {
		if pats[i].ID == resp.ID {
			found = &pats[i]
		}
	}
	require.NotNil(t, found)
	require.NotNil(t, found.ExpiresAt)
	assert.WithinDuration(t, time.Now().Add(720*time.Hour), *found.ExpiresAt, time.Minute)
	assert.Equal(t, "bootstrap", found.CreatedBy)

	st, err := pg.GetBootstrapState(t.Context())
	require.NoError(t, err)
	assert.True(t, st.Done)

	logs, err := pg.ListAuditLogs(t.Context(), 50, 0)
	require.NoError(t, err)
	var rows int
	for _, l := range logs {
		if l.Action == "bootstrap.exchange" {
			rows++
			assert.Equal(t, "bootstrap", l.ActorID)
			assert.Equal(t, "bootstrap", l.Actor)
			assert.JSONEq(t, `{"patId":"`+resp.ID+`"}`, string(l.Details))
		}
	}
	assert.Equal(t, 1, rows)
}

func TestBootstrapWrongValue(t *testing.T) {
	s, _ := newBootstrapServer(t, Config{}, "uci_right")
	rec := bootstrapPost(t, s, "uci_wrong", "")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, "invalid or already used initial token\n", rec.Body.String())
}

func TestBootstrapNoInitialTokenStored(t *testing.T) {
	s, _ := newBootstrapServer(t, Config{}, "")
	rec := bootstrapPost(t, s, "uci_anything", "")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, "invalid or already used initial token\n", rec.Body.String())
}

func TestBootstrapRateLimit(t *testing.T) {
	s, _ := newBootstrapServer(t, Config{}, "uci_right")
	for i := 0; i < 5; i++ {
		rec := bootstrapPost(t, s, "uci_wrong", "")
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	}
	rec := bootstrapPost(t, s, "uci_right", "")
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "too many bootstrap attempts\n", rec.Body.String())
}

func TestBootstrapRemovesOwnTokenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, InitialTokenFileName)
	require.NoError(t, os.WriteFile(path, []byte("uci_right"), 0o600))
	s, _ := newBootstrapServer(t, Config{InitialTokenDirs: []string{dir}}, "uci_right")
	rec := bootstrapPost(t, s, "uci_right", "ci-admin")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	_, err := os.Stat(path)
	assert.True(t, os.IsNotExist(err))
}

func TestBootstrapRejectsReservedName(t *testing.T) {
	s, pg := newBootstrapServer(t, Config{}, "uci_right")
	rec := bootstrapPost(t, s, "uci_right", "env:x")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	// The token was not consumed.
	st, err := pg.GetBootstrapState(t.Context())
	require.NoError(t, err)
	assert.False(t, st.Done)
}

func bootstrapPostFrom(t testing.TB, s *Server, remoteAddr, initial string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"initialToken": initial})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/bootstrap", bytes.NewReader(body))
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec
}

func TestBootstrapRateLimitIsPerIP(t *testing.T) {
	s, _ := newBootstrapServer(t, Config{}, "uci_right")
	for i := 0; i < 5; i++ {
		rec := bootstrapPostFrom(t, s, "198.51.100.1:4000", "uci_wrong")
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	}
	rec := bootstrapPostFrom(t, s, "198.51.100.1:4001", "uci_wrong")
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)

	// Another client address has its own budget.
	rec = bootstrapPostFrom(t, s, "198.51.100.2:4000", "uci_right")
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

func TestBootstrapRefusedWhileAdminDisabled(t *testing.T) {
	s, pg := newBootstrapServer(t, Config{}, "uci_right")
	s.cfg.AdminEnabled = false

	rec := bootstrapPost(t, s, "uci_right", "")
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, "the built-in admin account is disabled; enable it or use the recovery command\n", rec.Body.String())
	// The token was not consumed.
	st, err := pg.GetBootstrapState(t.Context())
	require.NoError(t, err)
	assert.False(t, st.Done)

	// A running recovery lease counts as enabled (drop the cached lease read).
	s.adminLease.valid = false
	require.NoError(t, pg.SetAdminEnabledUntil(t.Context(), time.Now().Add(time.Hour)))
	rec = bootstrapPost(t, s, "uci_right", "")
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}
