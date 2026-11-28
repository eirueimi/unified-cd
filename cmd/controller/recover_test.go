package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/controller"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/eirueimi/unified-cd/internal/store/storetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// execRecover runs `admin recover` with args and returns the exit code and the
// captured stdout and stderr.
func execRecover(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runAdmin(append([]string{"recover"}, args...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "controller.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func openStore(t *testing.T, dsn string) *store.Postgres {
	t.Helper()
	pg, err := store.NewPostgres(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(pg.Close)
	return pg
}

func TestRecoverPrintsWorkingAdminPAT(t *testing.T) {
	dsn := storetest.NewEmptyTestDatabase(t)
	before := time.Now()
	code, stdout, stderr := execRecover(t, "--dsn", dsn)
	require.Equal(t, 0, code, stderr)

	token := strings.TrimSpace(stdout)
	assert.True(t, strings.HasPrefix(token, "exc_"), "stdout must hold only the token, got %q", stdout)
	assert.Equal(t, token+"\n", stdout)
	assert.NotEmpty(t, stderr)
	assert.NotContains(t, stderr, token)

	pg := openStore(t, dsn)
	pat, err := pg.GetPATByHash(t.Context(), controller.HashToken(token))
	require.NoError(t, err)
	require.NotNil(t, pat)
	assert.True(t, strings.HasPrefix(pat.Name, "recovery-"), pat.Name)
	assert.Equal(t, "recovery", pat.CreatedBy)
	require.NotNil(t, pat.ExpiresAt)
	assert.WithinDuration(t, before.Add(time.Hour), *pat.ExpiresAt, time.Minute)

	srv := controller.NewServer(controller.Config{AdminEnabled: true}, pg)
	t.Cleanup(srv.Close)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/authz/policy", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	logs, err := pg.ListAuditLogs(t.Context(), 10, 0)
	require.NoError(t, err)
	var found bool
	for _, l := range logs {
		if l.Action != "admin.recover" {
			continue
		}
		found = true
		assert.Equal(t, "recovery", l.Actor)
		assert.Equal(t, "recovery", l.ActorID)
		assert.JSONEq(t, `{"patId":"`+pat.ID+`","ttl":"1h0m0s","enableAdmin":false}`, string(l.Details))
	}
	assert.True(t, found, "audit row admin.recover not found")
}

func TestRecoverTTLCap(t *testing.T) {
	code, stdout, stderr := execRecover(t, "--dsn", "postgres://unused", "--ttl", "25h")
	assert.Equal(t, 2, code)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "--ttl must not exceed 24h")
}

func TestRecoverDisabledAdmin(t *testing.T) {
	dsn := storetest.NewEmptyTestDatabase(t)
	cfg := writeConfig(t, "serviceAccounts:\n  admin:\n    enabled: false\n")

	code, stdout, stderr := execRecover(t, "-f", cfg, "--dsn", dsn)
	assert.Equal(t, 1, code)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "the built-in admin account is disabled in the configuration; pass --enable-admin")

	before := time.Now()
	code, stdout, stderr = execRecover(t, "-f", cfg, "--dsn", dsn, "--enable-admin", "--ttl", "30m")
	require.Equal(t, 0, code, stderr)
	token := strings.TrimSpace(stdout)

	pg := openStore(t, dsn)
	pat, err := pg.GetPATByHash(t.Context(), controller.HashToken(token))
	require.NoError(t, err)
	require.NotNil(t, pat)
	require.NotNil(t, pat.ExpiresAt)
	state, err := pg.GetBootstrapState(t.Context())
	require.NoError(t, err)
	require.NotNil(t, state.AdminEnabledUntil)
	assert.WithinDuration(t, *pat.ExpiresAt, *state.AdminEnabledUntil, time.Millisecond)
	assert.WithinDuration(t, before.Add(30*time.Minute), *state.AdminEnabledUntil, time.Minute)

	// The server built from the disabled config accepts the PAT through the lease.
	srv := controller.NewServer(controller.Config{AdminEnabled: false}, pg)
	t.Cleanup(srv.Close)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/authz/policy", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestRecoverUnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	assert.Equal(t, 2, runAdmin([]string{"nope"}, &stdout, &stderr))
	assert.Equal(t, 2, runAdmin(nil, &stdout, &stderr))
}

func TestServerStartArgsUnchanged(t *testing.T) {
	cfg := writeConfig(t, "dsn: postgres://from-file\naddr: \":9999\"\n")

	fs := flag.NewFlagSet("controller", flag.ContinueOnError)
	eff, err := loadRuntimeConfig([]string{"-f", cfg}, fs)
	require.NoError(t, err)
	assert.Equal(t, "postgres://from-file", eff.DSN)
	assert.Equal(t, ":9999", eff.Addr)

	fs = flag.NewFlagSet("controller", flag.ContinueOnError)
	eff, err = loadRuntimeConfig([]string{"-f", cfg, "--dsn", "postgres://flag", "--addr", ":7777"}, fs)
	require.NoError(t, err)
	assert.Equal(t, "postgres://flag", eff.DSN)
	assert.Equal(t, ":7777", eff.Addr)

	fs = flag.NewFlagSet("controller", flag.ContinueOnError)
	eff, err = loadRuntimeConfig(nil, fs)
	require.NoError(t, err)
	assert.Equal(t, ":8080", eff.Addr)
}

// failingStore wraps a store and fails the selected recovery side effects.
type failingStore struct {
	store.Store
	failAudit, failLease bool
}

func (f failingStore) InsertAuditLog(ctx context.Context, e store.AuditEntry) error {
	if f.failAudit {
		return errors.New("audit boom")
	}
	return f.Store.InsertAuditLog(ctx, e)
}

func (f failingStore) SetAdminEnabledUntil(ctx context.Context, t time.Time) error {
	if f.failLease {
		return errors.New("lease boom")
	}
	return f.Store.SetAdminEnabledUntil(ctx, t)
}

func TestRecoverFailureLeavesNoPATAndNoLease(t *testing.T) {
	for name, fs := range map[string]failingStore{
		"audit fails": {failAudit: true},
		"lease fails": {failLease: true},
	} {
		t.Run(name, func(t *testing.T) {
			pg := storetest.NewTestPostgres(t)
			fs.Store = pg
			before, err := pg.GetBootstrapState(t.Context())
			require.NoError(t, err)

			token, _, err := recoverAdminPAT(t.Context(), fs, time.Now().UTC(), time.Hour, true)
			require.Error(t, err)
			assert.Empty(t, token)

			// No PAT of the admin account survives, so nothing can authenticate.
			accounts, err := pg.ListAccounts(t.Context())
			require.NoError(t, err)
			for _, a := range accounts {
				pats, err := pg.ListPATsByAccount(t.Context(), a.ID)
				require.NoError(t, err)
				assert.Empty(t, pats, "account %s", a.Name)
			}
			after, err := pg.GetBootstrapState(t.Context())
			require.NoError(t, err)
			assert.Equal(t, before.AdminEnabledUntil, after.AdminEnabledUntil)
		})
	}
}
