package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/artifact"
)

// HTTPError represents a non-successful HTTP status returned by the server.
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("http %d: %s", e.StatusCode, e.Body)
}

// Client represents an HTTP client for the master server.
type Client struct {
	base   string
	source TokenSource
	http   *http.Client
}

// NewClient creates a new client with the given base URL and token.
func NewClient(baseURL, token string) *Client {
	return NewClientWithTokenSource(baseURL, staticTokenSource(token), nil)
}

// NewClientWithTokenSource creates a client which obtains an access token for
// every request. A nil httpClient uses the standard agent timeout.
func NewClientWithTokenSource(baseURL string, source TokenSource, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{base: baseURL, source: source, http: httpClient}
}

func (c *Client) authorize(ctx context.Context, req *http.Request) error {
	if c.source == nil {
		return fmt.Errorf("agent token source is required")
	}
	token, err := c.source.Token(ctx)
	if err != nil {
		var requestErr *credentialRequestError
		if errors.As(err, &requestErr) {
			return requestErr
		}
		return fmt.Errorf("obtain agent token")
	}
	if token == "" {
		// A TokenSource may deal with credentials internally. Do not surface its
		// error here because it could contain a credential from a remote response.
		return fmt.Errorf("obtain agent token")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

// do is a general-purpose method that executes an HTTP request and decodes the response.
func (c *Client) do(ctx context.Context, method, path string, body any, out any) (int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return 0, err
	}
	if err := c.authorize(ctx, req); err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return resp.StatusCode, &HTTPError{StatusCode: resp.StatusCode, Body: "response omitted"}
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode, nil
}

func safeResponseBody([]byte) string { return "response omitted" }

// Register registers the agent with the master server.
func (c *Client) Register(ctx context.Context, req api.AgentRegisterRequest) error {
	_, err := c.do(ctx, http.MethodPost, "/api/v1/agents/register", req, nil)
	return err
}

// ReconcileRuns asks the controller to fail Running runs still claimed by
// this agent — orphans left behind by a previous process incarnation (the
// stuck-run reaper cannot catch them because the same agent ID resumes
// heartbeating immediately). Returns the number of runs failed.
func (c *Client) ReconcileRuns(ctx context.Context, agentID string) (int, error) {
	var out struct {
		FailedRuns int `json:"failedRuns"`
	}
	_, err := c.do(ctx, http.MethodPost, "/api/v1/agents/"+agentID+"/runs/reconcile", nil, &out)
	return out.FailedRuns, err
}

// Deregister removes the agent from the master server.
func (c *Client) Deregister(ctx context.Context, agentID string) error {
	_, err := c.do(ctx, http.MethodDelete, "/api/v1/agents/"+agentID, nil, nil)
	return err
}

// Heartbeat refreshes the agent's last_seen_at on the controller.
// activeRunIDs is the current snapshot of runs this agent process has in
// flight, as reported by the caller's active-run provider (see
// StartHeartbeat / RunSet.Snapshot):
//   - nil sends no body at all (bodyless) — this is the legacy/pre-tracking
//     wire shape and is only reachable if a caller explicitly passes nil;
//     live code always passes a snapshot, so this path is kept for safety
//     rather than exercised in production.
//   - any non-nil slice, including an empty one, is marshalled as
//     api.HeartbeatRequest{ActiveRunIDs: activeRunIDs} and always sent as a
//     body — even for zero active runs — so the controller can distinguish
//     "live agent, zero active runs" (a reconcile candidate) from "legacy
//     agent, no body" (skip, unknown).
func (c *Client) Heartbeat(ctx context.Context, agentID string, activeRunIDs []string) error {
	var body any
	if activeRunIDs != nil {
		body = api.HeartbeatRequest{ActiveRunIDs: activeRunIDs}
	}
	_, err := c.do(ctx, http.MethodPost, "/api/v1/agents/"+agentID+"/heartbeat", body, nil)
	return err
}

// Claim requests an executable Run for the agent. labels is the list of agent labels.
func (c *Client) Claim(ctx context.Context, agentID, timeout string, labels []string) (api.ClaimResponse, error) {
	path := fmt.Sprintf("/api/v1/agents/%s/claim?timeout=%s", agentID, timeout)
	if len(labels) > 0 {
		path += "&labels=" + strings.Join(labels, ",")
	}
	var out api.ClaimResponse
	_, err := c.do(ctx, http.MethodPost, path, nil, &out)
	return out, err
}

// ReportStep reports the status of a step to the master server.
func (c *Client) ReportStep(ctx context.Context, agentID string, req api.StepReportRequest) error {
	_, err := c.do(ctx, http.MethodPost, "/api/v1/agents/"+agentID+"/steps", req, nil)
	return err
}

// AppendLog sends a log line to the master server.
func (c *Client) AppendLog(ctx context.Context, agentID string, req api.LogAppendRequest) error {
	_, err := c.do(ctx, http.MethodPost, "/api/v1/agents/"+agentID+"/logs", req, nil)
	return err
}

// FinishRun notifies the master server that a Run has completed.
func (c *Client) FinishRun(ctx context.Context, agentID, runID string, status api.RunStatus) error {
	body := map[string]string{"status": string(status)}
	_, err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/api/v1/agents/%s/runs/%s/finish", agentID, runID),
		body, nil)
	return err
}

// CreateRun creates a new Run with the given job name and parameters.
func (c *Client) CreateRun(ctx context.Context, jobName string, params map[string]string) (api.Run, error) {
	body := api.TriggerRunRequest{JobName: jobName, Params: params}
	var run api.Run
	_, err := c.do(ctx, http.MethodPost, "/api/v1/runs", body, &run)
	return run, err
}

