package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// auditActionKey classifies a route (method + chi route pattern) into a short
// action label like "job.apply" or "secret.set", plus how to resolve the
// resource name for the audit log row. Only state-changing (POST/PUT/DELETE)
// human-facing routes need an entry. The table is an allow-list: a route absent
// from it is not recorded, except that a request refused by authorization is
// always recorded (see auditLogMiddleware).
var auditActionTable = map[string]string{
	"POST /api/v1/jobs":                                          "job.apply",
	"DELETE /api/v1/jobs/*":                                      "job.delete",
	"POST /api/v1/runs":                                          "run.trigger",
	"POST /api/v1/runs/{id}/cancel":                              "run.cancel",
	"POST /api/v1/runs/{id}/replay":                              "run.replay",
	"DELETE /api/v1/runs/{id}":                                   "run.delete",
	"POST /api/v1/runs/{runID}/approvals/{stepIndex}":            "run.approval.decide",
	"POST /api/v1/secrets":                                       "secret.set",
	"DELETE /api/v1/secrets/{name}":                              "secret.delete",
	"POST /api/v1/gitcredentials":                                "gitcredential.upsert",
	"DELETE /api/v1/gitcredentials/{name}":                       "gitcredential.delete",
	"POST /api/v1/tokens":                                        "token.create",
	"DELETE /api/v1/tokens/{id}":                                 "token.delete",
	"POST /api/v1/webhooks":                                      "webhook.apply",
	"DELETE /api/v1/webhooks/{name}":                             "webhook.delete",
	"POST /api/v1/vars":                                          "vars.apply",
	"DELETE /api/v1/vars/{name}":                                 "vars.delete",
	"POST /api/v1/schedules":                                     "schedule.apply",
	"DELETE /api/v1/schedules/{name}":                            "schedule.delete",
	"POST /api/v1/appsources":                                    "appsource.apply",
	"DELETE /api/v1/appsources/{name}":                           "appsource.delete",
	"POST /api/v1/appsources/{name}/sync":                        "appsource.sync",
	"POST /api/v1/agent-enrollments":                             "agent.enrollment.create",
	"DELETE /api/v1/agent-enrollments/{id}":                      "agent.enrollment.revoke",
	"POST /api/v1/agent-enrollment-policies":                     "agent.policy.create",
	"PUT /api/v1/agent-enrollment-policies/{name}":               "agent.policy.update",
	"DELETE /api/v1/agent-enrollment-policies/{name}":            "agent.policy.delete",
	"POST /api/v1/agent-identities/{agentId}/enable":             "agent.identity.enable",
	"POST /api/v1/agent-identities/{agentId}/disable":            "agent.identity.disable",
	"POST /api/v1/agent-identities/{agentId}/credentials/revoke": "agent.credentials.revoke",
}

// auditResourceParams lists, per action, the chi URL param name (if any) that
// identifies the resource. When empty, the resource is resolved from the
// request/response body instead (see auditBodyNameActions).
//
// "*" is a special sentinel meaning the resource is the chi catch-all wildcard
// param, with any leading slash trimmed (see auditResourceFromParam). This is
// used by routes registered as "/jobs/*" rather than "/jobs/{name}", where job
// names may themselves contain "/" (e.g. "team-a/build").
var auditResourceParams = map[string]string{
	"job.delete":               "*",
	"run.cancel":               "id",
	"run.replay":               "id",
	"run.delete":               "id",
	"run.approval.decide":      "runID",
	"secret.delete":            "name",
	"gitcredential.delete":     "name",
	"token.delete":             "id",
	"webhook.delete":           "name",
	"vars.delete":              "name",
	"schedule.delete":          "name",
	"appsource.delete":         "name",
	"appsource.sync":           "name",
	"agent.enrollment.revoke":  "id",
	"agent.policy.update":      "name",
	"agent.policy.delete":      "name",
	"agent.identity.enable":    "agentId",
	"agent.identity.disable":   "agentId",
	"agent.credentials.revoke": "agentId",
}

// auditResourceFromParam resolves a resource name from a chi URL param. The
// "*" sentinel (see auditResourceParams) reads the catch-all wildcard and
// trims any leading slash, so a request to DELETE /api/v1/jobs/team-a/build
// resolves to the resource "team-a/build", not "/team-a/build".
func auditResourceFromParam(r *http.Request, param string) string {
	if param == "*" {
		return strings.TrimPrefix(chi.URLParam(r, "*"), "/")
	}
	return chi.URLParam(r, param)
}

// auditBodyNameSource says whether to resolve a body-derived resource name
// from the "request" body or the "response" body for a given action.
// Secrets deliberately read the request body (name only, never "value")
// so that no secret value is ever inspected or logged. gitcredential.upsert
// also reads the request body: its handler responds 204 No Content with no
// body, so a "response" source would always resolve to an empty resource.
var auditBodyNameSource = map[string]string{
	"job.apply":               "response",
	"run.trigger":             "response",
	"secret.set":              "request",
	"gitcredential.upsert":    "request",
	"token.create":            "response",
	"webhook.apply":           "response",
	"vars.apply":              "response",
	"schedule.apply":          "response",
	"appsource.apply":         "response",
	"agent.enrollment.create": "response",
	"agent.policy.create":     "request",
}

const auditBodyPeekLimit = 1 << 16 // 64 KiB is plenty for the small JSON envelopes these handlers use.

