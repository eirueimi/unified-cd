package controller

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/eirueimi/unified-cd/internal/store/storetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// auditedDelete issues an audited DELETE (vars.delete) with the given
// credentials and returns the newest audit row.
func auditedDelete(t *testing.T, s *Server, pg store.Store, setAuth func(*http.Request)) api.AuditLog {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/vars/probe", nil)
	setAuth(req)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.NotEqual(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	logs, err := pg.ListAuditLogs(t.Context(), 1, 0)
	require.NoError(t, err)
	require.Len(t, logs, 1)
	return logs[0]
}

func bearerAuth(tok string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}

func TestAudit_ActorIDBreakGlass(t *testing.T) {
	pg := storetest.NewTestPostgres(t)
	_, err := pg.UpsertBootstrapPAT(t.Context(), BootstrapPATName, HashToken("secret"))
	require.NoError(t, err)
	s := NewServer(Config{Token: "secret"}, pg)
	t.Cleanup(s.Close)

	row := auditedDelete(t, s, pg, bearerAuth("secret"))
	assert.Equal(t, "break-glass", row.ActorID)
	assert.Empty(t, row.ActorSub)
}

// TestAudit_SamePATNameDistinguishable is the spoofing case: a user names their
// PAT like the bootstrap token and like another PAT. The display name is
// identical, the stable actor id is not.
func TestAudit_SamePATNameDistinguishable(t *testing.T) {
	pg := storetest.NewTestPostgres(t)
	_, err := pg.UpsertBootstrapPAT(t.Context(), BootstrapPATName, HashToken("secret"))
	require.NoError(t, err)
	s := NewServer(Config{Token: "secret"}, pg)
	t.Cleanup(s.Close)

	a, err := pg.CreatePAT(t.Context(), "ci", HashToken("tok-a"), "admin", nil)
	require.NoError(t, err)
	b, err := pg.CreatePAT(t.Context(), "ci", HashToken("tok-b"), "admin", nil)
	require.NoError(t, err)
	imposter, err := pg.CreatePAT(t.Context(), BootstrapPATName, HashToken("tok-c"), "admin", nil)
	require.NoError(t, err)

	rowA := auditedDelete(t, s, pg, bearerAuth("tok-a"))
	rowB := auditedDelete(t, s, pg, bearerAuth("tok-b"))
	rowC := auditedDelete(t, s, pg, bearerAuth("tok-c"))
	rowBG := auditedDelete(t, s, pg, bearerAuth("secret"))

	assert.Equal(t, "ci", rowA.Actor)
	assert.Equal(t, "ci", rowB.Actor)
	assert.Equal(t, "pat:"+a.ID, rowA.ActorID)
	assert.Equal(t, "pat:"+b.ID, rowB.ActorID)
	assert.NotEqual(t, rowA.ActorID, rowB.ActorID)

	assert.Equal(t, BootstrapPATName, rowC.Actor)
	assert.Equal(t, BootstrapPATName, rowBG.Actor)
	assert.Equal(t, "pat:"+imposter.ID, rowC.ActorID)
	assert.Equal(t, "break-glass", rowBG.ActorID)
}

func TestAudit_ActorIDOwnerPATAndSession(t *testing.T) {
	pg := storetest.NewTestPostgres(t)
	s := NewServer(Config{}, pg)
	t.Cleanup(s.Close)

	pat, err := pg.CreateOwnedPAT(t.Context(), store.NewOwnedPAT{
		Name: "mine", TokenHash: HashToken("tok-owned"), Role: "admin",
		Owner: store.PATOwner{Sub: "sub-1", Email: "u@example.com"},
	})
	require.NoError(t, err)
	row := auditedDelete(t, s, pg, bearerAuth("tok-owned"))
	assert.Equal(t, "pat:"+pat.ID, row.ActorID)
	assert.Equal(t, "sub-1", row.ActorSub)

	_, err = pg.CreateSession(t.Context(), HashToken("sess"), "sub-2", "v@example.com", "admin", nil, []byte("dek"), []byte("ct"), time.Now().Add(time.Hour))
	require.NoError(t, err)
	row = auditedDelete(t, s, pg, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "sess"})
	})
	assert.Equal(t, "oidc:sub-2", row.ActorID)
	assert.Equal(t, "sub-2", row.ActorSub)
	assert.Equal(t, "v@example.com", row.Actor)
}

func TestPrincipal_AuditActorID(t *testing.T) {
	assert.Equal(t, "break-glass", Principal{Kind: "pat", PATID: "x", BreakGlass: true}.AuditActorID())
	assert.Equal(t, "pat:x", Principal{Kind: "pat", PATID: "x"}.AuditActorID())
	assert.Equal(t, "oidc:s", Principal{Kind: "oidc", Sub: "s"}.AuditActorID())
	assert.Equal(t, "oidc:s", Principal{Kind: "session", Sub: "s"}.AuditActorID())
	assert.Equal(t, "", Principal{Kind: "oidc"}.AuditActorID())
}

