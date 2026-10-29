package controller

import (
	"net/http"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
)

// walkRoutes returns every route registered on a server, keyed
// "METHOD normalizedPattern".
func walkRoutes(t *testing.T) map[string]bool {
	t.Helper()
	s, _ := newTestServerWithConfig(t, Config{WebDir: t.TempDir()})
	routes := map[string]bool{}
	err := chi.Walk(s.Router().(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		routes[method+" "+normalizeRoutePattern(route)] = true
		return nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, routes)
	return routes
}

func TestEveryRegisteredRouteIsClassified(t *testing.T) {
	for key := range walkRoutes(t) {
		m, p, _ := strings.Cut(key, " ")
		_, ok := lookupRoute(m, p)
		require.Truef(t, ok, "%s has no permission: classify it in the route table", key)
	}
}

func TestEveryRouteTableEntryIsRegistered(t *testing.T) {
	routes := walkRoutes(t)
	for _, row := range routeTable {
		found := false
		for key := range routes {
			m, p, _ := strings.Cut(key, " ")
			if p == row.Pattern && (row.Method == mAny || row.Method == m) {
				found = true
				break
			}
		}
		require.Truef(t, found, "route table row %s %s is not registered", row.Method, row.Pattern)
	}
}

func TestRouteTableShape(t *testing.T) {
	// Scopes whose permission is checked against a job.
	jobScopes := map[routeScope]bool{
		scopeJobPath: true, scopeJobRunID: true, scopeJobQuery: true, scopeJobBody: true,
		scopeScheduleBody: true, scopeSchedulePath: true, scopeFiltered: true,
	}
	// The only rows with an alternative permission (spec section 7.2).
	altPermRows := map[string]authz.Permission{
		"GET /api/v1/tokens":         authz.TokenManage,
		"DELETE /api/v1/tokens/{id}": authz.TokenManage,
	}
	// The only agent-or-human rows.
	agentBypassRows := map[string]bool{
		"GET /api/v1/runs/{id}":                     true,
		"GET /api/v1/runs/{id}/outputs":             true,
		"GET /api/v1/runs/{runID}/artifacts":        true,
		"GET /api/v1/runs/{runID}/artifacts/{name}": true,
		"GET /api/v1/agents/{agentId}":              true,
		"GET /api/v1/agents/{agentId}/runs":         true,
	}

	seen := map[string]bool{}
	for _, row := range routeTable {
		key := row.Method + " " + row.Pattern
		require.Falsef(t, seen[key], "duplicate route table row %s", key)
		seen[key] = true
		require.Equalf(t, row.Pattern, normalizeRoutePattern(row.Pattern), "%s: pattern must be normalized", key)

		if isHumanRow(row) {
			_, known := authz.Parse(string(row.Perm))
			require.Truef(t, known, "%s: human row needs a known Perm, got %q", key, row.Perm)
		} else {
			require.Emptyf(t, row.Perm, "%s: public/self/agent row must not carry a Perm", key)
		}
		if jobScopes[row.Scope] {
			require.Truef(t, authz.FolderScopable(row.Perm), "%s: folder-scoped row needs a folder-scopable Perm, got %q", key, row.Perm)
		}
		if row.Scope == scopeGlobal {
			require.Falsef(t, authz.FolderScopable(row.Perm), "%s: global row needs a global-only Perm, got %q", key, row.Perm)
		}

		if row.Scope == scopeJobRunID {
			require.Containsf(t, []string{"id", "runID"}, row.RunParam, "%s: RunParam", key)
			require.Containsf(t, row.Pattern, "{"+row.RunParam+"}", "%s: RunParam must be a parameter of the pattern", key)
		} else {
			require.Emptyf(t, row.RunParam, "%s: RunParam only applies to scopeJobRunID", key)
		}

		require.Equalf(t, agentBypassRows[key], row.AgentBypass, "%s: AgentBypass", key)
		require.Equalf(t, key == "GET /api/v1/agents/{agentId}/runs", row.FiltersRows, "%s: FiltersRows", key)

		alt, wantAlt := altPermRows[key]
		require.Equalf(t, alt, row.AltPerm, "%s: AltPerm", key)
		if wantAlt {
			require.Equalf(t, authz.TokenCreate, row.Perm, "%s: Perm", key)
		}
	}
	for key := range altPermRows {
		require.Truef(t, seen[key], "AltPerm row %s is missing from the table", key)
	}
	for key := range agentBypassRows {
		require.Truef(t, seen[key], "agent-bypass row %s is missing from the table", key)
	}
}
