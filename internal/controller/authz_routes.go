package controller

import (
	"strings"

	"github.com/eirueimi/unified-cd/internal/authz"
)

// routeScope says how the job (if any) a request acts on is found, which in
// turn decides how the route's permission is checked.
type routeScope uint8

const (
	// scopeGlobal: the permission is checked against the caller's global grants.
	scopeGlobal routeScope = iota
	// scopeJobPath: the job name is the "*" wildcard of the path.
	scopeJobPath
	// scopeJobRunID: the job is the one the run in the path belongs to
	// (routeRule.RunParam names the path parameter).
	scopeJobRunID
	// scopeJobQuery: the job is the "jobName" query parameter.
	scopeJobQuery
	// scopeJobBody: the job comes from the request body.
	scopeJobBody
	// scopeScheduleBody: the job is the target of the schedule in the body (and
	// the existing schedule's current target when the name already exists).
	scopeScheduleBody
	// scopeSchedulePath: the job is the current target of the schedule named in
	// the path.
	scopeSchedulePath
	// scopeFiltered: any authenticated caller may reach the handler; rows are
	// filtered by the permission in the handler.
	scopeFiltered
	// scopePublic: no principal; the route authenticates itself or is open.
	scopePublic
	// scopeSelf: any authenticated principal, answering about itself.
	scopeSelf
	// scopeAgent: agent credentials only, enforced by agent middleware.
	scopeAgent
)

// routeRule classifies one registered route.
type routeRule struct {
	// Method is an HTTP method, or "*" for routes registered with HandleFunc
	// (any method).
	Method string
	// Pattern is the chi route pattern as RoutePattern() reports it.
	Pattern string
	// Perm is the permission a human caller needs (empty for public, self and
	// agent routes).
	Perm authz.Permission
	// AltPerm is an alternative permission: the route is satisfied when the
	// caller holds Perm or AltPerm (zero value: none).
	AltPerm authz.Permission
	Scope   routeScope
	// RunParam names the run path parameter ("id" or "runID") for scopeJobRunID.
	RunParam string
	// AgentBypass marks agent-or-human routes: an agent principal skips the
	// permission check.
	AgentBypass bool
	// FiltersRows marks a route whose rows are additionally filtered by job.read
	// in the handler (GET /api/v1/agents/{agentId}/runs).
	FiltersRows bool
}

const (
	mGet    = "GET"
	mPost   = "POST"
	mPut    = "PUT"
	mDelete = "DELETE"
	mAny    = "*"
)

func global(method, pattern string, perm authz.Permission) routeRule {
	return routeRule{Method: method, Pattern: pattern, Perm: perm, Scope: scopeGlobal}
}

func ofRun(method, pattern string, perm authz.Permission, runParam string) routeRule {
	return routeRule{Method: method, Pattern: pattern, Perm: perm, Scope: scopeJobRunID, RunParam: runParam}
}

func unauthenticated(method, pattern string) routeRule {
	return routeRule{Method: method, Pattern: pattern, Scope: scopePublic}
}

func agentOnly(method, pattern string) routeRule {
	return routeRule{Method: method, Pattern: pattern, Scope: scopeAgent}
}

