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

	"github.com/unified-cd/unified-cd/internal/api"
	"github.com/unified-cd/unified-cd/internal/dsl"
	"github.com/unified-cd/unified-cd/internal/secrets"
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
		ID: stored.ID, Name: stored.Name, UpdatedAt: stored.UpdatedAt,
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
		result = append(result, api.WebhookReceiverMeta{ID: wr.ID, Name: wr.Name, UpdatedAt: wr.UpdatedAt})
	}
	writeJSON(w, http.StatusOK, result)
}

// handleDeleteWebhook deletes the WebhookReceiver with the given name.
func (s *Server) handleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := s.store.DeleteWebhookReceiver(r.Context(), name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleWebhookIngress receives a webhook payload, performs signature verification, filter evaluation, and parameter mapping, then creates a Run.
func (s *Server) handleWebhookIngress(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	wr, err := s.store.GetWebhookReceiver(r.Context(), name)
	if err != nil {
		http.Error(w, "webhook receiver not found", http.StatusNotFound)
		return
	}

	var spec dsl.WebhookReceiverSpec
	if err := json.Unmarshal(wr.Spec, &spec); err != nil {
		http.Error(w, "invalid receiver spec", http.StatusInternalServerError)
		return
	}

	// Read the body (up to 1 MB).
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Verify the signature when authentication is configured.
	if spec.Auth.Type != "none" && spec.Auth.Type != "" {
		if err := s.verifyWebhookSignature(r, body, spec.Auth); err != nil {
			http.Error(w, "signature verification failed: "+err.Error(), http.StatusUnauthorized)
			return
		}
	}

	// Parse the JSON payload.
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid JSON payload", http.StatusBadRequest)
		return
	}

	tplData := dsl.WebhookTemplateData{Payload: payload}

	// Evaluate filters (any result other than "true" is treated as filtered out).
	for i, filter := range spec.Filters {
		result, err := dsl.ExpandWebhookTemplate(filter, tplData)
		if err != nil {
			http.Error(w, fmt.Sprintf("filter[%d] error: %s", i, err), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(result) != "true" {
			// Filtered out — return 204, not an error.
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}

	// Map parameters from the payload.
	params := map[string]string{}
	for k, tpl := range spec.ParamsMapping {
		val, err := dsl.ExpandWebhookTemplate(tpl, tplData)
		if err != nil {
			http.Error(w, fmt.Sprintf("paramsMapping[%s] error: %s", k, err), http.StatusBadRequest)
			return
		}
		params[k] = val
	}

	// Fetch the Job.
	job, err := s.store.GetJob(r.Context(), spec.Trigger.Job)
	if err != nil {
		http.Error(w, "job not found: "+spec.Trigger.Job, http.StatusBadRequest)
		return
	}

	// Extract the agentSelector from the job spec.
	var jobSpec dsl.Spec
	agentSelector := []string{}
	if err := json.Unmarshal(job.Spec, &jobSpec); err == nil {
		agentSelector = jobSpec.AgentSelector
	}
	agentSelector, err = dsl.ExpandAgentSelector(agentSelector, params)
	if err != nil {
		http.Error(w, "agentSelector: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Create the Run.
	run, err := s.store.CreateRun(r.Context(), job.Name, params, job.Spec, agentSelector, "webhook:"+name)
	if err != nil {
		http.Error(w, "create run: "+err.Error(), http.StatusInternalServerError)
		return
	}

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
		return fmt.Errorf("secret %q not found", auth.SecretRef)
	}
	secretBytes, err := secrets.Decrypt(r.Context(), s.km, stored.EncryptedDEK, stored.Ciphertext)
	if err != nil {
		return fmt.Errorf("decrypt secret: %w", err)
	}

	mac := hmac.New(sha256.New, secretBytes)
	mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	// Check X-Signature (generic) or X-Hub-Signature-256 (GitHub).
	var gotSig string
	switch auth.Type {
	case "github":
		gotSig = r.Header.Get("X-Hub-Signature-256")
	default:
		gotSig = r.Header.Get("X-Signature")
		if gotSig == "" {
			gotSig = r.Header.Get("X-Hub-Signature-256")
		}
	}

	if !hmac.Equal([]byte(gotSig), []byte(expected)) {
		return fmt.Errorf("HMAC mismatch")
	}
	return nil
}
