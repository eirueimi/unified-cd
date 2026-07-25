package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
)

// TestAgent_ClaimsDetachedOutsideMaxConcurrent verifies the agent runs a
// separate detached claim pool: even with MaxConcurrent slots busy-polling, it
// still issues kind=detached claims from the MaxDetachedConcurrent pool.
func TestAgent_ClaimsDetachedOutsideMaxConcurrent(t *testing.T) {
	var sawDetached, sawNormal atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/{id}/claim", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "kind=detached") {
			sawDetached.Store(true)
		} else {
			sawNormal.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.ClaimResponse{}) // nothing to claim; keep looping
	})
	// Everything else (register, heartbeat, reconcile, ...) succeeds no-op.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := &Agent{
		ID:                    "a1",
		Client:                NewClient(srv.URL, "tok"),
		MaxConcurrent:         1,
		MaxDetachedConcurrent: 1,
		WorkspaceDir:          t.TempDir(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	_ = a.Run(ctx)

	assert.True(t, sawNormal.Load(), "agent must still issue normal claims")
	assert.True(t, sawDetached.Load(), "agent must issue detached claims from a separate pool")
}

// TestAgent_DetachedPoolOffWhenNegative verifies a negative MaxDetachedConcurrent
// disables detached claiming (no kind=detached claims are issued).
func TestAgent_DetachedPoolOffWhenNegative(t *testing.T) {
	var sawDetached atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/{id}/claim", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "kind=detached") {
			sawDetached.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.ClaimResponse{})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := &Agent{
		ID:                    "a1",
		Client:                NewClient(srv.URL, "tok"),
		MaxConcurrent:         1,
		MaxDetachedConcurrent: -1, // off
		WorkspaceDir:          t.TempDir(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	_ = a.Run(ctx)

	assert.False(t, sawDetached.Load(), "a negative MaxDetachedConcurrent must disable detached claiming")
}