type captureAudit struct{ entries []store.AuditEntry }

func (c *captureAudit) InsertAuditLog(_ context.Context, e store.AuditEntry) error {
	c.entries = append(c.entries, e)
	return nil
}

func resetAuthFailureLimiter(t *testing.T) {
	t.Helper()
	authFailureBuckets.Range(func(k, _ any) bool { authFailureBuckets.Delete(k); return true })
	t.Cleanup(func() { authFailureBuckets.Range(func(k, _ any) bool { authFailureBuckets.Delete(k); return true }) })
}

func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func TestReportAuthFailure_LogsAndAuditsWithoutToken(t *testing.T) {
	resetAuthFailureLimiter(t)
	buf := captureSlog(t)
	cap := &captureAudit{}

	const secretToken = "super-secret-token-value"
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
	req.RemoteAddr = "203.0.113.7:51234"
	req.Header.Set("Authorization", "Bearer "+secretToken)
	h := ServerAuth(nil, nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	// ServerAuth with a nil store cannot audit; call the reporter directly for the audit row.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	reportAuthFailure(cap, req, "bearer", "invalid_token")

	out := buf.String()
	assert.Contains(t, out, "authentication failed")
	assert.Contains(t, out, "remote=203.0.113.7")
	assert.Contains(t, out, "method=bearer")
	assert.Contains(t, out, "reason=invalid_token")
	assert.NotContains(t, out, secretToken)

	require.Len(t, cap.entries, 1)
	e := cap.entries[0]
	assert.Equal(t, "auth.failed", e.Action)
	assert.Equal(t, http.StatusUnauthorized, e.Status)
	assert.Contains(t, e.Resource, "203.0.113.7")
	assert.False(t, strings.Contains(e.Resource+e.Path+e.Actor, secretToken))
}

func TestServerAuth_401WritesAuditRow(t *testing.T) {
	resetAuthFailureLimiter(t)
	pg := storetest.NewTestPostgres(t)
	s := NewServer(Config{}, pg)
	t.Cleanup(s.Close)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
	req.RemoteAddr = "198.51.100.9:1111"
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	logs, err := pg.ListAuditLogs(t.Context(), 5, 0)
	require.NoError(t, err)
	require.Len(t, logs, 1)
	assert.Equal(t, "auth.failed", logs[0].Action)
	assert.Equal(t, 401, logs[0].Status)
	assert.Contains(t, logs[0].Resource, "reason=invalid_token")
	assert.Contains(t, logs[0].Resource, "198.51.100.9")
	assert.NotContains(t, logs[0].Resource, "not-a-real-token")
}

func TestReportAuthFailure_RateLimited(t *testing.T) {
	resetAuthFailureLimiter(t)
	captureSlog(t)
	cap := &captureAudit{}
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	for i := 0; i < authFailureBurst*3; i++ {
		reportAuthFailure(cap, req, "bearer", "invalid_token")
	}
	assert.LessOrEqual(t, len(cap.entries), authFailureBurst+2)
	assert.Greater(t, authFailureBucketFor("bearer").suppressed.Load(), int64(0))

	// A flood of one method must not crowd out another, and "none" is log-only.
	reportAuthFailure(cap, req, "session", "invalid_or_expired_session")
	assert.Equal(t, "session", strings.Fields(strings.TrimPrefix(cap.entries[len(cap.entries)-1].Resource, "method="))[0])
	n := len(cap.entries)
	reportAuthFailure(cap, req, "none", "missing_credentials")
	assert.Len(t, cap.entries, n)
}

func TestAgentAuth_401IsLogged(t *testing.T) {
	resetAuthFailureLimiter(t)
	buf := captureSlog(t)
	s := &Server{}
	h := s.agentAuth(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/a/claim", nil)
	req.RemoteAddr = "192.0.2.5:4000"
	req.Header.Set("Authorization", "Bearer uca_not-a-valid-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	out := buf.String()
	assert.Contains(t, out, "method=agent-access")
	assert.Contains(t, out, "remote=192.0.2.5")
	assert.NotContains(t, out, "uca_not-a-valid-token")
}

func TestAudit_EnrollmentExchangeActorID(t *testing.T) {
	pg := storetest.NewTestPostgres(t)
	s := NewServer(Config{}, pg)
	t.Cleanup(s.Close)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/enroll", nil)
	s.auditAgentCredentialAs(req, "k8s:c:ns:uid", "agent.enrollment.exchange", "pol", http.StatusOK)
	logs, err := pg.ListAuditLogs(t.Context(), 1, 0)
	require.NoError(t, err)
	require.Len(t, logs, 1)
	assert.Equal(t, "agent:k8s:c:ns:uid", logs[0].ActorID)
}
