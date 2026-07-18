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
	principal, ok := agentPrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	labels := req.Labels
	capabilities := req.Capabilities
	if principal.AuthMethod != "legacy" {
		if req.AgentID != principal.AgentID {
			s.recordAgentAuth("access", "failure", "policy")
			http.Error(w, "agent identity mismatch", http.StatusForbidden)
			return
		}
		labels = append([]string(nil), principal.AuthorizedLabels...)
		capabilities = append([]string(nil), principal.AuthorizedCapabilities...)
	}
	// Validate every advertised capability against the known vocabulary before
	// persisting. An unrecognized capability string would silently never match
	// any run's required_caps (or worse, mean something unintended), so reject
	// the whole registration rather than accept a partially-bogus set.
	for _, c := range capabilities {
		if !dsl.ValidCapability(c) {
			http.Error(w, "unknown capability: "+c, http.StatusBadRequest)
			return
		}
	}
	// Use an empty slice when labels is nil to avoid a NULL constraint violation.
	if labels == nil {
		labels = []string{}
	}
	// Legacy registrations may attach a self-reported hostname label. Principal
	// registrations use only controller-authorized labels.
	if principal.AuthMethod == "legacy" && req.Hostname != "" {
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
	if err := s.store.UpsertAgent(r.Context(), req.AgentID, req.Hostname, req.OS, req.Version, labels, capabilities, req.Env); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// heartbeatReconcileGrace is how long a Running run must have sat claimed
// before an absent-from-the-reported-set heartbeat is allowed to fail it.
// Protects a run the agent only just claimed from being reaped before the
// agent's next heartbeat has had a chance to report it as active.
const heartbeatReconcileGrace = 60 * time.Second

// handleAgentHeartbeat handles POST /api/v1/agents/{agentId}/heartbeat.
// Refreshes the agent's last_seen_at so a busy (non-polling) agent is not
// considered dead by the stuck-run reaper / stale-agent cleanup.
//
// A live agent (built after active-run tracking was added) additionally
// sends a JSON body reporting the run IDs it currently considers active,
// even when that set is empty. When a body is present, any Running run this
// controller has claimed to the agent, that is absent from the reported
// set, and that has sat claimed past heartbeatReconcileGrace, is failed as
// orphaned (e.g. the agent process restarted and forgot about it, or the
// run's goroutine died silently) — mirroring the stuck-run reaper's
// failOrphanedRun semantics but reacting on the agent's own heartbeat
// instead of waiting for last_seen_at staleness.
//
// A legacy agent (built before active-run tracking) sends no body at all,
// so reconcile is gated on BODY PRESENCE (r.ContentLength != 0), not on the
// decoded slice being non-nil: a live agent with zero active runs still
// sends `{"activeRunIds":[]}` (ContentLength > 0) and must reconcile (fail
// all of its stale runs), which a nil-slice check would wrongly skip. A
// decode error or missing body is treated the same as "unknown" and simply
// skips reconcile for this heartbeat — it never fails the heartbeat itself.
func (s *Server) handleAgentHeartbeat(w http.ResponseWriter, r *http.Request) {
	agentID := chi.URLParam(r, "agentId")
	if err := s.store.TouchAgent(r.Context(), agentID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if r.ContentLength != 0 {
		var req api.HeartbeatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
			reported := map[string]struct{}{}
			for _, id := range req.ActiveRunIDs {
				reported[id] = struct{}{}
			}
			ids, err := s.store.ListReconcilableRunIDsByAgent(r.Context(), agentID, heartbeatReconcileGrace)
			if err == nil {
				for _, id := range ids {
					if _, ok := reported[id]; ok {
						continue
					}
					if ferr := failOrphanedRun(r.Context(), s.store, id); ferr != nil {
						slog.Warn("heartbeat reconcile: fail orphaned run", "runID", id, "error", ferr)
					}
				}
			}
		}
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
	principal, ok := agentPrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var agentLabels []string
	if principal.AuthMethod == "legacy" {
		// Parse agent labels from the comma-separated query parameter.
		labelsStr := r.URL.Query().Get("labels")
		if labelsStr != "" {
			for _, l := range strings.Split(labelsStr, ",") {
				l = strings.TrimSpace(l)
				if l != "" {
					agentLabels = append(agentLabels, l)
				}
			}
		}
	} else {
		agentLabels = append([]string(nil), principal.AuthorizedLabels...)
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
				// ClaimNextRun already flipped this run to Running in the same SQL
				// statement, so leaving it as-is here would strand it Running forever:
				// the claiming agent is alive and heartbeating, so ListStuckRunIDs'
				// last_seen_at predicate would never select it for reaping. cerr is
				// deterministic (buildClaimResponse is pure computation over the
				// already-stored spec bytes — e.g. the pre-migration runsIn guard),
				// so retrying the claim can never succeed either; fail fast now rather
				// than treat it as a transient error.
				if _, logErr := s.store.AppendLog(r.Context(), claimed.ID, -1, "stderr", time.Now().UTC(), cerr.Error()); logErr != nil {
					slog.Error("agent claim: append claim-build failure reason", "runId", claimed.ID, "error", logErr)
				}
				if failErr := failOrphanedRun(r.Context(), s.store, claimed.ID); failErr != nil {
					slog.Error("agent claim: mark failed after claim build error", "runId", claimed.ID, "error", failErr)
				}
				writeJSON(w, http.StatusOK, api.ClaimResponse{})
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

	if err := rejectPreMigrationRunsIn(c.JobName, spec.Steps); err != nil {
		return api.ClaimResponse{}, err
	}
	if err := rejectPreMigrationRunsIn(c.JobName, spec.Finally); err != nil {
		return api.ClaimResponse{}, err
	}

	resp := api.ClaimResponse{
		RunID:          c.ID,
		JobName:        c.JobName,
		Params:         c.Params,
		TimeoutMinutes: spec.TimeoutMinutes,
		PodTemplate:    spec.PodTemplate,
		Native:         spec.Native,
	}

	for _, o := range spec.Params.Outputs {
		resp.JobOutputs = append(resp.JobOutputs, o.Name)
	}

	secretsNeeded := map[string]struct{}{}
	stepIdx := 0 // flat step counter across steps and finally

	resp.Stages = buildStages(spec.Steps, &stepIdx, secretsNeeded, spec.Shell)
	resp.Finally = buildStages(spec.Finally, &stepIdx, secretsNeeded, spec.Shell)

	for name := range secretsNeeded {
		resp.SecretsNeeded = append(resp.SecretsNeeded, name)
	}
	return resp, nil
}

// rejectPreMigrationRunsIn guards against a job that was applied before the
// 2026-07-08 job-isolation release, whose stored spec JSON may still carry a
// step-level runsIn: (image/container) on a non-uses step. Step-level
// runsIn: was removed from the DSL (see dsl.checkStepExecTarget); apply-time
// validation now rejects it on new applies, but a job stored before the
// migration was never re-validated, so its persisted JSON can still contain
// the removed field. buildOneClaimStep has no wire field for it, so without
// this guard the step would silently run on the default runner/container
// instead of the image/container the job author declared — a silent
// semantic drift. A uses: entry's runsIn.image is still legal (validated at
// apply time) and is intentionally not re-checked here.
func rejectPreMigrationRunsIn(jobName string, entries []dsl.StepEntry) error {
	for _, entry := range entries {
		if len(entry.Parallel) > 0 {
			for _, st := range entry.Parallel {
				if st.RunsIn != nil && st.Uses == nil {
					return fmt.Errorf("job %q: step %q uses the removed step-level runsIn: — re-apply the job after migrating to container: (see docs/migration-2026-07-job-isolation.md)", jobName, st.Name)
				}
			}
			continue
		}
		if entry.RunsIn != nil && entry.Uses == nil {
			return fmt.Errorf("job %q: step %q uses the removed step-level runsIn: — re-apply the job after migrating to container: (see docs/migration-2026-07-job-isolation.md)", jobName, entry.Name)
		}
	}
	return nil
}

// buildStages compiles a list of StepEntry into ClaimStages, advancing the
// shared flat step index and collecting referenced secret names. jobShell is
// the job-level spec.shell default (may be nil); it applies identically to
// top-level steps, parallel: sub-steps, and finally: steps — buildStages is
// used for all three (see buildClaimResponse).
func buildStages(entries []dsl.StepEntry, stepIdx *int, secretsNeeded map[string]struct{}, jobShell []string) []api.ClaimStage {
	stages := make([]api.ClaimStage, 0, len(entries))
	for stageIdx, entry := range entries {
		if len(entry.Parallel) > 0 {
			stage := api.ClaimStage{Parallel: make([]api.ClaimStep, 0, len(entry.Parallel))}
			for _, st := range entry.Parallel {
				cs := buildOneClaimStep(*stepIdx, stageIdx, stepToStepEntry(st), jobShell)
				stage.Parallel = append(stage.Parallel, cs)
				collectSecretNames(st.Run, secretsNeeded)
				for _, v := range st.Env {
					collectSecretNames(v, secretsNeeded)
				}
				*stepIdx++
			}
			stages = append(stages, stage)
		} else {
			cs := buildOneClaimStep(*stepIdx, stageIdx, entry, jobShell)
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
		ScopeID:        st.ScopeID,
		ScopeImage:     st.ScopeImage,
		TimeoutMinutes: st.TimeoutMinutes, Retry: st.Retry, Foreach: st.Foreach, Matrix: st.Matrix,
		Approval: st.Approval,
		Shell:    st.Shell,
	}
}

func buildOneClaimStep(stepIdx, stageIdx int, entry dsl.StepEntry, jobShell []string) api.ClaimStep {
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
		ScopeID:         entry.ScopeID,
		ScopeImage:      entry.ScopeImage,
		TimeoutMinutes:  entry.TimeoutMinutes,
		Retry:           entry.Retry,
		Shell:           resolveShell(entry.Shell, jobShell),
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
		// post.Shell is carried through as-is: present only when the dsl
		// post: hook declares its own shell:. Nil (post declares none) means
		// the agent inherits the owning step's effective cs.Shell above —
		// no resolution against jobShell happens here.
		cs.Post = &api.PostStep{Run: entry.Post.Run, Env: entry.Post.Env, Shell: entry.Post.Shell}
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

// resolveShell computes the effective interpreter argv for a step: the
// step's own shell: if declared, else the job-level spec.shell, else nil.
// Nil means "the agent applies the shim default" — the controller never
// resolves in a hardcoded default of its own. A uses: template's own
// declared shell (step-level or template-level spec.shell) has already been
// stamped onto stepShell by expandUsesStep before this ever runs (see
// internal/gittemplate/inline.go), so it naturally takes priority here too:
// stepShell is non-empty and jobShell (the caller's spec.shell) is skipped.
func resolveShell(stepShell, jobShell []string) []string {
	if len(stepShell) > 0 {
		return stepShell
	}
	if len(jobShell) > 0 {
		return jobShell
	}
	return nil
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
	agentID := chi.URLParam(r, "agentId")
	v, gerr := s.agentRunGuard(r.Context(), agentID, req.RunID, false)
	if gerr != nil {
		http.Error(w, gerr.Error(), http.StatusInternalServerError)
		return
	}
	if respondRunWriteVerdict(w, v, req.RunID) {
		return
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
	agentID := chi.URLParam(r, "agentId")
	v, gerr := s.agentRunGuard(r.Context(), agentID, req.RunID, false)
	if gerr != nil {
		http.Error(w, gerr.Error(), http.StatusInternalServerError)
		return
	}
	if respondRunWriteVerdict(w, v, req.RunID) {
		return
	}
	seq, err := s.store.AppendLog(r.Context(), req.RunID, req.StepIndex, req.Stream, req.Timestamp, req.Line)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if seq == 0 {
		// Sealed (logs already archived): dropped. 204 keeps unmodified
		// agents from retry-storming — same philosophy as FinishRun's
		// alreadyFinalized response.
		slog.Warn("dropping log line for sealed run", "run", req.RunID)
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
	agentID := chi.URLParam(r, "agentId")
	v, gerr := s.agentRunGuard(r.Context(), agentID, id, false)
	if gerr != nil {
		http.Error(w, gerr.Error(), http.StatusInternalServerError)
		return
	}
	if respondRunWriteVerdict(w, v, id) {
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
	agentID := chi.URLParam(r, "agentId")
	v, gerr := s.agentRunGuard(r.Context(), agentID, runID, true)
	if gerr != nil {
		http.Error(w, gerr.Error(), http.StatusInternalServerError)
		return
	}
	if respondRunWriteVerdict(w, v, runID) {
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
	agentID := chi.URLParam(r, "agentId")
	v, gerr := s.agentRunGuard(r.Context(), agentID, runID, true)
	if gerr != nil {
		http.Error(w, gerr.Error(), http.StatusInternalServerError)
		return
	}
	if respondRunWriteVerdict(w, v, runID) {
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
//
// Guarding runs first, in its own pass over the distinct RunIDs, so a mixed-
// ownership batch (e.g. one owned run's lines followed by another agent's
// run) is rejected before any line from the batch is appended — otherwise a
// batch straddling an owned and a not-owned run would partially land before
// the rejection is discovered mid-loop.
func (s *Server) handleAgentLogBulk(w http.ResponseWriter, r *http.Request) {
	var lines []api.LogAppendRequest
	if err := json.NewDecoder(r.Body).Decode(&lines); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	agentID := chi.URLParam(r, "agentId")
	guarded := map[string]bool{}
	for _, req := range lines {
		if guarded[req.RunID] {
			continue
		}
		v, gerr := s.agentRunGuard(r.Context(), agentID, req.RunID, false)
		if gerr != nil {
			http.Error(w, gerr.Error(), http.StatusInternalServerError)
			return
		}
		if respondRunWriteVerdict(w, v, req.RunID) {
			return
		}
		guarded[req.RunID] = true
	}
	dropped := 0
	var droppedRun string
	for _, req := range lines {
		if req.Timestamp.IsZero() {
			req.Timestamp = time.Now().UTC()
		}
		seq, err := s.store.AppendLog(r.Context(), req.RunID, req.StepIndex, req.Stream, req.Timestamp, req.Line)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if seq == 0 {
			dropped++
			droppedRun = req.RunID
		}
	}
	if dropped > 0 {
		slog.Warn("dropping log lines for sealed run", "run", droppedRun, "dropped", dropped)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAgentSidecarStatus records a user sidecar container's phase/exit-code
// report sent by an agent (host or k8s pump), for UI display.
//
// Ownership-only guard (rejectTerminal=false), unlike the other F2 endpoints:
// both agents stop their sidecar pumps via a deferred CloseScopes that runs
// AFTER FinishRun, so the final reportStatus(..., "exited", exitCode) call is
// *expected* to arrive once the run is already terminal. Rejecting it (as
// rejectTerminal=true would) permanently strands the sidecar's displayed
// status at its last pre-exit phase (e.g. "running") for every completed
// run. UpsertSidecarStatus is a display-only upsert keyed by (run, index),
// so a late/duplicate write here is harmless — it can only bring the shown
// status closer to the truth, never corrupt run state.
func (s *Server) handleAgentSidecarStatus(w http.ResponseWriter, r *http.Request) {
	var req api.SidecarStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	agentID := chi.URLParam(r, "agentId")
	v, gerr := s.agentRunGuard(r.Context(), agentID, req.RunID, false)
	if gerr != nil {
		http.Error(w, gerr.Error(), http.StatusInternalServerError)
		return
	}
	if respondRunWriteVerdict(w, v, req.RunID) {
		return
	}
	if err := s.store.UpsertSidecarStatus(r.Context(), req.RunID, req.Index, req.Name, req.Phase, req.ExitCode); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
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
