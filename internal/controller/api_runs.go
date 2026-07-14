package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/go-chi/chi/v5"
)

// handleTriggerRun creates a new Run and returns it in Pending state.
func (s *Server) handleTriggerRun(w http.ResponseWriter, r *http.Request) {
	var req api.TriggerRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.JobName == "" {
		http.Error(w, "jobName is required", http.StatusBadRequest)
		return
	}
	job, err := s.store.GetJob(r.Context(), req.JobName)
	if err != nil {
		http.Error(w, "job not found: "+req.JobName, http.StatusNotFound)
		return
	}
	// Extract the agentSelector from the stored spec JSON.
	var spec dsl.Spec
	agentSelector := []string{}
	if err := json.Unmarshal(job.Spec, &spec); err == nil {
		agentSelector = spec.AgentSelector
	}
	params, err := resolveParams(spec.Params.Inputs, req.Params)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	agentSelector, err = dsl.ExpandAgentSelector(agentSelector, params)
	if err != nil {
		http.Error(w, "agentSelector: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Infer the capability a run of this spec needs from an agent (native /
	// container / pod). A podTemplate that uses features the host agent's
	// claim pod cannot honor (named template, override, pod-level spec beyond
	// containers, or a host-unsupported container field) can only run on
	// Kubernetes, so RequiredCaps yields "pod" for it — the agent-side
	// capability match (ClaimNextRun) then restricts the run to a
	// pod-capable agent instead of the old blanket "kubernetes" label pin. A
	// host-runnable podTemplate (e.g. plain name/image containers,
	// workspace.pvc — which the host degrades to a bind mount) yields
	// "container" and is left to route by the author's agentSelector, so it
	// can run on a standard agent too.
	requiredCaps := dsl.RequiredCaps(spec)
	triggeredBy := "api"
	if p, ok := principalFromContext(r.Context()); ok && p.Name != "" {
		triggeredBy = p.Name
	}
	run, err := s.store.CreateRun(r.Context(), job.Name, params, job.Spec, agentSelector, requiredCaps, triggeredBy)
	if err != nil {
		http.Error(w, "create run: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// handleReplayRun creates a new run from an existing run's ORIGINAL spec
// snapshot (runs.spec) and params, rather than the job's current spec. Use it
// to reproduce a run exactly as it executed, even if the job YAML has since
// been re-applied. (The web "Rerun" button re-triggers with the LATEST job
// spec; replay is the point-in-time counterpart.)
func (s *Server) handleReplayRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	orig, err := s.store.GetRun(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrRunNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	specJSON, err := s.store.GetRunSpec(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(specJSON) == 0 {
		http.Error(w, "run has no stored spec to replay", http.StatusInternalServerError)
		return
	}
	// Derive routing (agentSelector, required capability) from the SNAPSHOT
	// spec, exactly as handleTriggerRun does from the job spec — so the replay
	// routes the same way the original run did, independent of the job's
	// current definition. Reuse the original run's already-resolved params.
	var spec dsl.Spec
	agentSelector := []string{}
	if json.Unmarshal(specJSON, &spec) == nil {
		agentSelector = spec.AgentSelector
	}
	agentSelector, err = dsl.ExpandAgentSelector(agentSelector, orig.Params)
	if err != nil {
		http.Error(w, "agentSelector: "+err.Error(), http.StatusBadRequest)
		return
	}
	requiredCaps := dsl.RequiredCaps(spec)
	run, err := s.store.CreateRun(r.Context(), orig.JobName, orig.Params, specJSON, agentSelector, requiredCaps, "replay:"+id)
	if err != nil {
		http.Error(w, "create run: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// handleGetRun returns the Run with the given ID.
func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	run, err := s.store.GetRun(r.Context(), id)
	if err != nil {
		// Only a genuine "not found" is a 404. Transient DB errors (pool
		// exhaustion, timeouts, dropped connections) must surface as 500 so
		// that clients such as the k8s pod-GC do not treat a still-running
		// run as gone and reap its pod.
		if errors.Is(err, store.ErrRunNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if cb, cErr := s.store.GetRunParent(r.Context(), id); cErr != nil {
		slog.Warn("get run parent failed", "runId", id, "error", cErr)
	} else if cb != nil {
		run.CalledBy = cb
	}
	writeJSON(w, http.StatusOK, run)
}

// handleGetRunYAML returns the YAML definition of the specified Run.
func (s *Server) handleGetRunYAML(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	specJSON, err := s.store.GetRunSpec(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	yamlBytes, err := specJSONToYAML(specJSON)
	if err != nil {
		http.Error(w, "render yaml: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(yamlBytes)
}

// handleTailLogs returns logs for the specified Run, starting after the sequence number given by the after query parameter.
func (s *Server) handleTailLogs(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	afterStr := r.URL.Query().Get("after")
	var after int64
	_, _ = fmt.Sscanf(afterStr, "%d", &after)
	trimmed, err := s.logsTrimmed(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var lines []api.LogLine
	if trimmed {
		all, err := s.archLogs.lines(r.Context(), id)
		if err != nil {
			http.Error(w, "log archive unavailable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		lines = tailAfter(all, after, 1000)
	} else {
		lines, err = s.store.TailLogs(r.Context(), id, after, 1000)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if lines == nil {
		lines = []api.LogLine{}
	}
	writeJSON(w, http.StatusOK, lines)
}

// handleGetRunOutputs returns the outputs of the specified Run.
func (s *Server) handleGetRunOutputs(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	outputs, err := s.store.GetRunOutputs(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if outputs == nil {
		outputs = map[string]string{}
	}
	writeJSON(w, http.StatusOK, api.RunOutputs{RunID: id, Outputs: outputs})
}

// handleListRunsByJob returns the most recent Runs for the specified Job.
func (s *Server) handleListRunsByJob(w http.ResponseWriter, r *http.Request) {
	jobName := r.URL.Query().Get("jobName")
	if jobName == "" {
		http.Error(w, "jobName query parameter is required", http.StatusBadRequest)
		return
	}
	runs, err := s.store.ListRunsByJob(r.Context(), jobName, 50)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if runs == nil {
		runs = []api.Run{}
	}
	writeJSON(w, http.StatusOK, runs)
}

// handleListActiveRuns returns all Runs in Pending, Queued, or Running state across all jobs.
func (s *Server) handleListActiveRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.store.ListActiveRuns(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if runs == nil {
		runs = []api.Run{}
	}
	writeJSON(w, http.StatusOK, runs)
}

// handleGetRunSteps returns the list of steps for the specified Run.
func (s *Server) handleGetRunSteps(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	steps, err := s.store.GetRunSteps(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Overlay onto the planned step list so not-yet-run steps show as Pending.
	// Best-effort: if the spec is missing/unparseable, fall back to reported.
	if specJSON, sErr := s.store.GetRunSpec(r.Context(), id); sErr == nil && len(specJSON) > 0 {
		var spec dsl.Spec
		if json.Unmarshal(specJSON, &spec) == nil {
			steps = mergedRunSteps(steps, spec)
		}
	}
	// Overlay live sidecar phase/exit-code (from Task 5's sidecar_status
	// reports) onto the sidecar pseudo-steps synthesized by plannedSteps.
	// Best-effort: a store error here should not fail the whole steps response.
	if scs, scErr := s.store.GetSidecarStatuses(r.Context(), id); scErr == nil {
		byIdx := map[int]api.SidecarStatusRequest{}
		for _, sc := range scs {
			byIdx[sc.Index] = sc
		}
		for i := range steps {
			if steps[i].Kind != "sidecar" {
				continue
			}
			if sc, ok := byIdx[steps[i].Index]; ok {
				steps[i].Status = sc.Phase // "running" / "exited"
				steps[i].ExitCode = sc.ExitCode
			}
		}
	}
	// The artifact/cache sidecar is injected per-agent (not in the run spec), so
	// surface it as a Sidecars entry only when it actually produced output (log
	// lines at dsl.ArtifactLogIndex). It has no phase report, so it carries no
	// live status.
	if cnt, _, _, cErr := s.store.CountLogs(r.Context(), id, []int{dsl.ArtifactLogIndex}); cErr == nil && cnt > 0 {
		present := false
		for i := range steps {
			if steps[i].Index == dsl.ArtifactLogIndex {
				present = true
				break
			}
		}
		if !present {
			steps = append(steps, api.StepReport{
				Index:   dsl.ArtifactLogIndex,
				Name:    "artifact",
				Kind:    "sidecar",
				Section: "sidecars",
			})
		}
	}
	if steps == nil {
		steps = []api.StepReport{}
	}
	writeJSON(w, http.StatusOK, steps)
}

// handleCancelRun transitions the specified Run to the Cancelled state.
func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.store.MarkRunFinished(r.Context(), id, api.RunCancelled); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Cascade cancellation to descendant runs spawned via call: steps, so a
	// child job doesn't keep running after its parent is cancelled. Best-effort:
	// the executing agent picks up the Cancelled status the same way it does for
	// a directly-cancelled run.
	cancelDescendantRuns(r.Context(), s.store, id)
	w.WriteHeader(http.StatusNoContent)
}

// cancelDescendantRuns walks the parent→child run tree (call: steps, linked via
// step_reports.child_run_id) breadth-first and marks every still-active
// descendant Cancelled. A visited set guards against cycles. Used when a parent
// run reaches a terminal Failed/Cancelled state so its children don't linger.
func cancelDescendantRuns(ctx context.Context, st store.Store, rootID string) {
	visited := map[string]bool{rootID: true}
	queue := []string{rootID}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		children, err := st.ListChildRunIDs(ctx, parent)
		if err != nil {
			slog.Warn("cascade cancel: list child runs failed", "run", parent, "error", err)
			continue
		}
		for _, child := range children {
			if visited[child] {
				continue
			}
			visited[child] = true
			if err := st.MarkRunFinished(ctx, child, api.RunCancelled); err != nil {
				slog.Warn("cascade cancel: mark cancelled failed", "run", child, "parent", parent, "error", err)
			}
			queue = append(queue, child)
		}
	}
}

// handleDeleteRun deletes a Run that has reached a terminal state (Succeeded/Failed/Cancelled).
// Returns 409 if the Run is still in progress.
func (s *Server) handleDeleteRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	run, err := s.store.GetRun(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrRunNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	switch run.Status {
	case api.RunSucceeded, api.RunFailed, api.RunCancelled:
	default:
		http.Error(w, fmt.Sprintf("run %s is still %s; only terminal runs can be deleted", id, run.Status), http.StatusConflict)
		return
	}
	if err := deleteRunEverywhere(r.Context(), s.store, s.objStore, id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleLogsArchive retrieves archived logs for a Run from the object store and streams them back.
func (s *Server) handleLogsArchive(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	arch, err := s.store.GetLogArchive(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if arch == nil {
		http.Error(w, "log archive not available yet", http.StatusNotFound)
		return
	}
	if s.objStore == nil {
		http.Error(w, "object store not configured", http.StatusNotImplemented)
		return
	}
	rc, err := s.objStore.Get(r.Context(), arch.ObjectKey)
	if err != nil {
		// The archive record exists in the DB, so a NotFound here means the
		// underlying object is gone (inconsistency) rather than "never
		// archived" — still surfaced as 404 since that's what the client
		// cares about; other errors (e.g. transient backend failure) stay 500.
		if errors.Is(err, objectstore.ErrNotFound) {
			http.Error(w, "log archive object not found", http.StatusNotFound)
			return
		}
		http.Error(w, "fetch archive: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", `attachment; filename="logs.ndjson"`)
	_, _ = io.Copy(w, rc)
}

// parseStepsParam parses the optional comma-separated steps=0,2 view filter.
func parseStepsParam(r *http.Request) ([]int, error) {
	raw := r.URL.Query().Get("steps")
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return nil, fmt.Errorf("invalid steps value %q", p)
		}
		out = append(out, n)
	}
	return out, nil
}

// handleLogStats returns the total line count and min/max seq for a run's
// windowed log view (optionally restricted to a set of steps).
func (s *Server) handleLogStats(w http.ResponseWriter, r *http.Request) {
	steps, err := parseStepsParam(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := chi.URLParam(r, "id")
	trimmed, err := s.logsTrimmed(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var count, minSeq, maxSeq int64
	if trimmed {
		all, err := s.archLogs.lines(r.Context(), id)
		if err != nil {
			http.Error(w, "log archive unavailable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		count, minSeq, maxSeq = countArchivedLogs(all, steps)
	} else {
		count, minSeq, maxSeq, err = s.store.CountLogs(r.Context(), id, steps)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]int64{"count": count, "minSeq": minSeq, "maxSeq": maxSeq})
}

// handleLogRange returns `limit` lines starting at 0-based view row `offset`
// for the windowed log viewer, optionally restricted to a set of steps.
func (s *Server) handleLogRange(w http.ResponseWriter, r *http.Request) {
	steps, err := parseStepsParam(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	offset, limit := 0, 1000
	if v := r.URL.Query().Get("offset"); v != "" {
		if offset, err = strconv.Atoi(v); err != nil || offset < 0 {
			http.Error(w, "invalid offset", http.StatusBadRequest)
			return
		}
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit <= 0 {
			http.Error(w, "invalid limit", http.StatusBadRequest)
			return
		}
	}
	if limit > 10000 {
		limit = 10000
	}
	id := chi.URLParam(r, "id")
	trimmed, err := s.logsTrimmed(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var lines []api.LogLine
	if trimmed {
		all, err := s.archLogs.lines(r.Context(), id)
		if err != nil {
			http.Error(w, "log archive unavailable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		lines = archivedLogRange(all, steps, offset, limit)
	} else {
		lines, err = s.store.ListLogsRange(r.Context(), id, steps, offset, limit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if lines == nil {
		lines = []api.LogLine{}
	}
	writeJSON(w, http.StatusOK, lines)
}

// handleLogSearch performs a server-side substring search over a run's log
// lines (optionally restricted to a set of steps), returning up to a capped
// number of matches plus the total match count.
func (s *Server) handleLogSearch(w http.ResponseWriter, r *http.Request) {
	steps, err := parseStepsParam(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	q := r.URL.Query().Get("q")
	if q == "" {
		http.Error(w, "q is required", http.StatusBadRequest)
		return
	}
	id := chi.URLParam(r, "id")
	trimmed, err := s.logsTrimmed(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var total int64
	var matches []store.LogSearchMatch
	if trimmed {
		all, err := s.archLogs.lines(r.Context(), id)
		if err != nil {
			http.Error(w, "log archive unavailable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		total, matches = searchArchivedLogs(all, steps, q, 1000)
	} else {
		total, matches, err = s.store.SearchLogs(r.Context(), id, steps, q, 1000)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if matches == nil {
		matches = []store.LogSearchMatch{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "matches": matches})
}
