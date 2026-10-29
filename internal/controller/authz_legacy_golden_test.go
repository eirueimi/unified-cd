package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/stretchr/testify/require"
)

// legacyMinRole is frozen from the minimum-role route registrations
// in server.go before matrix authorization (spec section 7.2, "Today" column).
// "" means any authenticated human.
var legacyMinRole = map[string]string{
	"GET /api/v1/audit": "admin",

	"POST /api/v1/jobs":     "developer",
	"GET /api/v1/jobs":      "viewer",
	"GET /api/v1/jobs/*":    "viewer",
	"DELETE /api/v1/jobs/*": "developer",

	"POST /api/v1/runs":                               "developer",
	"POST /api/v1/runs/{id}/replay":                   "developer",
	"GET /api/v1/runs":                                "viewer",
	"GET /api/v1/runs/active":                         "viewer",
	"GET /api/v1/runs/{id}":                           "viewer",
	"GET /api/v1/runs/{id}/outputs":                   "viewer",
	"GET /api/v1/runs/{id}/yaml":                      "viewer",
	"POST /api/v1/runs/{id}/cancel":                   "developer",
	"DELETE /api/v1/runs/{id}":                        "developer",
	"GET /api/v1/runs/{id}/events":                    "viewer",
	"GET /api/v1/runs/{id}/logs":                      "viewer",
	"GET /api/v1/runs/{id}/steps":                     "viewer",
	"GET /api/v1/runs/{id}/logs/archive":              "viewer",
	"GET /api/v1/runs/{id}/logs/stats":                "viewer",
	"GET /api/v1/runs/{id}/logs/range":                "viewer",
	"GET /api/v1/runs/{id}/logs/search":               "viewer",
	"GET /api/v1/runs/{runID}/approvals":              "viewer",
	"POST /api/v1/runs/{runID}/approvals/{stepIndex}": "developer",
	"GET /api/v1/runs/{runID}/artifacts":              "",
	"GET /api/v1/runs/{runID}/artifacts/{name}":       "",

	"POST /api/v1/secrets":                 "admin",
	"GET /api/v1/secrets":                  "developer",
	"DELETE /api/v1/secrets/{name}":        "admin",
	"POST /api/v1/gitcredentials":          "admin",
	"GET /api/v1/gitcredentials":           "admin",
	"DELETE /api/v1/gitcredentials/{name}": "admin",

	"POST /api/v1/tokens":        "developer",
	"GET /api/v1/tokens":         "developer",
	"DELETE /api/v1/tokens/{id}": "developer",

	"POST /api/v1/agent-enrollments":                             "admin",
	"GET /api/v1/agent-enrollments":                              "viewer",
	"DELETE /api/v1/agent-enrollments/{id}":                      "admin",
	"POST /api/v1/agent-enrollment-policies":                     "admin",
	"GET /api/v1/agent-enrollment-policies":                      "viewer",
	"GET /api/v1/agent-enrollment-policies/{name}":               "viewer",
	"PUT /api/v1/agent-enrollment-policies/{name}":               "admin",
	"DELETE /api/v1/agent-enrollment-policies/{name}":            "admin",
	"GET /api/v1/agent-identities/{agentId}":                     "viewer",
	"POST /api/v1/agent-identities/{agentId}/enable":             "admin",
	"POST /api/v1/agent-identities/{agentId}/disable":            "admin",
	"POST /api/v1/agent-identities/{agentId}/credentials/revoke": "admin",
	"GET /api/v1/agents":                                         "viewer",
	"GET /api/v1/agents/{agentId}":                               "viewer",
	"GET /api/v1/agents/{agentId}/runs":                          "viewer",

	"POST /api/v1/webhooks":          "admin",
	"GET /api/v1/webhooks":           "viewer",
	"DELETE /api/v1/webhooks/{name}": "admin",
	"POST /api/v1/vars":              "admin",
	"GET /api/v1/vars":               "viewer",
	"DELETE /api/v1/vars/{name}":     "admin",

	"POST /api/v1/schedules":          "developer",
	"GET /api/v1/schedules":           "viewer",
	"DELETE /api/v1/schedules/{name}": "developer",

	"POST /api/v1/appsources":             "admin",
	"GET /api/v1/appsources":              "viewer",
	"GET /api/v1/appsources/{name}":       "viewer",
	"DELETE /api/v1/appsources/{name}":    "admin",
	"POST /api/v1/appsources/{name}/sync": "admin",
}