// auditResponseRecorder wraps the chi WrapResponseWriter to additionally buffer
// a bounded prefix of the response body, so the audit middleware can pull a
// "name"/"id" field out of it without holding the entire body (large log/archive
// endpoints are excluded from this middleware entirely, but this bound is a
// second line of defense).
type auditResponseRecorder struct {
	middleware.WrapResponseWriter
	buf   bytes.Buffer
	limit int
}

func (r *auditResponseRecorder) Write(b []byte) (int, error) {
	if r.buf.Len() < r.limit {
		room := r.limit - r.buf.Len()
		if room > len(b) {
			room = len(b)
		}
		r.buf.Write(b[:room])
	}
	return r.WrapResponseWriter.Write(b)
}

// auditNameFromJSON extracts a human-readable resource identifier from a small
// JSON object, preferring "name" then "id". Never logs or returns any other
// field (in particular, this is why secrets are read from the request body:
// the only field pulled out is "name", "value" is never touched).
func auditNameFromJSON(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return ""
	}
	if v, ok := m["name"].(string); ok && v != "" {
		return v
	}
	if v, ok := m["id"].(string); ok && v != "" {
		return v
	}
	return ""
}

// classifyAudit resolves the (action, resolver) for a request based on its
// method and the matched chi route pattern. Returns ok=false when the route
// is not in the audit table (e.g. GET requests, or routes intentionally
// excluded such as agent/webhook-ingress/auth endpoints).
func classifyAudit(method, pattern string) (action string, ok bool) {
	action, ok = auditActionTable[method+" "+pattern]
	return action, ok
}

// auditLogMiddleware records human API calls to the audit_logs table: who
// (actor from the resolved Principal), what (method/path/action), on what
// (resource, best-effort), when, the resulting HTTP status, and the
// authorization decision (the permission checked, its scope and the grant that
// allowed it).
//
// State-changing (POST/PUT/DELETE) requests are recorded when their route is in
// auditActionTable; this is an explicit allow-list so that new routes must be
// deliberately classified rather than silently audited (or silently missed).
// Any request that authorization refused is recorded as well, including reads
// and routes absent from the table: the row's action is the permission that
// was checked, its resource the scope, and its status the one the client
// actually saw (a hidden resource is answered 404, 204 or 400, not 403).
// Allowed reads are never recorded. It must run inside ServerAuth so
// principalFromContext is populated, and outside authorize so the decision
// slot it attaches is filled.
func auditLogMiddleware(st interface {
	InsertAuditLog(ctx context.Context, e store.AuditEntry) error
}) func(http.Handler) http.Handler {
	insert := func(r *http.Request, e store.AuditEntry) {
		e.Actor = "unknown"
		if p, ok := principalFromContext(r.Context()); ok && p.Name != "" {
			e.Actor = p.Name
		}
		e.Method = r.Method
		e.Path = r.URL.Path
		// Recorded synchronously (after the response has already been written
		// to the client) so that callers/tests observing a completed request
		// can rely on the audit row already being present. This mirrors
		// TouchPAT/TouchSession, which fire-and-forget only for non-critical
		// bookkeeping; the audit trail is treated as part of the request.
		if err := st.InsertAuditLog(r.Context(), e); err != nil {
			slog.Warn("audit log insert failed", "error", err, "action", e.Action)
		}
	}
	// recordDenied writes the row for a refused request that has no entry in
	// auditActionTable (every GET/HEAD, and unclassified writes).
	recordDenied := func(r *http.Request, d *authzDecision) {
		insert(r, store.AuditEntry{
			Action:     string(d.Permission),
			Resource:   d.Scope,
			Status:     d.Status,
			Permission: string(d.Permission),
			Scope:      d.Scope,
		})
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r, slot := withDecisionSlot(r)

			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				next.ServeHTTP(w, r)
				if slot.Denied {
					recordDenied(r, slot)
				}
				return
			}
			if r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodDelete {
				next.ServeHTTP(w, r)
				return
			}

			// Peek at the body (bounded) for body-derived resource names, but
			// hand the handler the peeked bytes FOLLOWED BY the unread
			// remainder — the peek must never truncate what the handler sees
			// (a >64 KiB job YAML or secret would otherwise be silently cut).
			var reqBody []byte
			if r.Body != nil {
				origBody := r.Body
				reqBody, _ = io.ReadAll(io.LimitReader(origBody, auditBodyPeekLimit+1))
				r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(reqBody), origBody))
			}

			rec := &auditResponseRecorder{
				WrapResponseWriter: middleware.NewWrapResponseWriter(w, r.ProtoMajor),
				limit:              auditBodyPeekLimit,
			}
			next.ServeHTTP(rec, r)

			pattern := chi.RouteContext(r.Context()).RoutePattern()
			action, ok := classifyAudit(r.Method, pattern)
			if !ok {
				if slot.Denied {
					recordDenied(r, slot)
				}
				return
			}

			resource := ""
			if param, ok := auditResourceParams[action]; ok && param != "" {
				resource = auditResourceFromParam(r, param)
			} else if src, ok := auditBodyNameSource[action]; ok {
				switch src {
				case "request":
					resource = auditNameFromJSON(reqBody)
				case "response":
					resource = auditNameFromJSON(rec.buf.Bytes())
				}
			}

			e := store.AuditEntry{
				Action:     action,
				Resource:   resource,
				Status:     rec.Status(),
				Permission: string(slot.Permission),
				Scope:      slot.Scope,
			}
			if !slot.Denied {
				e.GrantedBy = slot.GrantedBy
			}
			insert(r, e)
		})
	}
}
