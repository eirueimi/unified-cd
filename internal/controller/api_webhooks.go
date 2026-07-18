package controller

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/secrets"
	"github.com/go-chi/chi/v5"
)

// handleApplyWebhook accepts a WebhookReceiver YAML and creates or updates it.
func (s *Server) handleApplyWebhook(w http.ResponseWriter, r *http.Request) {
	var req api.ApplyWebhookRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	wr, err := dsl.ParseWebhookReceiver(strings.NewReader(req.YAML))
	if err != nil {
		http.Error(w, "invalid yaml: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.guardManagedResource(r.Context(), "WebhookReceiver", wr.Metadata.Name); err != nil {
		writeGuardError(w, err)
		return
	}
	specJSON, err := json.Marshal(wr.Spec)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	stored, err := s.store.UpsertWebhookReceiver(r.Context(), wr.Metadata.Name, specJSON)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, api.WebhookReceiverMeta{
		ID: stored.ID, Name: stored.Name, UpdatedAt: stored.UpdatedAt, Spec: stored.Spec,
	})
}

// handleListWebhooks returns the list of registered WebhookReceivers.
func (s *Server) handleListWebhooks(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListWebhookReceivers(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	result := make([]api.WebhookReceiverMeta, 0, len(list))
	for _, wr := range list {
		result = append(result, api.WebhookReceiverMeta{ID: wr.ID, Name: wr.Name, UpdatedAt: wr.UpdatedAt, Spec: wr.Spec})
	}
	writeJSON(w, http.StatusOK, result)
}

// handleDeleteWebhook deletes the WebhookReceiver with the given name.
func (s *Server) handleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := s.guardManagedResource(r.Context(), "WebhookReceiver", name); err != nil {
		writeGuardError(w, err)
		return
	}
	if err := s.store.DeleteWebhookReceiver(r.Context(), name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// countWebhookEvent records a webhook ingress outcome when metrics are enabled.
func (s *Server) countWebhookEvent(name, outcome string) {
	if s.metrics != nil {
		s.metrics.WebhookEvent(name, outcome)
	}
}

// handleWebhookIngress receives a webhook payload, performs signature verification, filter evaluation, and parameter mapping, then creates a Run.
func (s *Server) handleWebhookIngress(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	wr, err := s.store.GetWebhookReceiver(r.Context(), name)
	if err != nil {
		s.countWebhookEvent("unknown", "rejected")
		http.Error(w, "webhook receiver not found", http.StatusNotFound)
		return
	}

	var spec dsl.WebhookReceiverSpec
	if err := json.Unmarshal(wr.Spec, &spec); err != nil {
		s.countWebhookEvent(name, "error")
		http.Error(w, "invalid receiver spec", http.StatusInternalServerError)
		return
	}

	// Read the body (up to 1 MB).
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		s.countWebhookEvent(name, "error")
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Verify authentication when configured.
	if spec.Auth.Type != "none" && spec.Auth.Type != "" {
		var verr error
		switch spec.Auth.Type {
		case "token":
			verr = s.verifyWebhookToken(r, spec.Auth)
		default: // "github", "hmac-sha256"
			verr = s.verifyWebhookSignature(r, body, spec.Auth)
		}
		if verr != nil {
			s.countWebhookEvent(name, "rejected")
			http.Error(w, "signature verification failed: "+verr.Error(), http.StatusUnauthorized)
			return
		}
	}

	// Parse the JSON payload.
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		s.countWebhookEvent(name, "error")
		http.Error(w, "invalid JSON payload", http.StatusBadRequest)
		return
	}

	tplData := dsl.WebhookTemplateData{Payload: payload}

	// Evaluate filters (any result other than "true" is treated as filtered out).
	for i, filter := range spec.Filters {
		result, err := dsl.ExpandWebhookTemplate(filter, tplData)
		if err != nil {
			s.countWebhookEvent(name, "error")
			http.Error(w, fmt.Sprintf("filter[%d] error: %s", i, err), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(result) != "true" {
			// Filtered out — return 204, not an error.
			s.countWebhookEvent(name, "filtered")
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}

	// AppSource trigger: force a GitOps re-sync instead of creating a Run.
	// paramsMapping does not apply here (there are no job inputs to fill).
	if spec.Trigger.AppSource != "" {
		if _, err := s.store.GetAppSource(r.Context(), spec.Trigger.AppSource); err != nil {
			s.countWebhookEvent(name, "error")
			http.Error(w, "appSource not found: "+spec.Trigger.AppSource, http.StatusBadRequest)
			return
		}
		if err := s.store.ResetAppSourceCommit(r.Context(), spec.Trigger.AppSource); err != nil {
			s.countWebhookEvent(name, "error")
			http.Error(w, "trigger appSource sync: "+err.Error(), http.StatusInternalServerError)
			return
		}
		s.countWebhookEvent(name, "accepted")
		writeJSON(w, http.StatusAccepted, map[string]string{
			"appSource": spec.Trigger.AppSource,
			"status":    "sync scheduled",
		})
		return
	}

	// Map parameters from the payload.
	params := map[string]string{}
	for k, tpl := range spec.ParamsMapping {
		val, err := dsl.ExpandWebhookTemplate(tpl, tplData)
		if err != nil {
			s.countWebhookEvent(name, "error")
			http.Error(w, fmt.Sprintf("paramsMapping[%s] error: %s", k, err), http.StatusBadRequest)
			return
		}
		params[k] = val
	}

	// Fetch the Job.
	job, err := s.store.GetJob(r.Context(), spec.Trigger.Job)
	if err != nil {
		s.countWebhookEvent(name, "error")
		http.Error(w, "job not found: "+spec.Trigger.Job, http.StatusBadRequest)
		return
	}

	// Extract the agentSelector from the job spec.
	var jobSpec dsl.Spec
	agentSelector := []string{}
	if err := json.Unmarshal(job.Spec, &jobSpec); err == nil {
		agentSelector = jobSpec.AgentSelector
	}
	params, err = resolveParams(jobSpec.Params.Inputs, params)
	if err != nil {
		s.countWebhookEvent(name, "error")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	agentSelector, err = dsl.ExpandAgentSelector(agentSelector, params)
	if err != nil {
		s.countWebhookEvent(name, "error")
		http.Error(w, "agentSelector: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Infer the capability a run of this spec needs from an agent (native /
	// container / pod), mirroring handleTriggerRun. A podTemplate that uses
	// features the host agent's claim pod cannot honor can only run on
	// Kubernetes, so RequiredCaps yields "pod" for it — the agent-side
	// capability match (ClaimNextRun) then restricts the run to a
	// pod-capable agent instead of the old blanket "kubernetes" label pin.
	requiredCaps := dsl.RequiredCaps(jobSpec)

	// Create the Run.
	run, err := s.store.CreateRun(r.Context(), job.Name, params, job.Spec, agentSelector, requiredCaps, "webhook:"+name)
	if err != nil {
		s.countWebhookEvent(name, "error")
		http.Error(w, "create run: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.countWebhookEvent(name, "accepted")
	writeJSON(w, http.StatusOK, run)
}

// verifyWebhookSignature verifies an HMAC-SHA256 signature.
// Supports the X-Signature header (generic) or the X-Hub-Signature-256 header (GitHub-compatible).
func (s *Server) verifyWebhookSignature(r *http.Request, body []byte, auth dsl.WebhookAuth) error {
	if s.km == nil {
		return fmt.Errorf("key manager not configured — cannot verify signature")
	}
	stored, err := s.store.GetSecret(r.Context(), auth.SecretRef, "global", "")
	if err != nil {
		return fmt.Errorf("secret %q not found — create it with `unified-cli secret set %s <value>` using the same value configured on the sender", auth.SecretRef, auth.SecretRef)
	}
	secretBytes, err := secrets.Decrypt(r.Context(), s.km, stored.EncryptedDEK, stored.Ciphertext,
		secrets.SecretBinding(stored.Name, stored.Scope, stored.ScopeRef))
	if err != nil {
		return fmt.Errorf("decrypt secret %q: %w", auth.SecretRef, err)
	}
	if len(secretBytes) == 0 {
		return fmt.Errorf("secret %q is empty — set a non-empty value that matches the sender (an empty value can happen when piping with a trailing newline; prefer `unified-cli secret set %s '<value>'`)", auth.SecretRef, auth.SecretRef)
	}

	// Locate the signature header: X-Hub-Signature-256 for GitHub, or
	// X-Signature (falling back to X-Hub-Signature-256) for generic HMAC.
	var sigHeader, gotSig string
	switch auth.Type {
	case "github":
		sigHeader = "X-Hub-Signature-256"
		gotSig = r.Header.Get(sigHeader)
	default:
		sigHeader = "X-Signature"
		gotSig = r.Header.Get(sigHeader)
		if gotSig == "" {
			sigHeader = "X-Hub-Signature-256"
			gotSig = r.Header.Get(sigHeader)
		}
	}
	if gotSig == "" {
		if auth.Type == "github" {
			return fmt.Errorf("missing %s header — GitHub sends it only when the webhook has a Secret set; configure the same secret as %q on the GitHub webhook", sigHeader, auth.SecretRef)
		}
		return fmt.Errorf("missing signature header — expected X-Signature or X-Hub-Signature-256 carrying an HMAC-SHA256 of the request body")
	}

	mac := hmac.New(sha256.New, secretBytes)
	mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(gotSig), []byte(expected)) {
		return fmt.Errorf("%s does not match — the %q secret differs from the value configured on the sender, or the request body was modified in transit (for GitHub, set the webhook Content type to application/json)", sigHeader, auth.SecretRef)
	}
	return nil
}

// verifyWebhookToken verifies a plaintext shared-secret token sent in a header
// (GitLab-style X-Gitlab-Token). No HMAC: constant-time compare of the header
// value against the stored secret referenced by auth.SecretRef. The header name
// is configurable via auth.Header (default "X-Gitlab-Token").
func (s *Server) verifyWebhookToken(r *http.Request, auth dsl.WebhookAuth) error {
	if s.km == nil {
		return fmt.Errorf("key manager not configured — cannot verify token")
	}
	stored, err := s.store.GetSecret(r.Context(), auth.SecretRef, "global", "")
	if err != nil {
		return fmt.Errorf("secret %q not found — create it with `unified-cli secret set %s <value>` using the same value configured on the sender", auth.SecretRef, auth.SecretRef)
	}
	secretBytes, err := secrets.Decrypt(r.Context(), s.km, stored.EncryptedDEK, stored.Ciphertext,
		secrets.SecretBinding(stored.Name, stored.Scope, stored.ScopeRef))
	if err != nil {
		return fmt.Errorf("decrypt secret %q: %w", auth.SecretRef, err)
	}
	header := auth.Header
	if header == "" {
		header = "X-Gitlab-Token"
	}
	got := r.Header.Get(header)
	if got == "" {
		return fmt.Errorf("missing %s header — the sender must send the shared token in this header", header)
	}
	if !hmac.Equal([]byte(got), secretBytes) {
		return fmt.Errorf("token in %s does not match secret %q — check that the sender's token equals the stored value (watch for a trailing newline)", header, auth.SecretRef)
	}
	return nil
}
