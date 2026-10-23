package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
)

func u64p(v uint64) *uint64 { return &v }
func boolp(v bool) *bool    { return &v }

// postHeartbeatForTest sends a heartbeat as agentID and returns the recorder.
func postHeartbeatForTest(t *testing.T, s *Server, agentID, token string, hb *api.HeartbeatRequest) *httptest.ResponseRecorder {
	t.Helper()
	var body *bytes.Reader
	if hb != nil {
		b, err := json.Marshal(hb)
		require.NoError(t, err)
		body = bytes.NewReader(b)
	} else {
		body = bytes.NewReader(nil)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/heartbeat", body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	s.Router().ServeHTTP(rr, req)
	return rr
}

func TestHandleAgentHeartbeat_PersistsTheStorage(t *testing.T) {
	s, pg := newTestServer(t)
	token := issueAgentAccessForTest(t, pg, "agent-st", nil, nil)
	measured := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	want := &api.AgentStorage{
		AgentName:          "host-1",
		Workspace:          &api.FilesystemStats{FreeBytes: 40, TotalBytes: 100},
		Volumes:            map[string]api.VolumeStats{"cache": {FilesystemStats: api.FilesystemStats{FreeBytes: 7, TotalBytes: 50}, MeasuredAt: measured, ClaimBlocked: true}},
		MinFreeDiskBytes:   u64p(0),
		ClaimBlockedByDisk: boolp(false),
	}

	rr := postHeartbeatForTest(t, s, "agent-st", token, &api.HeartbeatRequest{ActiveRunIDs: []string{}, Storage: want})
	require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())

	got, err := pg.ListAgentStorage(context.Background(), time.Minute)
	require.NoError(t, err)
	require.Contains(t, got, "agent-st")
	assert.Equal(t, "host-1", got["agent-st"].AgentName)
	assert.Equal(t, want.Workspace, got["agent-st"].Workspace)
	assert.EqualValues(t, 7, got["agent-st"].Volumes["cache"].FreeBytes)
	assert.True(t, got["agent-st"].Volumes["cache"].ClaimBlocked, "a gated volume must survive the heartbeat round trip")
	require.NotNil(t, got["agent-st"].MinFreeDiskBytes, "a configured-as-zero threshold must survive as 0, not become absent")
	assert.EqualValues(t, 0, *got["agent-st"].MinFreeDiskBytes)
	require.NotNil(t, got["agent-st"].ClaimBlockedByDisk)
	assert.False(t, *got["agent-st"].ClaimBlockedByDisk)
}

// An agent that stops reporting (a downgrade, or a measurement that gave up)
// must lose its series rather than keep the last reading alive: every beat
// replaces the stored reading, and a beat without one clears it.
func TestHandleAgentHeartbeat_ABeatWithoutAReadingClearsTheStoredOne(t *testing.T) {
	s, pg := newTestServer(t)
	token := issueAgentAccessForTest(t, pg, "agent-st", nil, nil)

	rr := postHeartbeatForTest(t, s, "agent-st", token, &api.HeartbeatRequest{
		ActiveRunIDs: []string{},
		Storage:      &api.AgentStorage{AgentName: "host-1", Workspace: &api.FilesystemStats{FreeBytes: 1, TotalBytes: 2}},
	})
	require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())
	got, err := pg.ListAgentStorage(context.Background(), time.Minute)
	require.NoError(t, err)
	require.Contains(t, got, "agent-st")

	// A live body with no reading, then a legacy bodyless beat: both clear.
	for name, hb := range map[string]*api.HeartbeatRequest{
		"body without storage": {ActiveRunIDs: []string{}},
		"bodyless":             nil,
	} {
		rr = postHeartbeatForTest(t, s, "agent-st", token, &api.HeartbeatRequest{
			ActiveRunIDs: []string{},
			Storage:      &api.AgentStorage{AgentName: "host-1", Workspace: &api.FilesystemStats{FreeBytes: 1, TotalBytes: 2}},
		})
		require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())

		rr = postHeartbeatForTest(t, s, "agent-st", token, hb)
		require.Equal(t, http.StatusNoContent, rr.Code, name+": "+rr.Body.String())
		got, err = pg.ListAgentStorage(context.Background(), time.Minute)
		require.NoError(t, err)
		assert.NotContains(t, got, "agent-st", name+": the stale reading was kept")
	}
}

// storageFailingStore fails only SetAgentStorage.
type storageFailingStore struct{ store.Store }

func (storageFailingStore) SetAgentStorage(context.Context, string, *api.AgentStorage) error {
	return errors.New("storage write failed")
}

// Liveness matters more than a metric: a failed storage write must not fail
// the heartbeat, or the agent would be marked stale over a metrics hiccup.
func TestHandleAgentHeartbeat_AStorageWriteFailureDoesNotFailTheHeartbeat(t *testing.T) {
	_, pg := newTestServer(t)
	s := NewServer(Config{}, storageFailingStore{Store: pg})
	t.Cleanup(s.Close)
	token := issueAgentAccessForTest(t, pg, "agent-st", nil, nil)

	rr := postHeartbeatForTest(t, s, "agent-st", token, &api.HeartbeatRequest{
		ActiveRunIDs: []string{},
		Storage:      &api.AgentStorage{AgentName: "host-1"},
	})

	require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())
}
