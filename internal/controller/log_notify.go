package controller

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/eirueimi/unified-cd/internal/store"
)

// This file implements the fan-out side of the shared listener; the single
// global Postgres NOTIFY channel it LISTENs on (store.LogAppendedChannel)
// and the dual-publish that keeps a rolling upgrade safe are defined in
// internal/store/postgres.go next to notifyLogAppended, AppendLog's and
// AppendLogs' only publishers of it.

// logNotifySub is one SSE viewer's registration with a logNotifyHub. ch is
// buffered to exactly 1: a wake-up is purely a "go check for more" signal —
// handleRunEvents always redrains from its own lastSeq regardless of what
// arrives on ch, never from the notification's content — so a second
// wake-up queued behind an unconsumed first one carries no new
// information. Capacity 1, combined with the non-blocking send in publish
// and publishAll below, is what keeps one slow viewer from ever blocking
// the shared listener goroutine, or any other viewer: the send either
// lands immediately or is dropped, it never waits on a reader.
type logNotifySub struct {
	ch chan struct{}
}

// logNotifyHub fans out "this run's logs changed" wake-ups, delivered over
// ONE shared Postgres LISTEN connection (see runLogNotifyListener), to the
// in-process SSE viewers watching that run. Before this existed, every
// viewer held its own listenPool connection for the life of its stream
// (see git history on internal/controller/sse.go); the hub turns that into
// O(1) listenPool connections per controller replica — not per viewer, not
// per run — at the cost of the bookkeeping below.
type logNotifyHub struct {
	mu   sync.Mutex
	subs map[string]map[*logNotifySub]struct{} // runID -> live subscribers
}

func newLogNotifyHub() *logNotifyHub {
	return &logNotifyHub{subs: make(map[string]map[*logNotifySub]struct{})}
}

// subscribe registers interest in runID's wake-ups. It returns the channel
// to wait on and an unsubscribe func the caller MUST defer immediately
// after a successful subscribe — before any code that could return early
// or panic (see (*Server).subscribeLogNotify's caller in sse.go) — or the
// map entry leaks forever: every future publish for that run keeps
// finding, and non-blockingly sending to, a subscriber nobody is reading
// from any more, and the empty-but-present map entry never shrinks the
// registry back down.
func (h *logNotifyHub) subscribe(runID string) (<-chan struct{}, func()) {
	sub := &logNotifySub{ch: make(chan struct{}, 1)}
	h.mu.Lock()
	set := h.subs[runID]
	if set == nil {
		set = make(map[*logNotifySub]struct{})
		h.subs[runID] = set
	}
	set[sub] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		// once.Do makes unsubscribe idempotent: sse.go calls it exactly
		// once via defer, but nothing here would break if it were called
		// twice — except deleting an already-deleted entry is harmless
		// only until two goroutines' concurrent double-unsubscribes race
		// with a fresh subscribe reusing the same runID key, which is the
		// scenario once.Do exists to rule out entirely rather than reason
		// about.
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if set := h.subs[runID]; set != nil {
				delete(set, sub)
				if len(set) == 0 {
					delete(h.subs, runID) // no empty per-run entries left sitting in the map forever
				}
			}
		})
	}
	return sub.ch, unsubscribe
}

// publish wakes every current subscriber of runID with a non-blocking send
// (see logNotifySub's doc comment for why capacity 1 + non-blocking is
// sufficient and correct here — a dropped duplicate costs nothing because
// the reader always redrains from its own lastSeq).
func (h *logNotifyHub) publish(runID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs[runID] {
		select {
		case sub.ch <- struct{}{}:
		default:
		}
	}
}

// publishAll wakes every subscriber of every run. It exists for one
// reason: closing the gap a dropped-and-reconnected LISTEN connection
// leaves behind (see runLogNotifyListener). A NOTIFY sent to a channel
// with no session currently listening is not queued for later delivery —
// PostgreSQL just drops it — so any AppendLog/AppendLogs that lands while
// the shared connection is down would otherwise never wake anyone for
// that run. Every viewer redrains from its own lastSeq regardless of why
// it woke up, so waking one that has nothing new is a harmless no-op
// TailLogs call, not a correctness risk, which is what makes "wake
// everyone, unconditionally, we don't know what we missed" an acceptable
// hammer here.
//
// This is called once per (re)connect attempt in runLogNotifyListener, not
// on a standing timer: the only window where a NOTIFY can be truly lost is
// while the single shared LISTEN session is down, and a (re)connect
// attempt is exactly what brackets that window. A wall-clock ticker would
// additionally poll during the (much longer) time the connection is
// healthy, where nothing is lost and nothing needs catching up — paying a
// continuous per-viewer DB-read tax for a risk that provably does not
// exist in that stretch.
func (h *logNotifyHub) publishAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, set := range h.subs {
		for sub := range set {
			select {
			case sub.ch <- struct{}{}:
			default:
			}
		}
	}
}

