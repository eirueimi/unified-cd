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

func writeSSE(w http.ResponseWriter, event sseEvent) {
	b, _ := json.Marshal(event)
	fmt.Fprintf(w, "data: %s\n\n", b)
}

// handleRunEvents streams Run logs and status changes as Server-Sent Events.
// Uses Postgres LISTEN "log_appended:{runID}", so it works across multiple replicas.
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

	// Listen for new log lines via Postgres NOTIFY.
	// DB calls inside the callback use context.Background() so they continue even
	// after the HTTP request context is cancelled (client disconnect) — this prevents
	// cancelled-context errors from being silently swallowed inside the callback.
	channel := "log_appended:" + id
	_ = s.store.ListenForNotify(r.Context(), channel, func(payload string) {
		dbCtx := context.Background()
		newLines, err := s.store.TailLogs(dbCtx, id, lastSeq, 10_000)
		if err != nil {
			slog.Warn("SSE tail logs error", "runId", id, "error", err)
			return
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

		run, err := s.store.GetRun(dbCtx, id)
		if err == nil && isTerminalStatus(string(run.Status)) {
			writeSSE(w, sseEvent{Type: "status", Status: string(run.Status)})
			flusher.Flush()
		}
	})
}

func isTerminalStatus(status string) bool {
	switch status {
	case "Succeeded", "Failed", "Cancelled":
		return true
	}
	return false
}
