package agent

import (
	"context"
	"log/slog"
	"time"
)

// DefaultHeartbeatInterval is how often an agent refreshes its liveness.
const DefaultHeartbeatInterval = 15 * time.Second

// heartbeatTimeout bounds a single heartbeat attempt. It is well below the
// reaper's staleAfter so a stalled request cannot consume the whole staleness
// window — a saturated agent still gets several attempts within staleAfter.
const heartbeatTimeout = 10 * time.Second

// StartHeartbeat spawns a goroutine that periodically calls client.Heartbeat so
// the controller's last_seen_at stays fresh even when all execution slots are
// busy (and claim polling has paused). Best-effort: a failed heartbeat is logged
// and retried on the next tick. Returns immediately; the goroutine exits when ctx
// is done. The returned channel is closed once the goroutine has fully stopped,
// letting a caller cancel ctx and then join it to guarantee no further heartbeat
// fires (e.g. Agent.Run joins before returning so a beat can't outlive shutdown).
//
// activeRunIDs is called once per beat to get the current set of runs this
// agent process has in flight (see RunSet.Snapshot); its result is forwarded
// to client.Heartbeat unchanged so the controller can see which runs are
// still alive here. A nil provider (defensive; every production caller
// supplies one) is treated as "no data" and sends a bodyless legacy beat.
func StartHeartbeat(ctx context.Context, client *Client, agentID string, interval time.Duration, activeRunIDs func() []string) <-chan struct{} {
	if interval <= 0 {
		interval = DefaultHeartbeatInterval
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				hbCtx, cancel := context.WithTimeout(ctx, heartbeatTimeout)
				var ids []string
				if activeRunIDs != nil {
					ids = activeRunIDs()
				}
				err := client.Heartbeat(hbCtx, agentID, ids)
				cancel()
				if err != nil && ctx.Err() == nil {
					slog.Warn("agent heartbeat failed", "agentId", agentID, "error", err)
				}
			}
		}
	}()
	return done
}