// legacyExempt lists the permitted behaviour differences (spec sections 5.6,
// 5.14, 5.15): token management is narrowed and PAT creation refuses an
// over-high role instead of clamping it.
var legacyExempt = map[string]bool{
	"POST /api/v1/tokens":        true,
	"GET /api/v1/tokens":         true,
	"DELETE /api/v1/tokens/{id}": true,
}

// legacyNewRoutes are human routes that did not exist before this work
// (section 5.18). Task 10 adds its two rows here.
var legacyNewRoutes = map[string]bool{}

// isHumanRow reports whether a row is decided by a permission (as opposed to
// public, self-authenticating and agent-only routes).
func isHumanRow(r routeRule) bool {
	return r.Scope != scopePublic && r.Scope != scopeSelf && r.Scope != scopeAgent
}

func TestLegacyEquivalence_RouteTableAndBundles(t *testing.T) {
	humanRows := map[string]bool{}
	for _, row := range routeTable {
		if !isHumanRow(row) {
			continue
		}
		key := row.Method + " " + row.Pattern
		humanRows[key] = true
		min, frozen := legacyMinRole[key]
		if !frozen {
			require.Truef(t, legacyNewRoutes[key], "%s is a human route with no frozen legacy decision", key)
			continue
		}
		if legacyExempt[key] {
			continue
		}
		for _, role := range []string{"viewer", "developer", "admin"} {
			bundle, ok := authz.Bundle(role)
			require.True(t, ok)
			// A row is satisfied by Perm or, when set, AltPerm.
			got := bundle.Has(row.Perm) || (row.AltPerm != "" && bundle.Has(row.AltPerm))
			want := roleRank(role) >= roleRank(min)
			require.Equalf(t, want, got, "%s as %s: bundle decision differs from legacy minimum role %q", key, role, min)
		}
	}
	for key := range legacyMinRole {
		require.Truef(t, humanRows[key], "frozen legacy row %s is not a human row of the route table", key)
	}
}

// placeholderPath fills a route pattern with values that satisfy the router
// without needing the objects to exist.
func placeholderPath(pattern string) string {
	return strings.NewReplacer(
		"{id}", "00000000-0000-0000-0000-000000000000",
		"{runID}", "00000000-0000-0000-0000-000000000000",
		"{name}", "x",
		"{agentId}", "x",
		"{stepIndex}", "0",
		"*", "x",
	).Replace(pattern)
}

func TestLegacyEquivalence_HTTP(t *testing.T) {
	s, pg := newTestServer(t)
	roles := []string{"viewer", "developer", "admin"}
	for _, role := range roles {
		makeRolePAT(t, pg, "golden_"+role, role)
	}

	for key, min := range legacyMinRole {
		if legacyExempt[key] {
			continue
		}
		method, pattern, _ := strings.Cut(key, " ")
		path := placeholderPath(pattern)
		for _, role := range roles {
			// Every request gets a short deadline so a streaming handler (the
			// SSE route streams without checking the run exists) cannot hang
			// the test; a stream that starts counts as admitted.
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			var req *http.Request
			if method == http.MethodPost || method == http.MethodPut {
				req = httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(`{}`))
				req.Header.Set("Content-Type", "application/json")
			} else {
				req = httptest.NewRequestWithContext(ctx, method, path, nil)
			}
			req.Header.Set("Authorization", "Bearer golden_"+role)
			rec := httptest.NewRecorder()
			s.Router().ServeHTTP(rec, req)
			cancel()

			wantForbidden := roleRank(role) < roleRank(min)
			require.Equalf(t, wantForbidden, rec.Code == http.StatusForbidden,
				"%s as %s: status %d, body %q", key, role, rec.Code, rec.Body.String())
		}
	}
}