// routeTable is the single classification of every registered route
// (spec §7.2). A route that is not listed here fails
// TestEveryRegisteredRouteIsClassified.
var routeTable = []routeRule{
	// Human and agent-or-human routes.
	global(mGet, "/api/v1/audit", authz.AuditRead),
	global(mGet, "/api/v1/authz/policy", authz.AuthzRead),
	global(mPost, "/api/v1/authz/explain", authz.AuthzRead),

	{Method: mPost, Pattern: "/api/v1/jobs", Perm: authz.JobConfigure, Scope: scopeJobBody},
	{Method: mGet, Pattern: "/api/v1/jobs", Perm: authz.JobRead, Scope: scopeFiltered},
	// Covers the job itself, "/yaml" and "/schedulability".
	{Method: mGet, Pattern: "/api/v1/jobs/*", Perm: authz.JobRead, Scope: scopeJobPath},
	{Method: mDelete, Pattern: "/api/v1/jobs/*", Perm: authz.JobDelete, Scope: scopeJobPath},

	{Method: mPost, Pattern: "/api/v1/runs", Perm: authz.RunTrigger, Scope: scopeJobBody},
	ofRun(mPost, "/api/v1/runs/{id}/replay", authz.RunReplay, "id"),
	{Method: mGet, Pattern: "/api/v1/runs", Perm: authz.JobRead, Scope: scopeJobQuery},
	{Method: mGet, Pattern: "/api/v1/runs/active", Perm: authz.JobRead, Scope: scopeFiltered},
	{Method: mGet, Pattern: "/api/v1/runs/{id}", Perm: authz.JobRead, Scope: scopeJobRunID, RunParam: "id", AgentBypass: true},
	{Method: mGet, Pattern: "/api/v1/runs/{id}/outputs", Perm: authz.JobRead, Scope: scopeJobRunID, RunParam: "id", AgentBypass: true},
	ofRun(mGet, "/api/v1/runs/{id}/yaml", authz.JobRead, "id"),
	ofRun(mPost, "/api/v1/runs/{id}/cancel", authz.RunCancel, "id"),
	ofRun(mDelete, "/api/v1/runs/{id}", authz.RunDelete, "id"),
	ofRun(mGet, "/api/v1/runs/{id}/events", authz.JobRead, "id"),
	ofRun(mGet, "/api/v1/runs/{id}/logs", authz.JobRead, "id"),
	ofRun(mGet, "/api/v1/runs/{id}/steps", authz.JobRead, "id"),
	ofRun(mGet, "/api/v1/runs/{id}/logs/archive", authz.JobRead, "id"),
	ofRun(mGet, "/api/v1/runs/{id}/logs/stats", authz.JobRead, "id"),
	ofRun(mGet, "/api/v1/runs/{id}/logs/range", authz.JobRead, "id"),
	ofRun(mGet, "/api/v1/runs/{id}/logs/search", authz.JobRead, "id"),
	ofRun(mGet, "/api/v1/runs/{runID}/approvals", authz.JobRead, "runID"),
	ofRun(mPost, "/api/v1/runs/{runID}/approvals/{stepIndex}", authz.RunApprove, "runID"),
	{Method: mGet, Pattern: "/api/v1/runs/{runID}/artifacts", Perm: authz.JobRead, Scope: scopeJobRunID, RunParam: "runID", AgentBypass: true},
	{Method: mGet, Pattern: "/api/v1/runs/{runID}/artifacts/{name}", Perm: authz.JobRead, Scope: scopeJobRunID, RunParam: "runID", AgentBypass: true},

	global(mPost, "/api/v1/secrets", authz.SecretWrite),
	global(mGet, "/api/v1/secrets", authz.SecretList),
	global(mDelete, "/api/v1/secrets/{name}", authz.SecretWrite),
	global(mPost, "/api/v1/gitcredentials", authz.GitCredentialWrite),
	global(mGet, "/api/v1/gitcredentials", authz.GitCredentialRead),
	global(mDelete, "/api/v1/gitcredentials/{name}", authz.GitCredentialWrite),

	global(mPost, "/api/v1/tokens", authz.TokenCreate),
	// token.create covers the caller's own tokens, token.manage everyone's.
	{Method: mGet, Pattern: "/api/v1/tokens", Perm: authz.TokenCreate, AltPerm: authz.TokenManage, Scope: scopeGlobal},
	{Method: mDelete, Pattern: "/api/v1/tokens/{id}", Perm: authz.TokenCreate, AltPerm: authz.TokenManage, Scope: scopeGlobal},

	global(mPost, "/api/v1/agent-enrollments", authz.AgentManage),
	global(mGet, "/api/v1/agent-enrollments", authz.AgentRead),
	global(mDelete, "/api/v1/agent-enrollments/{id}", authz.AgentManage),
	global(mPost, "/api/v1/agent-enrollment-policies", authz.AgentManage),
	global(mGet, "/api/v1/agent-enrollment-policies", authz.AgentRead),
	global(mGet, "/api/v1/agent-enrollment-policies/{name}", authz.AgentRead),
	global(mPut, "/api/v1/agent-enrollment-policies/{name}", authz.AgentManage),
	global(mDelete, "/api/v1/agent-enrollment-policies/{name}", authz.AgentManage),
	global(mGet, "/api/v1/agent-identities/{agentId}", authz.AgentRead),
	global(mPost, "/api/v1/agent-identities/{agentId}/enable", authz.AgentManage),
	global(mPost, "/api/v1/agent-identities/{agentId}/disable", authz.AgentManage),
	global(mPost, "/api/v1/agent-identities/{agentId}/credentials/revoke", authz.AgentManage),
	global(mGet, "/api/v1/agents", authz.AgentRead),
	{Method: mGet, Pattern: "/api/v1/agents/{agentId}", Perm: authz.AgentRead, Scope: scopeGlobal, AgentBypass: true},
	{Method: mGet, Pattern: "/api/v1/agents/{agentId}/runs", Perm: authz.AgentRead, Scope: scopeGlobal, AgentBypass: true, FiltersRows: true},

	global(mPost, "/api/v1/webhooks", authz.WebhookWrite),
	global(mGet, "/api/v1/webhooks", authz.WebhookRead),
	global(mDelete, "/api/v1/webhooks/{name}", authz.WebhookWrite),
	global(mPost, "/api/v1/vars", authz.VarsWrite),
	global(mGet, "/api/v1/vars", authz.VarsRead),
	global(mDelete, "/api/v1/vars/{name}", authz.VarsWrite),

	{Method: mPost, Pattern: "/api/v1/schedules", Perm: authz.ScheduleConfigure, Scope: scopeScheduleBody},
	{Method: mGet, Pattern: "/api/v1/schedules", Perm: authz.JobRead, Scope: scopeFiltered},
	{Method: mDelete, Pattern: "/api/v1/schedules/{name}", Perm: authz.ScheduleConfigure, Scope: scopeSchedulePath},

	global(mPost, "/api/v1/appsources", authz.AppSourceWrite),
	global(mGet, "/api/v1/appsources", authz.AppSourceRead),
	global(mGet, "/api/v1/appsources/{name}", authz.AppSourceRead),
	global(mDelete, "/api/v1/appsources/{name}", authz.AppSourceWrite),
	global(mPost, "/api/v1/appsources/{name}/sync", authz.AppSourceWrite),

	// Public and self-authenticating routes.
	unauthenticated(mGet, "/healthz"),
	unauthenticated(mGet, "/readyz"),
	unauthenticated(mGet, "/metrics"),
	unauthenticated(mGet, "/api/v1/auth/oidc-config"),
	unauthenticated(mGet, "/api/v1/ui-config"),
	unauthenticated(mGet, "/api/v1/auth/oidc-login"),
	unauthenticated(mGet, "/api/v1/auth/oidc-callback"),
	unauthenticated(mPost, "/api/v1/auth/logout"),
	{Method: mGet, Pattern: "/api/v1/auth/me", Scope: scopeSelf},
	unauthenticated(mAny, "/dex"),
	unauthenticated(mAny, "/dex/*"),
	unauthenticated(mAny, "/device/callback"),
	unauthenticated(mPost, "/webhook/{name}"),
	unauthenticated(mPost, "/api/v1/agents/enroll"),
	unauthenticated(mPost, "/api/v1/agents/token/refresh"),
	unauthenticated(mPost, "/api/v1/store-credentials"),
	unauthenticated(mAny, "/ui"),
	unauthenticated(mAny, "/ui/*"),
	unauthenticated(mGet, "/"),

	// Agent-only routes (agentAuth; agentRouteIdentityMatrix).
	agentOnly(mPost, "/api/v1/agents/register"),
	agentOnly(mPost, "/api/v1/agents/{agentId}/heartbeat"),
	agentOnly(mDelete, "/api/v1/agents/{agentId}"),
	agentOnly(mPost, "/api/v1/agents/{agentId}/claim"),
	agentOnly(mPost, "/api/v1/agents/{agentId}/steps"),
	agentOnly(mPost, "/api/v1/agents/{agentId}/logs"),
	agentOnly(mPost, "/api/v1/agents/{agentId}/runs/reconcile"),
	agentOnly(mPost, "/api/v1/agents/{agentId}/runs/{runId}/finish"),
	agentOnly(mPost, "/api/v1/agents/{agentId}/runs/{runId}/children"),
	agentOnly(mPost, "/api/v1/agents/{agentId}/runs/{runId}/steps/{stepIndex}/outputs"),
	agentOnly(mPost, "/api/v1/agents/{agentId}/runs/{runId}/outputs"),
	agentOnly(mPost, "/api/v1/agents/{agentId}/runs/{runId}/steps/{stepIndex}/logs/bulk"),
	agentOnly(mPost, "/api/v1/agents/{agentId}/runs/{runId}/sidecars"),
	agentOnly(mPost, "/api/v1/agents/{agentId}/secrets/fetch"),
	agentOnly(mPost, "/api/v1/agents/{agentId}/runs/{runId}/approvals"),
	agentOnly(mGet, "/api/v1/agents/{agentId}/runs/{runId}/approvals/{stepIndex}"),
	agentOnly(mPut, "/api/v1/runs/{runID}/artifacts/{name}"),
}

var routeIndex = func() map[[2]string]routeRule {
	idx := make(map[[2]string]routeRule, len(routeTable))
	for _, r := range routeTable {
		idx[[2]string{r.Method, r.Pattern}] = r
	}
	return idx
}()

// lookupRoute returns the rule for a method and chi route pattern: an exact
// method match first, then a "*" (any method) row.
func lookupRoute(method, pattern string) (routeRule, bool) {
	if r, ok := routeIndex[[2]string{method, pattern}]; ok {
		return r, true
	}
	r, ok := routeIndex[[2]string{mAny, pattern}]
	return r, ok
}

// normalizeRoutePattern turns a pattern as chi.Walk reports it into the form
// RoutePattern() reports: one trailing "/" is dropped unless the pattern is "/".
func normalizeRoutePattern(p string) string {
	if p != "/" {
		p = strings.TrimSuffix(p, "/")
	}
	return p
}
