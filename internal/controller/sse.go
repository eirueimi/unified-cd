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
	existing, err := s.store.TailLogsRecent(r.Context(), id, sseBackfillLimit+1)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
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
