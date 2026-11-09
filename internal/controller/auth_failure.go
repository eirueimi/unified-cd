package controller

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/eirueimi/unified-cd/internal/store"
	"golang.org/x/time/rate"
)

// Failed authentications are logged and audited, but a client can fail at
// request rate, so reporting is capped by a token bucket per auth method
// (a flood of one method cannot crowd out reports of another): sustained
// failures beyond authFailureRate per second (after a burst of
// authFailureBurst) are dropped and only counted. The count is attached to the
// next report of that method that gets through, so a flood stays visible
// without growing the log or audit_logs at request rate.
const (
	authFailureRate  = 5
	authFailureBurst = 20
)

type authFailureBucket struct {
	limiter    *rate.Limiter
	suppressed atomic.Int64
}

var authFailureBuckets sync.Map // method -> *authFailureBucket

func authFailureBucketFor(method string) *authFailureBucket {
	if b, ok := authFailureBuckets.Load(method); ok {
		return b.(*authFailureBucket)
	}
	b, _ := authFailureBuckets.LoadOrStore(method, &authFailureBucket{limiter: rate.NewLimiter(authFailureRate, authFailureBurst)})
	return b.(*authFailureBucket)
}

// auditWriter is the slice of store.Store that failure reporting needs.
type auditWriter interface {
	InsertAuditLog(ctx context.Context, e store.AuditEntry) error
}

// reportAuthFailure records a rejected authentication attempt: a structured
// slog warning and, when st is non-nil, an `auth.failed` audit row. method is
// the credential kind that was tried ("bearer", "session", "none",
// "agent-token", "oidc-callback") and reason a short fixed code. The presented
// credential, or any part of it, is never logged or stored.
func reportAuthFailure(st auditWriter, r *http.Request, method, reason string) {
	bucket := authFailureBucketFor(method)
	if !bucket.limiter.Allow() {
		bucket.suppressed.Add(1)
		return
	}
	remote := normalizedRemoteIP(r.RemoteAddr)
	attrs := []any{"remote", remote, "method", method, "reason", reason, "path", r.URL.Path}
	if n := bucket.suppressed.Swap(0); n > 0 {
		attrs = append(attrs, "suppressed", n)
	}
	slog.Warn("authentication failed", attrs...)
	// Requests with no credentials at all (unauthenticated probes, health
	// checks, a browser before login) are log-only: an audit row per such
	// request would be noise.
	if st == nil || method == "none" {
		return
	}
	e := store.AuditEntry{
		Actor:    "unknown",
		Method:   r.Method,
		Path:     r.URL.Path,
		Action:   "auth.failed",
		Resource: "method=" + method + " reason=" + reason + " remote=" + remote,
		Status:   http.StatusUnauthorized,
	}
	if err := st.InsertAuditLog(r.Context(), e); err != nil {
		slog.Warn("audit log insert failed", "error", err, "action", e.Action)
	}
}
