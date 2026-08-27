package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/require"
)

// listenExhaustedStore wraps a real store.Store and overrides only
// ListenForNotify, to deterministically simulate listen-pool exhaustion
// without needing to actually saturate a real pool at this layer — that
// case is covered honestly in internal/store's own test. This layer only
// needs to prove handleRunEvents reacts to the sentinel correctly.
type listenExhaustedStore struct {
	store.Store
}

func (listenExhaustedStore) ListenForNotify(_ context.Context, channel string, _ func(payload string)) error {
	return fmt.Errorf("acquire listen pool connection for channel %q: %w", channel, store.ErrListenPoolExhausted)
}

func TestAPI_RunEvents_SSE_ListenPoolExhausted_EmitsErrorEventAndCloses(t *testing.T) {
	s, pg := newTestServer(t)
	_, err := pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "", "")
	require.NoError(t, err)
	_, err = pg.TransitionPendingToQueued(t.Context(), 10)
	require.NoError(t, err)
	_, err = pg.ClaimNextRun(t.Context(), "agent-1", nil)
	require.NoError(t, err)
	require.NoError(t, pg.MarkRunRunning(t.Context(), run.ID))

	s.store = listenExhaustedStore{Store: pg}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+run.ID+"/events", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code) // headers already committed before Listen is reached — confirms the "503 impossible here" finding
	require.Contains(t, rec.Body.String(), `"type":"error"`)
	require.Contains(t, rec.Body.String(), "connection pool exhausted")
}
