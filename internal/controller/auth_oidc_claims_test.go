package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/authz"
)

const overageLogMessage = "oidc: the identity provider reported a groups overage; group-based roles and permissions are not applied for this login (see identity-providers.md \"Entra ID\")"

// overageWarnings returns the captured JSON log records carrying the exact overage message.
func overageWarnings(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec))
		if rec["msg"] == overageLogMessage {
			out = append(out, rec)
		}
	}
	return out
}

// captureLogs routes the default slog logger into a buffer for the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestExtractRoleValues_NestedPath(t *testing.T) {
	assert.Equal(t, []string{"admin"},
		extractRoleValues(map[string]any{"realm_access": map[string]any{"roles": []any{"admin"}}}, "realm_access.roles"))
	assert.Equal(t, []string{"dev"},
		extractRoleValues(map[string]any{"https://example.com/roles": []any{"dev"}}, "https://example.com/roles"))
	// The exact top-level key wins over a nested path.
	assert.Equal(t, []string{"exact"},
		extractRoleValues(map[string]any{
			"a.b": []any{"exact"},
			"a":   map[string]any{"b": []any{"nested"}},
		}, "a.b"))
	// Missing segment, object leaf, and non-object intermediate give nil.
	assert.Nil(t, extractRoleValues(map[string]any{"realm_access": map[string]any{}}, "realm_access.roles"))
	assert.Nil(t, extractRoleValues(map[string]any{"realm_access": map[string]any{"roles": map[string]any{"x": 1}}}, "realm_access.roles"))
	assert.Nil(t, extractRoleValues(map[string]any{"realm_access": "str"}, "realm_access.roles"))
	// A string leaf is a single value.
	assert.Equal(t, []string{"solo"},
		extractRoleValues(map[string]any{"realm_access": map[string]any{"roles": "solo"}}, "realm_access.roles"))
	// The default is unchanged.
	assert.Equal(t, []string{"x"}, extractRoleValues(map[string]any{"groups": []any{"x"}}, ""))
}

func newClaimsServer(rolesClaim string) *Server {
	return &Server{oidcCfg: &OIDCConfig{RolesClaim: rolesClaim}}
}

func TestOIDCIdentity_EmailVerified(t *testing.T) {
	s := newClaimsServer("groups")
	cases := map[string]struct {
		extra     map[string]any
		wantEmail string
	}{
		"false bool":    {map[string]any{"email_verified": false}, "sub-1"},
		"false string":  {map[string]any{"email_verified": "false"}, "sub-1"},
		"true bool":     {map[string]any{"email_verified": true}, "u@example.com"},
		"true string":   {map[string]any{"email_verified": "true"}, "u@example.com"},
		"absent":        {nil, "u@example.com"},
		"number is ign": {map[string]any{"email_verified": 1}, "u@example.com"},
		"object is ign": {map[string]any{"email_verified": map[string]any{}}, "u@example.com"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			claims := map[string]any{"sub": "sub-1", "email": "u@example.com"}
			for k, v := range tc.extra {
				claims[k] = v
			}
			sub, email, _ := oidcIdentity(s, claims)
			assert.Equal(t, "sub-1", sub)
			assert.Equal(t, tc.wantEmail, email)
		})
	}
}

func TestAdmission_EmailVerifiedFalseDoesNotMatchUserMap(t *testing.T) {
	s, _ := newAuthzTestServer(t, authz.Legacy{
		UserMap:     map[string]string{"u@example.com": "admin"},
		DefaultRole: "deny",
	}, "")
	ctx := context.Background()

	status, _ := s.admitOIDCClaims(ctx, map[string]any{"sub": "sub-1", "email": "u@example.com", "email_verified": false})
	assert.Equal(t, http.StatusForbidden, status)
	status, _ = s.admitOIDCClaims(ctx, map[string]any{"sub": "sub-1", "email": "u@example.com", "email_verified": "false"})
	assert.Equal(t, http.StatusForbidden, status)
	status, msg := s.admitOIDCClaims(ctx, map[string]any{"sub": "sub-1", "email": "u@example.com", "email_verified": true})
	assert.Equal(t, 0, status, msg)
	status, msg = s.admitOIDCClaims(ctx, map[string]any{"sub": "sub-1", "email": "u@example.com"})
	assert.Equal(t, 0, status, msg)

	// The same entry keyed by sub still admits an unverified email.
	s2, _ := newAuthzTestServer(t, authz.Legacy{
		UserMap:     map[string]string{"sub-1": "admin"},
		DefaultRole: "deny",
	}, "")
	status, msg = s2.admitOIDCClaims(ctx, map[string]any{"sub": "sub-1", "email": "u@example.com", "email_verified": false})
	assert.Equal(t, 0, status, msg)
}