// GetRun retrieves the Run with the given RunID.
func (c *Client) GetRun(ctx context.Context, runID string) (api.Run, error) {
	var run api.Run
	_, err := c.do(ctx, http.MethodGet, "/api/v1/runs/"+runID, nil, &run)
	return run, err
}

// GetRunOutputs retrieves the outputs of the Run with the given RunID.
func (c *Client) GetRunOutputs(ctx context.Context, runID string) (map[string]string, error) {
	var out api.RunOutputs
	_, err := c.do(ctx, http.MethodGet, "/api/v1/runs/"+runID+"/outputs", nil, &out)
	if err != nil {
		return nil, err
	}
	if out.Outputs == nil {
		out.Outputs = map[string]string{}
	}
	return out.Outputs, nil
}

// SetStepOutputs sends the step outputs to the master server. variant is the
// matrix combination key ("" for non-matrix steps).
func (c *Client) SetStepOutputs(ctx context.Context, agentID, runID string, stepIndex int, variant string, outputs map[string]string) error {
	path := fmt.Sprintf("/api/v1/agents/%s/runs/%s/steps/%d/outputs", agentID, runID, stepIndex)
	if variant != "" {
		path += "?variant=" + url.QueryEscape(variant)
	}
	_, err := c.do(ctx, http.MethodPost, path, api.SetOutputsRequest{Outputs: outputs}, nil)
	return err
}

// SetRunOutputs sends the Run-level outputs to the master server.
func (c *Client) SetRunOutputs(ctx context.Context, agentID, runID string, outputs map[string]string) error {
	path := fmt.Sprintf("/api/v1/agents/%s/runs/%s/outputs", agentID, runID)
	_, err := c.do(ctx, http.MethodPost, path, api.SetOutputsRequest{Outputs: outputs}, nil)
	return err
}

// AppendLogBulk sends multiple log lines in a single HTTP request.
func (c *Client) AppendLogBulk(ctx context.Context, agentID, runID string, stepIndex int, lines []api.LogAppendRequest) error {
	path := fmt.Sprintf("/api/v1/agents/%s/runs/%s/steps/%d/logs/bulk", agentID, runID, stepIndex)
	_, err := c.do(ctx, http.MethodPost, path, lines, nil)
	return err
}

// ReportSidecarStatus reports a user sidecar container's phase/exit-code to
// the master server for UI display.
func (c *Client) ReportSidecarStatus(ctx context.Context, agentID string, req api.SidecarStatusRequest) error {
	path := fmt.Sprintf("/api/v1/agents/%s/runs/%s/sidecars", agentID, req.RunID)
	_, err := c.do(ctx, http.MethodPost, path, req, nil)
	return err
}

// CreateApproval creates a pending approval record for an approval gate step.
func (c *Client) CreateApproval(ctx context.Context, agentID, runID string, req api.CreateApprovalRequest) error {
	_, err := c.do(ctx, http.MethodPost, "/api/v1/agents/"+agentID+"/runs/"+runID+"/approvals", req, nil)
	return err
}

// GetApproval retrieves the approval record for the given step index.
func (c *Client) GetApproval(ctx context.Context, agentID, runID string, stepIndex int) (api.RunApproval, error) {
	var a api.RunApproval
	_, err := c.do(ctx, http.MethodGet, "/api/v1/agents/"+agentID+"/runs/"+runID+"/approvals/"+strconv.Itoa(stepIndex), nil, &a)
	return a, err
}

// FetchSecrets retrieves secret values in plaintext from the master server.
func (c *Client) FetchSecrets(ctx context.Context, agentID string, names []string) (map[string]string, error) {
	if len(names) == 0 {
		return map[string]string{}, nil
	}
	path := fmt.Sprintf("/api/v1/agents/%s/secrets/fetch", agentID)
	var out api.AgentFetchSecretsResponse
	_, err := c.do(ctx, http.MethodPost, path, api.AgentFetchSecretsRequest{Names: names}, &out)
	if err != nil {
		return nil, err
	}
	if out.Secrets == nil {
		out.Secrets = map[string]string{}
	}
	return out.Secrets, nil
}

// UploadArtifact archives path as tar+zstd and uploads it to the master
// server, streaming the archive as a chunked request body so it is never
// fully buffered in memory. No Content-Length is set — net/http uses chunked
// transfer-encoding, and the controller reads r.Body straight into the object
// store (r.ContentLength == -1 → a multipart streaming Put).
func (c *Client) UploadArtifact(ctx context.Context, runID, name, path string) error {
	body := artifact.StreamTarZstd(path)
	defer body.Close()

	url := c.base + fmt.Sprintf("/api/v1/runs/%s/artifacts/%s", runID, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, body)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	if err := c.authorize(ctx, req); err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("upload artifact http %d", resp.StatusCode)
	}
	return nil
}

// DownloadArtifact downloads an artifact from the master server and extracts it into destDir.
// Fetches the stream from GET /api/v1/runs/{runID}/artifacts/{name} and extracts it.
func (c *Client) DownloadArtifact(ctx context.Context, runID, name, destDir string) error {
	url := c.base + fmt.Sprintf("/api/v1/runs/%s/artifacts/%s", runID, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	if err := c.authorize(ctx, req); err != nil {
		return err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("download artifact http %d", resp.StatusCode)
	}

	if destDir == "" {
		destDir = "."
	}
	return artifact.ExtractTarZstd(resp.Body, destDir)
}
