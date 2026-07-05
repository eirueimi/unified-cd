package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// auditActionKey classifies a route (method + chi route pattern) into a short
// action label like "job.apply" or "secret.set", plus how to resolve the
// resource name for the audit log row. Only state-changing (POST/PUT/DELETE)
// human-facing routes need an entry; anything absent from this table is
// recorded with a generic action derived from the path (see classifyAudit).
var auditActionTable = map[string]string{
	"POST /api/v1/jobs":                               "job.apply",
	"DELETE /api/v1/jobs/{name}":                      "job.delete",
	"POST /api/v1/runs":                               "run.trigger",
	"POST /api/v1/runs/{id}/cancel":                   "run.cancel",
	"DELETE /api/v1/runs/{id}":                        "run.delete",
	"POST /api/v1/runs/{runID}/approvals/{stepIndex}": "run.approval.decide",
	"POST /api/v1/secrets":                            "secret.set",
	"DELETE /api/v1/secrets/{name}":                   "secret.delete",
	"POST /api/v1/gitcredentials":                     "gitcredential.upsert",
	"DELETE /api/v1/gitcredentials/{name}":            "gitcredential.delete",
	"POST /api/v1/tokens":                             "token.create",
	"DELETE /api/v1/tokens/{id}":                      "token.delete",
	"POST /api/v1/webhooks":                           "webhook.apply",
	"DELETE /api/v1/webhooks/{name}":                  "webhook.delete",
	"POST /api/v1/schedules":                          "schedule.apply",
	"DELETE /api/v1/schedules/{name}":                 "schedule.delete",
	"POST /api/v1/appsources":                         "appsource.apply",
	"DELETE /api/v1/appsources/{name}":                "appsource.delete",
	"POST /api/v1/appsources/{name}/sync":             "appsource.sync",
}

// auditResourceParams lists, per action, the chi URL param name (if any) that
// identifies the resource. When empty, the resource is resolved from the
// request/response body instead (see auditBodyNameActions).
var auditResourceParams = map[string]string{
	"job.delete":           "name",
	"run.cancel":           "id",
	"run.delete":           "id",
	"run.approval.decide":  "runID",
	"secret.delete":        "name",
	"gitcredential.delete": "name",
	"token.delete":         "id",
	"webhook.delete":       "name",
	"schedule.delete":      "name",
	"appsource.delete":     "name",
	"appsource.sync":       "name",
}

// auditBodyNameSource says whether to resolve a body-derived resource name
// from the "request" body or the "response" body for a given action.
// Secrets deliberately read the request body (name only, never "value")
// so that no secret value is ever inspected or logged. gitcredential.upsert
// also reads the request body: its handler responds 204 No Content with no
// body, so a "response" source would always resolve to an empty resource.
var auditBodyNameSource = map[string]string{
	"job.apply":            "response",
	"run.trigger":          "response",
	"secret.set":           "request",
	"gitcredential.upsert": "request",
	"token.create":         "response",
	"webhook.apply":        "response",
	"schedule.apply":       "response",
	"appsource.apply":      "response",
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

// auditLogMiddleware records state-changing (POST/PUT/DELETE) human API calls
// to the audit_logs table: who (actor from the resolved Principal), what
// (method/path/action), on what (resource, best-effort), when, and the
// resulting HTTP status. Read-only (GET) requests are never recorded.
//
// Only routes present in auditActionTable are recorded; this is an explicit
// allow-list so that new routes must be deliberately classified rather than
// silently audited (or silently missed). It must run inside ServerAuth so
// principalFromContext is populated.
func auditLogMiddleware(st interface {
	InsertAuditLog(ctx context.Context, actor, method, path, action, resource string, status int) error
}) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodDelete {
				next.ServeHTTP(w, r)
				return
			}

			// Buffer the request body (bounded) so handlers can still read it
			// normally, while the middleware can peek at it after the fact for
			// body-derived resource names (e.g. secret.set -> "name" only).
			var reqBody []byte
			if r.Body != nil {
				reqBody, _ = io.ReadAll(io.LimitReader(r.Body, auditBodyPeekLimit+1))
				r.Body = io.NopCloser(bytes.NewReader(reqBody))
			}

			rec := &auditResponseRecorder{
				WrapResponseWriter: middleware.NewWrapResponseWriter(w, r.ProtoMajor),
				limit:              auditBodyPeekLimit,
			}
			next.ServeHTTP(rec, r)

			pattern := chi.RouteContext(r.Context()).RoutePattern()
			action, ok := classifyAudit(r.Method, pattern)
			if !ok {
				return
			}

			resource := ""
			if param, ok := auditResourceParams[action]; ok && param != "" {
				resource = chi.URLParam(r, param)
			} else if src, ok := auditBodyNameSource[action]; ok {
				switch src {
				case "request":
					resource = auditNameFromJSON(reqBody)
				case "response":
					resource = auditNameFromJSON(rec.buf.Bytes())
				}
			}

			actor := "unknown"
			if p, ok := principalFromContext(r.Context()); ok && p.Name != "" {
				actor = p.Name
			}

			// Recorded synchronously (after the response has already been written
			// to the client) so that callers/tests observing a completed request
			// can rely on the audit row already being present. This mirrors
			// TouchPAT/TouchSession, which fire-and-forget only for non-critical
			// bookkeeping; the audit trail is treated as part of the request.
			if err := st.InsertAuditLog(r.Context(), actor, r.Method, r.URL.Path, action, resource, rec.Status()); err != nil {
				slog.Warn("audit log insert failed", "error", err, "action", action)
			}
		})
	}
}