func TestAdmission_EmailVerifiedFalseDoesNotMatchPermissionsUsers(t *testing.T) {
	policy := `
folders:
  team-a:
    users:
      "u@example.com": developer
`
	s, _ := newAuthzTestServer(t, authz.Legacy{DefaultRole: "deny"}, policy)
	ctx := context.Background()
	status, msg := s.admitOIDCClaims(ctx, map[string]any{"sub": "sub-1", "email": "u@example.com"})
	require.Equal(t, 0, status, msg)
	status, _ = s.admitOIDCClaims(ctx, map[string]any{"sub": "sub-1", "email": "u@example.com", "email_verified": false})
	assert.Equal(t, http.StatusForbidden, status)
}

func TestBearer_EmailVerifiedFalseDoesNotMatchUserMap(t *testing.T) {
	s, _ := newAuthzTestServer(t, authz.Legacy{
		UserMap:     map[string]string{"u@example.com": "admin"},
		DefaultRole: "deny",
	}, "")
	s.verifyIDTokenHook = func(context.Context, string, string) (map[string]any, error) {
		return map[string]any{"sub": "sub-1", "email": "u@example.com", "email_verified": false}, nil
	}
	_, rec := authenticate(t, s, "tok", "")
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func overageClaims(marker string) map[string]any {
	c := map[string]any{"sub": "sub-ov", "email": "secret-user@example.com"}
	switch marker {
	case "names":
		c["_claim_names"] = map[string]any{"groups": "src1"}
		c["_claim_sources"] = map[string]any{"src1": map[string]any{"endpoint": "https://graph.example.com/x"}}
	case "hasgroups":
		c["hasgroups"] = true
	}
	return c
}

func TestOIDCIdentity_GroupsOverage(t *testing.T) {
	for _, marker := range []string{"names", "hasgroups"} {
		t.Run(marker, func(t *testing.T) {
			buf := captureLogs(t)
			s := newClaimsServer("groups")
			sub, _, values := oidcIdentity(s, overageClaims(marker))
			assert.Equal(t, "sub-ov", sub)
			assert.Empty(t, values)
			out := buf.String()
			recs := overageWarnings(t, buf)
			require.Len(t, recs, 1, out)
			assert.Equal(t, "WARN", recs[0]["level"])
			assert.Equal(t, "sub-ov", recs[0]["sub"])
			assert.Equal(t, "groups", recs[0]["rolesClaim"])
			assert.NotContains(t, out, "secret-user@example.com")
			assert.NotContains(t, out, "graph.example.com")
		})
	}
}

func TestOIDCIdentity_OverageMarkerWithGroupsPresentNoWarning(t *testing.T) {
	buf := captureLogs(t)
	s := newClaimsServer("groups")
	claims := overageClaims("names")
	claims["groups"] = []any{"g1"}
	_, _, values := oidcIdentity(s, claims)
	assert.Equal(t, []string{"g1"}, values)
	assert.Empty(t, buf.String())

	// No marker, no groups: nothing to report either.
	_, _, values = oidcIdentity(s, map[string]any{"sub": "s"})
	assert.Empty(t, values)
	assert.Empty(t, buf.String())

	// An unrelated _claim_names entry is not an overage marker.
	_, _, _ = oidcIdentity(s, map[string]any{"sub": "s", "_claim_names": map[string]any{"other": "src1"}})
	assert.Empty(t, buf.String())
}

func TestOIDCIdentity_OverageTiedToRolesClaim(t *testing.T) {
	buf := captureLogs(t)
	// roles claim "roles": hasgroups and a groups marker are not about it.
	s := newClaimsServer("roles")
	oidcIdentity(s, overageClaims("hasgroups"))
	oidcIdentity(s, overageClaims("names"))
	assert.Empty(t, buf.String())
	// A marker naming the configured claim does warn.
	oidcIdentity(s, map[string]any{"sub": "s", "_claim_names": map[string]any{"roles": "src1"}})
	assert.Len(t, overageWarnings(t, buf), 1)
	// Empty rolesClaim means groups.
	buf.Reset()
	oidcIdentity(newClaimsServer(""), overageClaims("hasgroups"))
	assert.Len(t, overageWarnings(t, buf), 1)
}

func TestOverageLimiter_Bounded(t *testing.T) {
	var l overageLimiter
	now := time.Now()
	for i := 0; i < overageLimiterMax+10; i++ {
		assert.True(t, l.allow(fmt.Sprintf("s%d", i), now))
	}
	assert.Len(t, l.last, overageLimiterMax)
	// Recorded subjects are limited; unrecorded ones are still allowed.
	assert.False(t, l.allow("s0", now.Add(time.Minute)))
	assert.True(t, l.allow("s"+fmt.Sprint(overageLimiterMax+5), now.Add(time.Minute)))
	// Expired entries are pruned to make room.
	assert.True(t, l.allow("fresh", now.Add(overageWarnInterval+time.Second)))
	assert.False(t, l.allow("fresh", now.Add(overageWarnInterval+2*time.Second)))
}

func TestCompleteOIDCLogin_OverageDoesNotFailLogin(t *testing.T) {
	buf := captureLogs(t)
	s, _ := newAuthzTestServer(t, authz.Legacy{DefaultRole: "viewer"}, "")
	_, _, status, msg := s.completeOIDCLogin(context.Background(), overageClaims("names"), "rt")
	require.Equal(t, 0, status, msg)
	assert.Len(t, overageWarnings(t, buf), 1, "one warning per login, not one per evaluation")
	assert.NotContains(t, buf.String(), "secret-user@example.com")
}

func TestRefreshedClaims_OverageWarnsAndKeepsStoredClaims(t *testing.T) {
	buf := captureLogs(t)
	s, _ := newAuthzTestServer(t, authz.Legacy{}, "")
	s.verifyIDTokenHook = func(context.Context, string, string) (map[string]any, error) {
		return overageClaims("hasgroups"), nil
	}
	got, ok := s.refreshedClaims(context.Background(), "x")
	assert.False(t, ok)
	assert.Nil(t, got)
	assert.Len(t, overageWarnings(t, buf), 1)
}

func TestRefreshedClaims_NestedRolesClaim(t *testing.T) {
	s, _ := newAuthzTestServer(t, authz.Legacy{}, "")
	s.oidcCfg.RolesClaim = "realm_access.roles"
	s.verifyIDTokenHook = func(context.Context, string, string) (map[string]any, error) {
		return map[string]any{"realm_access": map[string]any{"roles": []any{"r1"}}}, nil
	}
	got, ok := s.refreshedClaims(context.Background(), "x")
	assert.True(t, ok)
	assert.Equal(t, []string{"r1"}, got)
}

func TestBearer_OverageWarningIsRateLimitedPerSub(t *testing.T) {
	buf := captureLogs(t)
	s, _ := newAuthzTestServer(t, authz.Legacy{DefaultRole: "viewer"}, "")
	s.verifyIDTokenHook = func(context.Context, string, string) (map[string]any, error) {
		return overageClaims("names"), nil
	}
	for i := 0; i < 3; i++ {
		_, rec := authenticate(t, s, "tok", "")
		require.Equal(t, http.StatusOK, rec.Code)
	}
	assert.Len(t, overageWarnings(t, buf), 1, buf.String())
	assert.NotContains(t, buf.String(), "secret-user@example.com")
}
