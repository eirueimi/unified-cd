package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/go-chi/chi/v5"
)

// handleAgentRegister registers or updates an agent.
func (s *Server) handleAgentRegister(w http.ResponseWriter, r *http.Request) {
	var req api.AgentRegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.AgentID == "" {
		http.Error(w, "agentId is required", http.StatusBadRequest)
		return
	}
	// Use an empty slice when labels is nil to avoid a NULL constraint violation.
	labels := req.Labels
	if labels == nil {
		labels = []string{}
	}
	// Automatically attach a hostname label. If the client explicitly provided a hostname:* label,
	// respect it and do not add a duplicate (so agents can be pinned via agentSelector).
	if req.Hostname != "" {
		hasHostnameLabel := false
		for _, l := range labels {
			if strings.HasPrefix(l, "hostname:") {
				hasHostnameLabel = true
				break
			}
		}
		if !hasHostnameLabel {
			labels = append(labels, "hostname:"+req.Hostname)
		}
	}
	if err := s.store.UpsertAgent(r.Context(), req.AgentID, req.Hostname, req.OS, req.Version, labels, req.Env); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAgentHeartbeat handles POST /api/v1/agents/{agentId}/heartbeat.
// Refreshes the agent's last_seen_at so a busy (non-polling) agent is not
// considered dead by the stuck-run reaper / stale-agent cleanup.
func (s *Server) handleAgentHeartbeat(w http.ResponseWriter, r *http.Request) {
	agentID := chi.URLParam(r, "agentId")
	if err := s.store.TouchAgent(r.Context(), agentID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAgentClaim is the endpoint for agents to pick up a Queued Run.
// Long-polls until a Run is available or the timeout is reached.
func (s *Server) handleAgentClaim(w http.ResponseWriter, r *http.Request) {
	agentID := chi.URLParam(r, "agentId")
	const maxClaimTimeout = 60 * time.Second
	const claimPollInterval = 1 * time.Second
	timeout, err := time.ParseDuration(r.URL.Query().Get("timeout"))
	if err != nil || timeout <= 0 || timeout > maxClaimTimeout {
		timeout = maxClaimTimeout
	}
	// Parse agent labels from the comma-separated query parameter.
	labelsStr := r.URL.Query().Get("labels")
	var agentLabels []string
	if labelsStr != "" {
		for _, l := range strings.Split(labelsStr, ",") {
			l = strings.TrimSpace(l)
			if l != "" {
				agentLabels = append(agentLabels, l)
			}
		}
	}

	// Upsert the agent's registration record on every claim so an agent that is
	// actively claiming/running jobs always (re)appears in inventory (agent list /
	// UI Agents page), even if it was never explicitly registered or the controller
	// DB was reset out from under a still-running agent. This is best-effort: a
	// failure here must not fail the claim itself.
	if err := s.store.UpsertAgentOnClaim(r.Context(), agentID, "", "", "", agentLabels, nil); err != nil {
		slog.Warn("agent claim: failed to upsert agent registration", "agentId", agentID, "error", err)
	}

	deadline := time.Now().Add(timeout)
	for {
		// Return an empty response immediately during shutdown (prevents errors before the DB connection closes).
		select {
		case <-s.claimDrainCh:
			writeJSON(w, http.StatusOK, api.ClaimResponse{})
			return
		default:
		}

		claimed, err := s.store.ClaimNextRun(r.Context(), agentID, agentLabels)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if claimed != nil {
			resp, cerr := buildClaimResponse(claimed)
			if cerr != nil {
				http.Error(w, cerr.Error(), http.StatusInternalServerError)
				return
			}
			resp.MatrixMaxCombinations = s.cfg.MatrixMaxCombinations
			writeJSON(w, http.StatusOK, resp)
			return
		}
		if time.Now().After(deadline) || r.Context().Err() != nil {
			writeJSON(w, http.StatusOK, api.ClaimResponse{})
			return
		}
		select {
		case <-s.claimDrainCh:
			// Return an empty response immediately on shutdown to release the agent.
			writeJSON(w, http.StatusOK, api.ClaimResponse{})
			return
		case <-r.Context().Done():
			writeJSON(w, http.StatusOK, api.ClaimResponse{})
			return
		case <-time.After(claimPollInterval):
		}
	}
}

// buildClaimResponse constructs a ClaimResponse from a ClaimedRun.
// Includes each step's Outputs/Call information and the Job-level output declarations.
func buildClaimResponse(c *store.ClaimedRun) (api.ClaimResponse, error) {
	var spec dsl.Spec
	if err := json.Unmarshal(c.Spec, &spec); err != nil {
		return api.ClaimResponse{}, err
	}

	resp := api.ClaimResponse{
		RunID:          c.ID,
		JobName:        c.JobName,
		Params:         c.Params,
		TimeoutMinutes: spec.TimeoutMinutes,
		PodTemplate:    spec.PodTemplate,
	}

	for _, o := range spec.Params.Outputs {
		resp.JobOutputs = append(resp.JobOutputs, o.Name)
	}

	secretsNeeded := map[string]struct{}{}
	stepIdx := 0 // flat step counter across steps and finally

	resp.Stages = buildStages(spec.Steps, &stepIdx, secretsNeeded)
	resp.Finally = buildStages(spec.Finally, &stepIdx, secretsNeeded)

	for name := range secretsNeeded {
		resp.SecretsNeeded = append(resp.SecretsNeeded, name)
	}
	return resp, nil
}

// buildStages compiles a list of StepEntry into ClaimStages, advancing the
// shared flat step index and collecting referenced secret names.
func buildStages(entries []dsl.StepEntry, stepIdx *int, secretsNeeded map[string]struct{}) []api.ClaimStage {
	stages := make([]api.ClaimStage, 0, len(entries))
	for stageIdx, entry := range entries {
		if len(entry.Parallel) > 0 {
			stage := api.ClaimStage{Parallel: make([]api.ClaimStep, 0, len(entry.Parallel))}
			for _, st := range entry.Parallel {
				cs := buildOneClaimStep(*stepIdx, stageIdx, stepToStepEntry(st))
				stage.Parallel = append(stage.Parallel, cs)
				collectSecretNames(st.Run, secretsNeeded)
				for _, v := range st.Env {
					collectSecretNames(v, secretsNeeded)
				}
				*stepIdx++
			}
			stages = append(stages, stage)
		} else {
			cs := buildOneClaimStep(*stepIdx, stageIdx, entry)
			stages = append(stages, api.ClaimStage{Step: &cs})
			collectSecretNames(entry.Run, secretsNeeded)
			for _, v := range entry.Env {
				collectSecretNames(v, secretsNeeded)
			}
			*stepIdx++
		}
	}
	return stages
}

// stepToStepEntry converts a dsl.Step (used inside parallel: blocks) into the
// equivalent dsl.StepEntry so it can go through buildOneClaimStep and receive
// the same foreach/matrix normalization as top-level steps.
func stepToStepEntry(st dsl.Step) dsl.StepEntry {
	return dsl.StepEntry{
		Name: st.Name, If: st.If, Env: st.Env, Run: st.Run,
		Outputs: st.Outputs, Call: st.Call, Uses: st.Uses, Cache: st.Cache,
		UploadArtifact: st.UploadArtifact, DownloadArtifact: st.DownloadArtifact,
		Post: st.Post, ContinueOnError: st.ContinueOnError, Container: st.Container,
		RunsIn:         st.RunsIn,
		ScopeID:        st.ScopeID,
		ScopeImage:     st.ScopeImage,
		TimeoutMinutes: st.TimeoutMinutes, Foreach: st.Foreach, Matrix: st.Matrix,
		Approval: st.Approval,
	}
}

func buildOneClaimStep(stepIdx, stageIdx int, entry dsl.StepEntry) api.ClaimStep {
	cs := api.ClaimStep{
		Index:           stepIdx,
		StageIndex:      stageIdx,
		Name:            entry.Name,
		If:              entry.If,
		Env:             entry.Env,
		Run:             entry.Run,
		Outputs:         entry.Outputs,
		Cache:           entry.Cache,
		ContinueOnError: entry.ContinueOnError,
		Container:       entry.Container,
		RunsIn:          entry.RunsIn,
		ScopeID:         entry.ScopeID,
		ScopeImage:      entry.ScopeImage,
		TimeoutMinutes:  entry.TimeoutMinutes,
	}
	if entry.Call != nil {
		cs.Call = &api.ClaimCallStep{Job: entry.Call.Job, Params: entry.Call.WithAsStrings()}
	}
	if entry.Uses != nil {
		// Uses is resolved by the agent; pass job URI and params directly.
		// Reuse ClaimCallStep — agent distinguishes by the git:// URI prefix.
		cs.Call = &api.ClaimCallStep{Job: entry.Uses.Job, Params: entry.Uses.WithAsStrings()}
	}
	if entry.Post != nil {
		cs.Post = &api.PostStep{Run: entry.Post.Run, Env: entry.Post.Env}
	}
	if entry.UploadArtifact != nil {
		cs.UploadArtifact = &api.UploadArtifactStep{Name: entry.UploadArtifact.Name, Path: entry.UploadArtifact.Path}
	}
	if entry.DownloadArtifact != nil {
		cs.DownloadArtifact = &api.DownloadArtifactStep{Name: entry.DownloadArtifact.Name, DestDir: entry.DownloadArtifact.DestDir}
	}
	if entry.Foreach != nil {
		cs.Matrix = &api.ClaimMatrixDef{Dimensions: []api.ClaimMatrixDimension{{
			Name:   entry.Foreach.Key,
			Source: api.ClaimForeachSource{Literal: entry.Foreach.Source.Literal, Expr: entry.Foreach.Source.Expr},
		}}}
	}
	if entry.Matrix != nil {
		dims := make([]api.ClaimMatrixDimension, len(entry.Matrix.Dimensions))
		for i, d := range entry.Matrix.Dimensions {
			dims[i] = api.ClaimMatrixDimension{
				Name:   d.Name,
				Source: api.ClaimForeachSource{Literal: d.Source.Literal, Expr: d.Source.Expr},
			}
		}
		cs.Matrix = &api.ClaimMatrixDef{Dimensions: dims, Exclude: entry.Matrix.Exclude}
	}
	if entry.Approval != nil {
		timeout := entry.Approval.TimeoutMinutes
		if timeout == 0 {
			timeout = 60
		}
		cs.Approval = &api.ClaimApproval{Message: entry.Approval.Message, TimeoutMinutes: timeout}
	}
	return cs
}

// collectSecretNames scans a template string for secret name references and
// adds each name to seen. It recognises both "secrets.NAME" (normalised form
// written by users) and ".Secrets.NAME" (direct dot-access form that also
// appears in examples). Names may contain letters, digits, underscores, and
// hyphens.
func collectSecretNames(tpl string, seen map[string]struct{}) {
	for _, prefix := range []string{"secrets.", ".Secrets."} {
		s := tpl
		for {
			idx := strings.Index(s, prefix)
			if idx < 0 {
				break
			}
			s = s[idx+len(prefix):]
			end := 0
			for end < len(s) && (s[end] == '_' || s[end] == '-' || (s[end] >= 'a' && s[end] <= 'z') ||
				(s[end] >= 'A' && s[end] <= 'Z') || (s[end] >= '0' && s[end] <= '9')) {
				end++
			}
			if end > 0 {
				seen[s[:end]] = struct{}{}
			}
			if end < len(s) {
				s = s[end:]
			} else {
				break
			}
		}
	}
}

// handleAgentStepReport records step execution status reported by an agent.
func (s *Server) handleAgentStepReport(w http.ResponseWriter, r *http.Request) {
	var req api.StepReportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var startedAt, endedAt *time.Time
	if !req.StartedAt.IsZero() {
		t := req.StartedAt
		startedAt = &t
	}
	if !req.EndedAt.IsZero() {
		t := req.EndedAt
		endedAt = &t
	}
	var exit *int
	if req.Status == "Succeeded" || req.Status == "Failed" {
		ec := req.ExitCode
		exit = &ec
	}
	// Guard against writing stale step state under an already-terminal run. If the
	// parent run was finalized (e.g. the reaper Failed it) before this late report
	// arrived, upserting would leave a step in a status inconsistent with the run's
	// terminal outcome. Report the no-op distinctly (200-with-body, mirroring
	// handleAgentFinishRun) so the agent's client — which treats >=400 as an error —
	// does not spuriously fail. GetRun's not-found/transient errors are handled
	// separately so a transient DB blip does not masquerade as "already finalized".
	if run, err := s.store.GetRun(r.Context(), req.RunID); err == nil {
		switch run.Status {
		case api.RunSucceeded, api.RunFailed, api.RunCancelled:
			writeJSON(w, http.StatusOK, map[string]any{
				"runId":            req.RunID,
				"stepIndex":        req.StepIndex,
				"alreadyFinalized": true,
			})
			return
		}
	} else if !errors.Is(err, store.ErrRunNotFound) {
		// Transient DB error: fail loudly rather than silently dropping the report.
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.store.UpsertStepReport(r.Context(), req.RunID, req.StepIndex, req.StageIndex, req.StepName, req.Variant, req.Status, exit, startedAt, endedAt, req.ChildRunID, req.CallJobName); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Transition the Run to Running as soon as any step becomes Running.
	// In parallel execution, step 0 is not necessarily the first to become Running.
	if req.Status == "Running" {
		_ = s.store.MarkRunRunning(r.Context(), req.RunID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAgentLogAppend appends a log line sent by an agent.
func (s *Server) handleAgentLogAppend(w http.ResponseWriter, r *http.Request) {
	var req api.LogAppendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Timestamp.IsZero() {
		req.Timestamp = time.Now().UTC()
	}
	if _, err := s.store.AppendLog(r.Context(), req.RunID, req.StepIndex, req.Stream, req.Timestamp, req.Line); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAgentFinishRun records the final status of a Run.
//
// When the run was already terminal (e.g. the orphaned-run reaper Failed it
// before this late report arrived), the CAS in the store matches no rows. Rather
// than returning a plain 204 — which would falsely tell the agent its report
// won the race — we respond 200 with a body flagging the run as already
// finalized. 200-with-body (not 409) is deliberate: the agent HTTP client treats
// any status >= 400 as an error, and FinishRun ignores the response body, so
// 200 keeps the agent from spuriously erroring while still letting other clients
// observe that the report was a no-op.
func (s *Server) handleAgentFinishRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "runId")
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	st := api.RunStatus(body.Status)
	switch st {
	case api.RunSucceeded, api.RunFailed, api.RunCancelled:
	default:
		http.Error(w, "invalid status", http.StatusBadRequest)
		return
	}
	updated, err := s.store.FinishRun(r.Context(), id, st)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !updated {
		// Late/CAS-miss report: the run was already terminal. Report the
		// no-op distinctly instead of a false-success 204.
		writeJSON(w, http.StatusOK, map[string]any{
			"runId":            id,
			"status":           string(st),
			"alreadyFinalized": true,
		})
		return
	}
	// A parent run that Failed/Cancelled should not leave its call: children
	// (possibly still Queued) running or waiting.
	if st == api.RunFailed || st == api.RunCancelled {
		cancelDescendantRuns(r.Context(), s.store, id)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAgentSetStepOutputs records step outputs reported by an agent.
func (s *Server) handleAgentSetStepOutputs(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runId")
	stepIndexStr := chi.URLParam(r, "stepIndex")
	var stepIndex int
	if _, err := fmt.Sscanf(stepIndexStr, "%d", &stepIndex); err != nil {
		http.Error(w, "invalid stepIndex", http.StatusBadRequest)
		return
	}
	variant := r.URL.Query().Get("variant")
	var req api.SetOutputsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for k, v := range req.Outputs {
		if err := s.store.SetStepOutput(r.Context(), runID, stepIndex, variant, k, v); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAgentSetRunOutputs records Run outputs reported by an agent.
func (s *Server) handleAgentSetRunOutputs(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runId")
	var req api.SetOutputsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for k, v := range req.Outputs {
		if err := s.store.SetRunOutput(r.Context(), runID, k, v); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAgentLogBulk appends multiple log lines sent by an agent in a single request.
func (s *Server) handleAgentLogBulk(w http.ResponseWriter, r *http.Request) {
	var lines []api.LogAppendRequest
	if err := json.NewDecoder(r.Body).Decode(&lines); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for _, req := range lines {
		if req.Timestamp.IsZero() {
			req.Timestamp = time.Now().UTC()
		}
		if _, err := s.store.AppendLog(r.Context(), req.RunID, req.StepIndex, req.Stream, req.Timestamp, req.Line); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAgentReconcileRuns fails Running runs still claimed by the calling
// agent. An agent calls this BEFORE it starts claiming (startup reconcile: a
// restarted process no longer executes runs its previous incarnation claimed
// — the stuck-run reaper cannot catch this case because the same agent ID
// immediately resumes heartbeating) and best-effort on force shutdown.
// Semantics match the stuck-run reaper via failOrphanedRun: Failed (never
// re-queued), locks released, call: descendants cascade-cancelled.
func (s *Server) handleAgentReconcileRuns(w http.ResponseWriter, r *http.Request) {
	agentID := chi.URLParam(r, "agentId")
	ids, err := s.store.ListRunningRunIDsByAgent(r.Context(), agentID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	failed := 0
	for _, id := range ids {
		if err := failOrphanedRun(r.Context(), s.store, id); err != nil {
			slog.Error("agent reconcile: mark failed", "runId", id, "agentId", agentID, "error", err)
			continue
		}
		slog.Warn("agent reconcile: failed orphaned run (agent process replaced)", "runId", id, "agentId", agentID)
		failed++
	}
	writeJSON(w, http.StatusOK, map[string]int{"failedRuns": failed})
}
