package objtransfer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
)

// A 401 makes the client discard the credential and retry the request once.
func TestClient_PresignRetriesOnceAfter401(t *testing.T) {
	var calls, invalidated int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(api.PresignResponse{Mode: api.ObjectModeController})
	}))
	t.Cleanup(srv.Close)

	c := &Client{
		BaseURL: srv.URL, RunID: "r1",
		Auth:           func(context.Context, *http.Request) error { return nil },
		OnUnauthorized: func() { atomic.AddInt32(&invalidated, 1) },
	}
	_, err := c.presign(t.Context(), api.PresignRequest{Kind: api.ObjectKindCache, Op: api.ObjectOpGet, Key: "k"})
	require.ErrorIs(t, err, errControllerMode) // the second answer was read
	assert.EqualValues(t, 2, atomic.LoadInt32(&calls))
	assert.EqualValues(t, 1, atomic.LoadInt32(&invalidated))
}

func TestClient_PresignGivesUpAfterSecond401(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL, RunID: "r1", OnUnauthorized: func() {}}
	_, err := c.presign(t.Context(), api.PresignRequest{Kind: api.ObjectKindCache, Op: api.ObjectOpGet, Key: "k"})
	require.Error(t, err)
	assert.EqualValues(t, 2, atomic.LoadInt32(&calls))
}
