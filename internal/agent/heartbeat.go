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
// is done.
func StartHeartbeat(ctx context.Context, client *Client, agentID string, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultHeartbeatInterval
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				hbCtx, cancel := context.WithTimeout(ctx, heartbeatTimeout)
				err := client.Heartbeat(hbCtx, agentID)
				cancel()
				if err != nil && ctx.Err() == nil {
					slog.Warn("agent heartbeat failed", "agentId", agentID, "error", err)
				}
			}
		}
	}()
}