// runLogNotifyListener holds the ONE Postgres connection this controller
// replica spends on live log wake-ups (down from one per SSE viewer — see
// logNotifyHub's doc comment) and keeps it LISTENing on
// store.LogAppendedChannel for as long as ctx is alive, reconnecting on any
// error.
//
// Each replica runs its own instance of this loop (started lazily — see
// (*Server).subscribeLogNotify) and each gets every NOTIFY independently:
// PostgreSQL delivers a NOTIFY to every session currently listening on the
// channel, not to one arbitrarily-chosen listener, so N replicas cost N
// listenPool connections total (one each, unavoidably — that is the
// nature of LISTEN/NOTIFY, not a limitation of this design), and no
// replica double-delivers to its OWN viewers: one callback fire per NOTIFY
// on its own connection, fanned out once per subscriber via logNotifyHub —
// exactly like the one-viewer-one-LISTEN code this replaces, just shared
// across every viewer that replica happens to be serving.
//
// Rolling-upgrade note: this listens ONLY on the new global channel. An
// old (pre-multiplex) controller replica mid-rollout still LISTENs on its
// own per-run "log_appended:{runID}" channel and neither knows nor cares
// that this channel exists — see the dual pg_notify in
// internal/store/postgres.go's AppendLog/AppendLogs for why both channel
// forms are populated for the duration of a rollout, so that neither an
// old nor a new replica's viewers go dark depending on which version
// happens to serve the write that would have woken them.
func runLogNotifyListener(ctx context.Context, st store.Store, hub *logNotifyHub) {
	// Backoff, not a tight retry loop: ListenForNotify's Acquire competes
	// for the same listenPool as every other replica reconnecting after a
	// shared outage (e.g. a Postgres restart), and a tight loop across
	// every replica at once would just be its own minor thundering herd.
	const reconnectDelay = 2 * time.Second
	first := true
	for {
		if !first {
			select {
			case <-ctx.Done():
				return
			case <-time.After(reconnectDelay):
			}
		}
		first = false

		// See publishAll's doc comment: this brackets the gap a reconnect
		// leaves, by construction, whether this is the very first connect
		// (nothing to catch up on, so a harmless no-op) or a reconnect
		// after a drop (where it is the entire point).
		hub.publishAll()

		err := st.ListenForNotify(ctx, store.LogAppendedChannel, func(payload string) {
			runID := strings.TrimSpace(payload)
			if runID == "" {
				// Defensive only: every publisher of this channel is this
				// package's own AppendLog/AppendLogs, which always sends a
				// non-empty run ID. Nothing today can produce this, but
				// silently dropping an empty wake-up costs nothing, and is
				// cheaper than trusting an invariant this file does not
				// itself enforce.
				return
			}
			hub.publish(runID)
		})
		if ctx.Err() != nil {
			return // clean shutdown: (*Server).Close cancelled ctx
		}
		if errors.Is(err, store.ErrListenPoolExhausted) {
			// The bounded acquire from PR #166 is what turns this into an
			// error at all instead of an invisible, indefinite block inside
			// Acquire — keep it that way. What changed with the shared
			// listener is only the blast radius and the response: exhaustion
			// is no longer one viewer's stream dying, it is this replica
			// having no live log wake-ups at all until a listenPool
			// connection frees up, so it is logged at error level here (the
			// store logs the pool stats alongside) and retried on the same
			// backoff as any other connection loss rather than surfaced to
			// one arbitrary viewer.
			slog.Error("log-notify listener could not acquire a listen-pool connection; live log updates are stalled on this replica until it can",
				"error", err, "retryIn", reconnectDelay)
			continue
		}
		slog.Warn("log-notify listener lost its connection; reconnecting", "error", err)
	}
}
