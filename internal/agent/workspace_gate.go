package agent

import (
	"log/slog"
	"sync"
	"time"
)

// workspaceGateWait is how long a claim slot sleeps after finding the
// workspace below minFreeDisk before it looks again. A var so a test can
// shorten it.
var workspaceGateWait = 2 * time.Second

// workspaceGateReminder is how often, while the workspace stays below the
// minimum, the agent repeats that it is not claiming.
const workspaceGateReminder = time.Minute

// workspaceGate de-duplicates the workspace gate's log lines. Its state is
// agent-wide: every claim slot (the normal ones and the detached ones) checks
// the same filesystem, so the transitions are logged once for the agent, not
// once per slot per check. The zero value is ready to use.
type workspaceGate struct {
	mu       sync.Mutex
	blocked  bool
	lastWarn time.Time
	now      func() time.Time // time.Now when nil; a test seam
}

func (g *workspaceGate) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

// below records that a slot found free space under the minimum. It logs one
// WARN when the agent enters the blocked state (attrs carry the numbers) and a
// reminder at most once per workspaceGateReminder while it stays blocked.
func (g *workspaceGate) below(attrs ...any) {
	g.mu.Lock()
	now := g.clock()
	entering := !g.blocked
	remind := !entering && now.Sub(g.lastWarn) >= workspaceGateReminder
	if entering || remind {
		g.blocked = true
		g.lastWarn = now
	}
	g.mu.Unlock()

	// Logged after the unlock: a stderr that applies backpressure must not hold
	// the mutex every slot takes.
	switch {
	case entering:
		slog.Warn("free disk space below minimum, skipping claim", attrs...)
	case remind:
		slog.Warn("free disk space still below minimum, not claiming", attrs...)
	}
}

// ok records that a slot found enough free space (or could not measure it,
// which fails open). It logs one INFO when the agent leaves the blocked state.
func (g *workspaceGate) ok() {
	g.mu.Lock()
	was := g.blocked
	g.blocked = false
	g.mu.Unlock()
	if was {
		slog.Info("free disk space is no longer below minimum, claiming again")
	}
}
