package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5"
)

const (
	msgNoPermissions           = "forbidden: no permissions"
	msgTokenOwnerNoPermissions = "forbidden: token owner has no permissions"
	msgUnclassifiedRoute       = "forbidden: unclassified route"
	// msgLogArchiveUnavailable is what the log archive route answers (404) when
	// the run has no archive, including a run that does not exist.
	msgLogArchiveUnavailable = "log archive not available yet"
	// msgNoPendingApproval is what deciding an approval answers (404) when there
	// is nothing to decide, including for a run that does not exist.
	msgNoPendingApproval = "no pending approval"
	// msgArtifactNotFound is what an artifact download answers (404) when there
	// is no such artifact, including for a run that does not exist.
	msgArtifactNotFound = "artifact not found"
)

// missingPerm is the 403 body for a refusal that names only the permission
// (global-only routes, and the pre-check before any job is looked at).
func missingPerm(p authz.Permission) string {
	return fmt.Sprintf("forbidden: missing permission %q", string(p))
}

// missingPermOnJob is the 403 body for a caller who can read job but lacks p.
func missingPermOnJob(p authz.Permission, job string) string {
	return fmt.Sprintf("forbidden: missing permission %q on job %q", string(p), job)
}

// authzDecision is what authorize decided for a request, kept for the audit
// log. Scope is the job name, or "global" for a global check.
type authzDecision struct {
	Permission authz.Permission
	Scope      string
	GrantedBy  string
	Denied     bool
	Status     int
}

const scopeGlobalName = "global"

type decisionSlotKey struct{}

// withDecisionSlot attaches an empty decision slot to the request; authorize
// and requireJob fill it through recordDecision.
func withDecisionSlot(r *http.Request) (*http.Request, *authzDecision) {
	slot := &authzDecision{}
	return r.WithContext(context.WithValue(r.Context(), decisionSlotKey{}, slot)), slot
}

// recordDecision stores d in the request's decision slot; without a slot it
// does nothing.
func recordDecision(r *http.Request, d authzDecision) {
	if slot, ok := r.Context().Value(decisionSlotKey{}).(*authzDecision); ok {
		*slot = d
	}
}

// deny records a refusal and answers it.
func deny(w http.ResponseWriter, r *http.Request, perm authz.Permission, scope string, status int, body string) {
	recordDecision(r, authzDecision{Permission: perm, Scope: scope, Denied: true, Status: status})
	http.Error(w, body, status)
}

// hideAnswer writes a hidden answer with hide and returns the status actually
// written to the client (the hidden answers are 404, 204 or 400 depending on
// the route), so the audit record names what the caller saw.
func hideAnswer(w http.ResponseWriter, r *http.Request, hide func(http.ResponseWriter)) int {
	ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
	hide(ww)
	if st := ww.Status(); st != 0 {
		return st
	}
	return http.StatusOK
}

// jobNotFound answers like a missing job: the store's own not-found text.
func jobNotFound(w http.ResponseWriter) {
	http.Error(w, store.JobNotFoundError().Error(), http.StatusNotFound)
}

// missingRunAnswer returns what the handler of the run route with the given
// chi pattern writes for a run that does not exist. A run the caller may not
// read is answered with exactly this, so hidden and missing runs look alike.
// Most run routes look the run up and answer with the store's not-found text;
// two write their own text.
func missingRunAnswer(pattern, runID string) func(http.ResponseWriter) {
	body := store.RunNotFoundError(runID).Error()
	switch pattern {
	case "/api/v1/runs/{id}/yaml":
		body = store.RunSpecNotFoundError().Error()
	case "/api/v1/runs/{id}/logs/archive":
		body = msgLogArchiveUnavailable
	case "/api/v1/runs/{runID}/approvals/{stepIndex}":
		body = msgNoPendingApproval
	case "/api/v1/runs/{runID}/artifacts/{name}":
		body = msgArtifactNotFound
	}
	return func(w http.ResponseWriter) { http.Error(w, body, http.StatusNotFound) }
}

// authorize is the single permission check for every human route (spec 7.2).
// It runs after authentication and looks the route up in routeTable; a route
// that is not classified is refused.
func (s *Server) authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pattern := ""
		if rctx := chi.RouteContext(r.Context()); rctx != nil {
			pattern = rctx.RoutePattern()
		}
		rule, ok := lookupRoute(r.Method, pattern)
		if !ok {
			slog.Error("unclassified route reached authorize", "method", r.Method, "pattern", pattern)
			http.Error(w, msgUnclassifiedRoute, http.StatusForbidden)
			return
		}
		switch rule.Scope {
		case scopePublic, scopeSelf, scopeAgent:
			// Authenticated (or deliberately open) by their own middleware.
			next.ServeHTTP(w, r)
			return
		}

		if _, isAgent := agentPrincipalFromContext(r.Context()); isAgent {
			if rule.AgentBypass {
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, missingPerm(rule.Perm), http.StatusForbidden)
			return
		}
		p, ok := principalFromContext(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		g := p.Grants
		if g.IsEmpty() {
			msg := msgNoPermissions
			if p.OwnerPATID != "" {
				msg = msgTokenOwnerNoPermissions
			}
			deny(w, r, rule.Perm, scopeGlobalName, http.StatusForbidden, msg)
			return
		}
		// Cheap pre-check before any body or database read.
		if !g.HeldAnywhere(rule.Perm) && !(rule.AltPerm != "" && g.HeldAnywhere(rule.AltPerm)) {
			deny(w, r, rule.Perm, scopeGlobalName, http.StatusForbidden, missingPerm(rule.Perm))
			return
		}

		switch rule.Scope {
		case scopeGlobal:
			ok, by := g.Decide(rule.Perm, "")
			if !ok && rule.AltPerm != "" {
				ok, by = g.Decide(rule.AltPerm, "")
			}
			if !ok {
				deny(w, r, rule.Perm, scopeGlobalName, http.StatusForbidden, missingPerm(rule.Perm))
				return
			}
			recordDecision(r, authzDecision{Permission: rule.Perm, Scope: scopeGlobalName, GrantedBy: by})
			next.ServeHTTP(w, r)

		case scopeJobPath:
			name, _ := parseJobWildcard(r)
			if s.requireJob(w, r, rule.Perm, name, jobNotFound) {
				next.ServeHTTP(w, r)
			}

		case scopeJobQuery:
			job := r.URL.Query().Get("jobName")
			if job == "" {
				next.ServeHTTP(w, r) // the handler answers 400
				return
			}
			if s.requireJob(w, r, rule.Perm, job, jobNotFound) {
				next.ServeHTTP(w, r)
			}

		case scopeJobRunID:
			s.authorizeRun(w, r, rule, next)

		case scopeSchedulePath:
			s.authorizeSchedule(w, r, rule, next)

		default:
			// scopeJobBody, scopeScheduleBody and scopeFiltered: the handlers
			// decide once they have parsed the body or loaded the rows.
			next.ServeHTTP(w, r)
		}
	})
}

