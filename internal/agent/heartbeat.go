package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
)

// DefaultHeartbeatInterval is how often an agent refreshes its liveness.
const DefaultHeartbeatInterval = 15 * time.Second

// heartbeatTimeout bounds a single heartbeat attempt. It is well below the
// reaper's staleAfter so a stalled request cannot consume the whole staleness
// window — a saturated agent still gets several attempts within staleAfter.
const heartbeatTimeout = 10 * time.Second

// BuildHeartbeat adapts the three per-beat providers to the request builder
// StartHeartbeat takes.
//
// activeRunIDs is called once per beat to get the current set of runs this
// agent process has in flight (see RunSet.Snapshot); its result is forwarded
// unchanged so the controller can see which runs are still alive here. A nil
// provider (defensive; every production caller supplies one), or one that
// returns nil, yields a nil request: a bodyless legacy beat.
//
// podBindings is likewise called once per beat and forwarded unchanged as
// api.HeartbeatRequest.PodBindings; it exists for the Kubernetes agent (see
// api.HeartbeatRequest.PodBindings' doc comment) and every host-agent caller
// passes nil, which simply sends no bindings.
//
// storage is likewise called once per beat and forwarded unchanged as
// api.HeartbeatRequest.Storage, so the controller holds the reading from the
// beat it last heard. A nil provider, or one that returns nil, sends no
// reading, and the controller drops whatever it held for the agent. Like
// podBindings, it rides only on a beat that has a body.
func BuildHeartbeat(activeRunIDs func() []string, podBindings func() map[string]api.PodBinding, storage func() *api.AgentStorage) func() *api.HeartbeatRequest {
	return func() *api.HeartbeatRequest {
		var ids []string
		if activeRunIDs != nil {
			ids = activeRunIDs()
		}
		var bindings map[string]api.PodBinding
		if podBindings != nil {
			bindings = podBindings()
		}
		var reading *api.AgentStorage
		if storage != nil {
			reading = storage()
		}
		// A nil id slice is the legacy bodyless beat; bindings and the
		// reading ride only on a beat that has a body.
		if ids == nil {
			return nil
		}
		return &api.HeartbeatRequest{ActiveRunIDs: ids, PodBindings: bindings, Storage: reading}
	}
}

// StartHeartbeat spawns a goroutine that periodically calls client.Heartbeat so
// the controller's last_seen_at stays fresh even when all execution slots are
// busy (and claim polling has paused). The first beat is sent immediately, the
// rest every interval. Best-effort: a failed heartbeat is logged and retried on
// the next tick. Returns immediately; the goroutine exits when ctx is done. The
// returned channel is closed once the goroutine has fully stopped, letting a
// caller cancel ctx and then join it to guarantee no further heartbeat fires
// (e.g. Agent.Run joins before returning so a beat can't outlive shutdown).
//
// build is called once per beat to make the request (nil sends a bodyless
// legacy beat; see BuildHeartbeat).
//
// onResponse is called with each successfully decoded answer, and only then: on
// a heartbeat error (the agent keeps whatever it last applied) and on an empty
// 204 answer (an old controller that does not deliver AgentVolumes; one warning
// is logged for the lifetime of this goroutine) it is not called. A nil
// onResponse discards answers.
func StartHeartbeat(ctx context.Context, client *Client, agentID string, interval time.Duration, build func() *api.HeartbeatRequest, onResponse func(*api.HeartbeatResponse)) <-chan struct{} {
	if interval <= 0 {
		interval = DefaultHeartbeatInterval
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		warned204 := false
		beat := func() {
			hbCtx, cancel := context.WithTimeout(ctx, heartbeatTimeout)
			defer cancel()
			var hb *api.HeartbeatRequest
			if build != nil {
				hb = build()
			}
			resp, err := client.Heartbeat(hbCtx, agentID, hb)
			switch {
			case err != nil:
				if ctx.Err() == nil {
					slog.Warn("agent heartbeat failed", "agentId", agentID, "error", err)
				}
			case resp == nil:
				if !warned204 {
					warned204 = true
					slog.Warn("controller does not deliver AgentVolumes; upgrade the controller", "agentId", agentID)
				}
			case onResponse != nil:
				onResponse(resp)
			}
		}
		beat()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				beat()
			}
		}
	}()
	return done
}
