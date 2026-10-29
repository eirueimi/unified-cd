package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/stretchr/testify/require"
)

// uiAction mirrors one entry of web/src/lib/ui-actions.json: a UI control that
// calls a state-changing API.
type uiAction struct {
	File       string  `json:"file"`
	Action     string  `json:"action"`
	Method     string  `json:"method"`
	Route      string  `json:"route"`
	Permission *string `json:"permission"`
}

// TestUIActionInventoryIsEnforced proves no UI action is protected only by
// hiding or disabling its control: every inventoried action maps to a route
// table row whose permission is the one the UI checks, and a caller holding
// every other permission is refused by the server.
func TestUIActionInventoryIsEnforced(t *testing.T) {
	raw, err := os.ReadFile("../../web/src/lib/ui-actions.json")
	require.NoError(t, err)
	var actions []uiAction
	require.NoError(t, json.Unmarshal(raw, &actions))
	require.NotEmpty(t, actions)

	// One caller per distinct permission set: it holds every permission except
	// the action's Perm and AltPerm.
	type caller struct{ email string }
	callers := map[string]caller{}
	var policy strings.Builder
	policy.WriteString("global:\n  users:\n")
	addCaller := func(rule routeRule) string {
		key := string(rule.Perm) + "|" + string(rule.AltPerm)
		if c, ok := callers[key]; ok {
			return c.email
		}
		email := "without-" + strings.NewReplacer(".", "-", "|", "-").Replace(key) + "@example.com"
		var perms []string
		for _, p := range authz.All() {
			if p != rule.Perm && p != rule.AltPerm {
				perms = append(perms, string(p))
			}
		}
		sort.Strings(perms)
		policy.WriteString("    \"" + email + "\": [" + strings.Join(perms, ", ") + "]\n")
		callers[key] = caller{email}
		return email
	}

	type check struct {
		action uiAction
		rule   routeRule
		email  string
	}
	var checks []check
	for _, a := range actions {
		rule, ok := lookupRoute(a.Method, a.Route)
		require.Truef(t, ok, "%s %s (%s %s) has no route table row", a.Method, a.Route, a.File, a.Action)
		if a.Permission == nil {
			require.Equalf(t, scopePublic, rule.Scope, "%s %s has no UI permission but the route is not public", a.Method, a.Route)
			continue
		}
		require.Truef(t, rule.Perm == authz.Permission(*a.Permission) || rule.AltPerm == authz.Permission(*a.Permission),
			"%s %s: the UI checks %q but the route requires %q (alt %q)", a.Method, a.Route, *a.Permission, rule.Perm, rule.AltPerm)
		checks = append(checks, check{a, rule, addCaller(rule)})
	}

	s, _ := newAuthzTestServer(t, authz.Legacy{DefaultRole: "deny"}, policy.String())
	for _, c := range checks {
		path := strings.NewReplacer("{id}", "missing", "{runID}", "missing", "{name}", "missing", "{stepIndex}", "0").Replace(c.action.Route)
		var body []byte
		if c.action.Method != http.MethodDelete {
			body = []byte(`{"jobName":"missing","name":"x","decision":"approve"}`)
		}
		// A short per-request timeout keeps a route that streams from hanging the test.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		req := httptest.NewRequest(c.action.Method, path, bytes.NewReader(body)).WithContext(ctx)
		req.Header.Set("Authorization", bearerPrefix+bearer(c.email))
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		cancel()
		require.Containsf(t, []int{http.StatusForbidden, http.StatusNotFound}, rec.Code,
			"%s %s (%s %s) was not refused for a caller lacking %q: %d %s", c.action.Method, c.action.Route, c.action.File, c.action.Action, c.rule.Perm, rec.Code, rec.Body.String())
	}
}