// authorizeRun checks a route whose job is the one the run in the path
// belongs to.
func (s *Server) authorizeRun(w http.ResponseWriter, r *http.Request, rule routeRule, next http.Handler) {
	p, _ := principalFromContext(r.Context())
	g := p.Grants
	runID := chi.URLParam(r, rule.RunParam)

	// Global fast path: both permissions are held everywhere, so the run's job
	// cannot change the answer.
	if g.Can(rule.Perm, "") && g.Can(authz.JobRead, "") {
		_, by := g.Decide(rule.Perm, "")
		d := authzDecision{Permission: rule.Perm, Scope: scopeGlobalName, GrantedBy: by}
		if r.Method != http.MethodGet {
			// Best effort: name the job in the audit record.
			if job, found, err := s.store.RunJobName(r.Context(), runID); err == nil && found {
				d.Scope = job
			}
		}
		recordDecision(r, d)
		next.ServeHTTP(w, r)
		return
	}

	job, found, err := s.store.RunJobName(r.Context(), runID)
	if err != nil {
		slog.Error("authorize: run job lookup failed", "run", runID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	missing := missingRunAnswer(rule.Pattern, runID)
	if !found {
		status := hideAnswer(w, r, missing)
		recordDecision(r, authzDecision{Permission: rule.Perm, Scope: scopeGlobalName, Denied: true, Status: status})
		return
	}
	if s.requireJob(w, r, rule.Perm, job, missing) {
		next.ServeHTTP(w, r)
	}
}

// authorizeSchedule checks a route whose job is the current target of the
// schedule named in the path.
func (s *Server) authorizeSchedule(w http.ResponseWriter, r *http.Request, rule routeRule, next http.Handler) {
	p, _ := principalFromContext(r.Context())
	g := p.Grants
	// Global fast path: both permissions are held everywhere, so the target job
	// cannot change the answer.
	if g.Can(rule.Perm, "") && g.Can(authz.JobRead, "") {
		_, by := g.Decide(rule.Perm, "")
		recordDecision(r, authzDecision{Permission: rule.Perm, Scope: scopeGlobalName, GrantedBy: by})
		next.ServeHTTP(w, r)
		return
	}
	name := chi.URLParam(r, "name")
	sc, err := s.store.GetSchedule(r.Context(), name)
	if errors.Is(err, pgx.ErrNoRows) {
		// Deleting a schedule that does not exist is idempotent.
		next.ServeHTTP(w, r)
		return
	}
	if err != nil {
		slog.Error("authorize: schedule lookup failed", "schedule", name, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// A schedule whose target the caller cannot read answers like a missing one
	// (the idempotent delete) without being touched.
	hide := func(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }
	if s.requireJob(w, r, rule.Perm, sc.JobName, hide) {
		next.ServeHTTP(w, r)
	}
}

// requireJob checks perm on job for the request's principal. A caller who
// cannot read job gets hide (a 404 identical to a missing resource); with a
// nil hide it is told what it is missing, as for a readable job. A caller who
// can read job but lacks perm gets a 403 naming the permission. It reports
// whether the request may proceed; when it returns false the response has been
// written.
func (s *Server) requireJob(w http.ResponseWriter, r *http.Request, perm authz.Permission, job string, hide func(http.ResponseWriter)) bool {
	p, ok := principalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	if readable, _ := p.Grants.Decide(authz.JobRead, job); !readable {
		if hide == nil {
			deny(w, r, perm, job, http.StatusForbidden, missingPermOnJob(perm, job))
			return false
		}
		status := hideAnswer(w, r, hide)
		recordDecision(r, authzDecision{Permission: perm, Scope: job, Denied: true, Status: status})
		return false
	}
	allowed, by := p.Grants.Decide(perm, job)
	if !allowed {
		deny(w, r, perm, job, http.StatusForbidden, missingPermOnJob(perm, job))
		return false
	}
	recordDecision(r, authzDecision{Permission: perm, Scope: job, GrantedBy: by})
	return true
}

// canReadJob reports whether the request's principal may read job. Agent
// principals always can.
func (s *Server) canReadJob(r *http.Request, job string) bool {
	if _, isAgent := agentPrincipalFromContext(r.Context()); isAgent {
		return true
	}
	p, ok := principalFromContext(r.Context())
	if !ok {
		return false
	}
	return p.Grants.Can(authz.JobRead, job)
}
