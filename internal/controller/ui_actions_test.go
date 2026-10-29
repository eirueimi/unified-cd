package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
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

	// Callers are created lazily per (kind, Perm, AltPerm) and written into the
	// policy before the server starts.
	policy := strings.Builder{}
	policy.WriteString("global:\n  users:\n")
	emails := map[string]string{}
	addCaller := func(kind string, perms []string, rule routeRule) string {
		key := kind + "|" + string(rule.Perm) + "|" + string(rule.AltPerm)
		if e, ok := emails[key]; ok {
			return e
		}
		sort.Strings(perms)
		email := kind + "-" + strings.NewReplacer(".", "-", "|", "-").Replace(key) + "@example.com"
		policy.WriteString("    \"" + email + "\": [" + strings.Join(perms, ", ") + "]\n")
		emails[key] = email
		return email
	}

	type check struct {
		action              uiAction
		rule                routeRule
		lacking, holds, alt string
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

		var others []string
		for _, p := range authz.All() {
			if p != rule.Perm && p != rule.AltPerm {
				others = append(others, string(p))
			}
		}
		c := check{action: a, rule: rule}
		c.lacking = addCaller("lacking", others, rule)
		c.holds = addCaller("holds", []string{string(rule.Perm), string(authz.JobRead)}, rule)
		if rule.AltPerm != "" {
			c.alt = addCaller("alt", []string{string(rule.AltPerm), string(authz.JobRead)}, rule)
		}
		checks = append(checks, c)
	}

	s, st := newAuthzTestServer(t, authz.Legacy{DefaultRole: "deny"}, policy.String())
	ctx := context.Background()
	const job = "team-a/deploy"
	seq := 0

	// do seeds the resources the action needs, sends the request with a short
	// timeout (a streaming route must not hang the test) and returns the answer
	// together with the seeded run ID.
	do := func(c check, email string) (*httptest.ResponseRecorder, string) {
		seq++
		_, err := st.UpsertJob(ctx, job, "unified-cd/v1", []byte(`{"steps":[]}`))
		require.NoError(t, err)
		run, err := st.CreateRun(ctx, job, nil, []byte(`{"steps":[]}`), nil, nil, "", "")
		require.NoError(t, err)
		_ = st.CreatePendingApproval(ctx, run.ID, 0, "gate", "", nil) // best effort: the gate may need a step
		_, err = st.UpsertAppSource(ctx, "apps", []byte(`{"repoURL":"https://github.com/org/repo","targetRevision":"main","path":"jobs/"}`))
		require.NoError(t, err)
		pat, err := st.CreatePAT(ctx, "seed", HashToken("seed-token-"+strconv.Itoa(seq)), "viewer", nil)
		require.NoError(t, err)

		path := strings.NewReplacer("{id}", run.ID, "{runID}", run.ID, "{name}", "apps", "{stepIndex}", "0").Replace(c.action.Route)
		if strings.HasPrefix(c.action.Route, "/api/v1/tokens/") {
			path = "/api/v1/tokens/" + pat.ID
		}
		var body []byte
		switch c.action.Method {
		case http.MethodDelete:
		default:
			body = []byte(`{"jobName":"` + job + `","name":"x","decision":"approve"}`)
		}
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		req := httptest.NewRequest(c.action.Method, path, bytes.NewReader(body)).WithContext(rctx)
		req.Header.Set("Authorization", bearerPrefix+bearer(email))
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		return rec, run.ID
	}

	for _, c := range checks {
		name := c.action.Method + " " + c.action.Route
		// (a) A caller lacking the permission is refused by authorization.
		rec, runID := do(c, c.lacking)
		refused := rec.Code == http.StatusForbidden && strings.HasPrefix(rec.Body.String(), "forbidden: missing permission")
		require.Truef(t, refused || isHidden(c.rule, rec, runID),
			"%s (%s %s): a caller lacking %q was not refused by authorization: %d %q", name, c.action.File, c.action.Action, c.rule.Perm, rec.Code, rec.Body.String())

		// (b) A caller holding Perm (and, separately, AltPerm) is admitted.
		for _, holder := range []string{c.holds, c.alt} {
			if holder == "" {
				continue
			}
			rec, runID := do(c, holder)
			require.Truef(t, isAdmitted(c.rule, rec, runID),
				"%s (%s %s): a caller holding the permission was refused: %d %q", name, c.action.File, c.action.Action, rec.Code, rec.Body.String())
		}
	}
}
