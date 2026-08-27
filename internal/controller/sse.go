package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// sseEvent no longer carries an "error" type: the only producer of one was
// handleRunEvents' own store.ErrListenPoolExhausted branch, back when this
// handler called ListenForNotify itself (PR #166). Listen-pool exhaustion is
// now a replica-wide condition handled — and logged — by the single shared
// listener in log_notify.go, which retries instead of tearing a viewer's
// stream down, so there is nothing left for a per-viewer error event to say.
type sseEvent struct {
	Type      string `json:"type"` // "log", "status", or "truncated"
	Seq       int64  `json:"seq,omitempty"`
	StepIndex int    `json:"stepIndex"` // must not use omitempty: index 0 (first step) is a valid value
	Stream    string `json:"stream,omitempty"`
	Line      string `json:"line,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	Status    string `json:"status,omitempty"`
}

// sseBackfillLimit bounds how many existing log lines are replayed when a client
// connects. Huge logs (e.g. Unity's `-logFile -` streams tens of thousands of
// lines) otherwise cost a multi-megabyte burst to transfer and parse. It is a
// var (not a const) so tests can shrink it. Live lines after connect are not
// affected by this cap.
//
// This backfill is only the initial window: the client can browse the full
// log regardless of size via the windowed viewer (GET /runs/{id}/logs/stats,
// /logs/range, /logs/search), which fetches ranges by row number as the user
// scrolls. This cap does not limit what's reachable, only what's replayed
// up front over SSE.
var sseBackfillLimit = 10_000

// sseDrainLimit bounds each TailLogs call the live-notify callback makes, for
// the same reason sseBackfillLimit bounds the initial backfill: an unbounded
// read could return an arbitrarily large result. Before batched log
// ingestion this bound was harmless — one pg_notify per line meant a
// backlog beyond the cap always had another wake-up coming to drain the
// rest. A batch can now carry more than this many lines for one run in a
// single notification, so the callback below loops, redraining immediately
// whenever a drain returns a full sseDrainLimit rows, instead of waiting for
// the run's next batch (which may never come, if this was the last one) to
// deliver the remainder. It is a var (not a const) so tests can shrink it.
var sseDrainLimit = 10_000

func writeSSE(w http.ResponseWriter, event sseEvent) {
	b, _ := json.Marshal(event)
	fmt.Fprintf(w, "data: %s\n\n", b)
}

// handleRunEvents streams Run logs and status changes as Server-Sent Events.
// Wake-ups ride a single shared Postgres LISTEN "log_appended" connection
// per controller replica, fanned out in-process to whichever viewers care
// (see log_notify.go) — not one LISTEN per viewer as before. It still works
// across multiple replicas for the same reason it always did: every
// replica runs its own listener, and NOTIFY reaches every session
// currently listening on the channel, not just one.
func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// Replay the most recent existing log lines first. For very large logs we cap
	// the backfill and keep the TAIL — the end of the log, where failures usually
	// are — rather than the head. If older lines were dropped we tell the client
	// (via a "truncated" event) so it can surface that the view is not complete.
	var lastSeq int64
	// READ before CHECK: read the DB first (as pre-trim code always did),
	// then check whether the run is trimmed. If a trim commits in between,
	// this DB read still executed strictly before that commit — its result
	// is simply superseded below by the archive read; checking trimmed
	// first would risk a trim landing in the gap and backfilling an empty
	// DB read from rows that were just deleted.
	dbExisting, dbErr := s.store.TailLogsRecent(r.Context(), id, sseBackfillLimit+1)
	if dbErr != nil {
		http.Error(w, dbErr.Error(), http.StatusInternalServerError)
		return
	}
	trimmed, err := s.logsTrimmed(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	existing := dbExisting
	if trimmed {
		all, aerr := s.archLogs.lines(r.Context(), id)
		if aerr != nil {
			http.Error(w, "log archive unavailable: "+aerr.Error(), http.StatusServiceUnavailable)
			return
		}
		existing = tailRecent(all, sseBackfillLimit+1)
	}
	if len(existing) > sseBackfillLimit {
		existing = existing[len(existing)-sseBackfillLimit:]
		writeSSE(w, sseEvent{Type: "truncated"})
	}
	for _, l := range existing {
		writeSSE(w, sseEvent{
			Type:      "log",
			Seq:       l.Seq,
			StepIndex: l.StepIndex,
			Stream:    l.Stream,
			Line:      l.Line,
			Timestamp: l.Timestamp.Format(time.RFC3339Nano),
		})
		lastSeq = l.Seq
	}
	flusher.Flush()

	// Check whether the Run is already in a terminal state.
	run, err := s.store.GetRun(r.Context(), id)
	if err == nil && isTerminalStatus(string(run.Status)) {
		writeSSE(w, sseEvent{Type: "status", Status: string(run.Status)})
		flusher.Flush()
		return
	}

	// Wake up whenever this run's logs change, via the shared logNotifyHub
	// fed by ONE Postgres LISTEN connection per controller replica (see
	// internal/controller/log_notify.go) instead of this handler holding
	// its own listenPool connection for the life of the stream, as every
	// other concurrent viewer of any run used to.
	//
	// unsubscribe is deferred immediately, before any code below that could
	// return early, so a subscriber is removed from the hub on every exit
	// path from here on — including a panic — never just the happy path.
	//
	// DB calls inside the loop use context.Background() so they continue even
	// after the HTTP request context is cancelled (client disconnect) — this prevents
	// cancelled-context errors from being silently swallowed before the loop
	// observes r.Context().Done() below and returns.
	wake, unsubscribe := s.subscribeLogNotify(id)
	defer unsubscribe()
outer:
	for {
		select {
		case <-r.Context().Done():
			return
		case <-wake:
		}

		dbCtx := context.Background()
		// Redrain immediately whenever a pass comes back full: a batch may
		// have carried more lines for this run than one drain returns, and
		// this may be its last notification, so there is no guarantee a
		// later wake-up drains the remainder. Stop as soon as a pass returns
		// under the limit — that means the backlog is exhausted.
		for {
			newLines, err := s.store.TailLogs(dbCtx, id, lastSeq, sseDrainLimit)
			if err != nil {
				slog.Warn("SSE tail logs error", "runId", id, "error", err)
				// Matches the pre-hub behavior exactly: a drain error skips
				// the rest of THIS wake-up (including the terminal-status
				// check below) but does not end the stream — go back to
				// waiting for the next wake-up, the same way returning from
				// ListenForNotify's callback used to just let its loop wait
				// for the next notification instead of unwinding the whole
				// handler.
				continue outer
			}
			for _, l := range newLines {
				writeSSE(w, sseEvent{
					Type:      "log",
					Seq:       l.Seq,
					StepIndex: l.StepIndex,
					Stream:    l.Stream,
					Line:      l.Line,
					Timestamp: l.Timestamp.Format(time.RFC3339Nano),
				})
				lastSeq = l.Seq
			}
			flusher.Flush()
			// sseDrainLimit <= 0 must still terminate the loop: with the
			// limit at its production value (10,000) this is just the normal
			// "pass came back under the cap" exit, but the var is
			// test-settable, and a non-positive value would otherwise spin
			// forever — TailLogs(..., 0) returns zero rows every call
			// (0 < 0 is false), re-querying with no progress and no way out.
			if len(newLines) < sseDrainLimit || sseDrainLimit <= 0 {
				break
			}
		}

		run, err := s.store.GetRun(dbCtx, id)
		if err == nil && isTerminalStatus(string(run.Status)) {
			writeSSE(w, sseEvent{Type: "status", Status: string(run.Status)})
			flusher.Flush()
		}
	}
}

func isTerminalStatus(status string) bool {
	switch status {
	case "Succeeded", "Failed", "Cancelled":
		return true
	}
	return false
}
