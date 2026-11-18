package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

// hbController answers heartbeats with a switchable behavior and records the
// bodies it received.
type hbController struct {
	mu       sync.Mutex
	mode     string // "set", "204", "500", "garbage"
	set      []api.DesiredVolume
	revision string
	bodies   []api.HeartbeatRequest
	srv      *httptest.Server
}

func newHBController(t *testing.T) *hbController {
	t.Helper()
	f := &hbController{mode: "set", revision: "r1"}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/heartbeat") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body api.HeartbeatRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		mode, set, rev := f.mode, f.set, f.revision
		f.mu.Unlock()
		switch mode {
		case "204":
			w.WriteHeader(http.StatusNoContent)
		case "500":
			http.Error(w, "boom", http.StatusInternalServerError)
		case "nokey":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"revision":"x"}`))
		case "garbage":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("{not json"))
		default:
			if set == nil {
				set = []api.DesiredVolume{}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(api.HeartbeatResponse{Volumes: set, Revision: rev})
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *hbController) setMode(mode string) {
	f.mu.Lock()
	f.mode = mode
	f.mu.Unlock()
}

func (f *hbController) beats() []api.HeartbeatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]api.HeartbeatRequest(nil), f.bodies...)
}

// waitBeats blocks until the controller has seen n more beats than before.
func (f *hbController) waitBeats(t *testing.T, n int) {
	t.Helper()
	want := len(f.beats()) + n
	require.Eventually(t, func() bool { return len(f.beats()) >= want }, 3*time.Second, 5*time.Millisecond)
}

func newVolumeTestAgent(t *testing.T, f *hbController) (*Agent, string) {
	t.Helper()
	vm, base := newEmptyVM(t)
	return &Agent{ID: "a1", Client: NewClient(f.srv.URL, "t"), Volumes: vm}, base
}

func startAgentHeartbeat(t *testing.T, a *Agent, interval time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := StartHeartbeat(ctx, a.Client, a.ID, interval, a.buildHeartbeat(func() []string { return []string{} }, nil), a.onHeartbeat)
	t.Cleanup(func() { cancel(); <-done })
}

func TestFirstHeartbeatIsImmediate(t *testing.T) {
	f := newHBController(t)
	a, _ := newVolumeTestAgent(t, f)
	startAgentHeartbeat(t, a, time.Hour) // no tick will fire during the test
	require.Eventually(t, func() bool { return len(f.beats()) == 1 }, 2*time.Second, 5*time.Millisecond)
}

func TestAgentAppliesHeartbeatSet(t *testing.T) {
	f := newHBController(t)
	f.set = []api.DesiredVolume{dv("u", "exclusive", dsl.ReclaimRetain)}
	a, _ := newVolumeTestAgent(t, f)
	startAgentHeartbeat(t, a, 20*time.Millisecond)

	require.Eventually(t, func() bool { return len(a.Volumes.Names()) == 1 }, 2*time.Second, 5*time.Millisecond)
	require.Equal(t, []string{"u"}, a.Volumes.Names())
	// The beat after the Apply reports its status.
	require.Eventually(t, func() bool {
		bs := f.beats()
		last := bs[len(bs)-1]
		return len(last.VolumeStatus) == 1 && last.VolumeStatus[0] == api.AgentVolumeStatus{Name: "u", Ready: true}
	}, 2*time.Second, 5*time.Millisecond)
	require.Empty(t, f.beats()[0].VolumeStatus, "the first beat has nothing to report yet")
}

func TestAgentKeepsLastSetOnHeartbeatError(t *testing.T) {
	f := newHBController(t)
	f.set = []api.DesiredVolume{dv("u", "exclusive", dsl.ReclaimRetain)}
	a, _ := newVolumeTestAgent(t, f)
	startAgentHeartbeat(t, a, 20*time.Millisecond)
	require.Eventually(t, func() bool { return len(a.Volumes.Names()) == 1 }, 2*time.Second, 5*time.Millisecond)

	f.setMode("500")
	f.waitBeats(t, 4)
	require.Equal(t, []string{"u"}, a.Volumes.Names())
}

// Apply with an empty set reclaims every Delete volume. A beat that did not
// deliver a set (error, 204, undecodable answer) must therefore never reach it.
func TestNoApplyWithoutADeliveredSet(t *testing.T) {
	for _, mode := range []string{"500", "204", "garbage", "nokey"} {
		t.Run(mode, func(t *testing.T) {
			f := newHBController(t)
			f.set = []api.DesiredVolume{dv("d", "exclusive", dsl.ReclaimDelete)}
			a, _ := newVolumeTestAgent(t, f)
			startAgentHeartbeat(t, a, 20*time.Millisecond)
			require.Eventually(t, func() bool { return len(a.Volumes.Names()) == 1 }, 2*time.Second, 5*time.Millisecond)
			dir := a.Volumes.PathOf("d")
			require.DirExists(t, dir)

			f.setMode(mode)
			f.waitBeats(t, 4)

			require.DirExists(t, dir, "a beat without a delivered set must not reclaim")
			require.Equal(t, []string{"d"}, a.Volumes.Names())
			_, err := os.Stat(dir)
			require.NoError(t, err)
		})
	}
}

// A delivered empty set is authoritative: it does reclaim (contrast with above).
func TestDeliveredEmptySetReclaimsDelete(t *testing.T) {
	f := newHBController(t)
	f.set = []api.DesiredVolume{dv("d", "exclusive", dsl.ReclaimDelete)}
	a, _ := newVolumeTestAgent(t, f)
	startAgentHeartbeat(t, a, 20*time.Millisecond)
	require.Eventually(t, func() bool { return len(a.Volumes.Names()) == 1 }, 2*time.Second, 5*time.Millisecond)
	dir := a.Volumes.PathOf("d")

	f.mu.Lock()
	f.set, f.revision = nil, "r2"
	f.mu.Unlock()
	require.Eventually(t, func() bool { _, err := os.Stat(dir); return err != nil }, 2*time.Second, 5*time.Millisecond)
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestAgentHeartbeat204Warns(t *testing.T) {
	buf := &syncBuf{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	f := newHBController(t)
	f.setMode("204")
	a, _ := newVolumeTestAgent(t, f)
	startAgentHeartbeat(t, a, 20*time.Millisecond)
	f.waitBeats(t, 5)

	require.Empty(t, a.Volumes.Names())
	require.False(t, a.isVolumesKnown())
	require.Equal(t, 1, strings.Count(buf.String(), "controller does not deliver AgentVolumes; upgrade the controller"), buf.String())
}

func TestClaimSendsVolumesKnown(t *testing.T) {
	f := newHBController(t)
	a, _ := newVolumeTestAgent(t, f)

	req := a.claimRequest()
	require.False(t, req.VolumesKnown)
	require.Empty(t, req.Volumes)

	// An empty delivered set is authoritative too.
	a.onHeartbeat(&api.HeartbeatResponse{Volumes: []api.DesiredVolume{}, Revision: "r0"})
	require.True(t, a.claimRequest().VolumesKnown)

	a.onHeartbeat(&api.HeartbeatResponse{Volumes: []api.DesiredVolume{dv("u", "shared", dsl.ReclaimRetain)}, Revision: "r1"})
	req = a.claimRequest()
	require.True(t, req.VolumesKnown)
	require.Equal(t, []string{"u"}, req.Volumes)
}
