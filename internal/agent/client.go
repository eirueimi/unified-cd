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
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/artifact"
	"github.com/eirueimi/unified-cd/internal/objtransfer"
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
	// forceController is the agent's "objectTransfer: controller" setting:
	// artifacts use the controller's streaming routes and never a presigned URL.
	forceController bool
}

// SetObjectTransferController makes every artifact and cache transfer made
// through this Client stream through the controller (objectTransfer:
// controller) instead of using presigned URLs. Call it before the Client is
// shared.
func (c *Client) SetObjectTransferController(controller bool) { c.forceController = controller }

// invalidateToken discards a credential the controller rejected, when the
// token source supports it, so the next request obtains a fresh one.
func (c *Client) invalidateToken() {
	if source, ok := c.source.(invalidatingTokenSource); ok {
		source.Invalidate()
	}
}

// ObjectsFor returns the presign client for one run. It authenticates with the
// agent's own credential source, so token refresh is shared with every other
// controller call. Its HTTP client is left to objtransfer: a transfer of an
// archive must not be bound by this Client's 60s request timeout.
func (c *Client) ObjectsFor(runID string) *objtransfer.Client {
	return &objtransfer.Client{
		BaseURL:         c.base,
		RunID:           runID,
		Auth:            c.authorize,
		TempDir:         os.TempDir(),
		ForceController: c.forceController,
		OnUnauthorized:  c.invalidateToken,
	}
}

// invalidatingTokenSource can discard a rejected access credential. It is
// intentionally optional so static legacy credentials retain their existing
// one-request behavior.
type invalidatingTokenSource interface {
	Invalidate()
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
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		payload = b
	}
	resp, err := c.doAuthorized(ctx, method, path, payload, body != nil)
	if err != nil {
		return 0, err
	}
	// A 401 is safe to replay only for reads: authentication happens before the
	// controller handler, but keeping retries to GET avoids duplicating a future
	// non-idempotent write if that assumption changes.
	if method == http.MethodGet && resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		if source, ok := c.source.(invalidatingTokenSource); ok {
			source.Invalidate()
			resp, err = c.doAuthorized(ctx, method, path, payload, body != nil)
			if err != nil {
				return 0, err
			}
		}
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

func (c *Client) doAuthorized(ctx context.Context, method, path string, payload []byte, hasBody bool) (*http.Response, error) {
	var body io.Reader
	if hasBody {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if err := c.authorize(ctx, req); err != nil {
		return nil, err
	}
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.http.Do(req)
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

// Heartbeat refreshes the agent's last_seen_at on the controller and returns
// the controller-declared volume set the agent should offer.
//
// A nil req sends no body at all (bodyless), the legacy wire shape; the
// controller still answers with the volume set. A non-nil req is sent as the
// body even when it reports zero active runs (a non-nil empty ActiveRunIDs),
// so the controller can tell "live agent, zero active runs" (a reconcile
// candidate) from "legacy agent, no body" (skip, unknown).
//
// The result is (nil, nil) when the controller answers 204, which is what a
// controller that predates controller-declared volumes does: the caller must
// not treat that as an empty set.
func (c *Client) Heartbeat(ctx context.Context, agentID string, req *api.HeartbeatRequest) (*api.HeartbeatResponse, error) {
	var body any
	if req != nil {
		body = req
	}
	var out api.HeartbeatResponse
	code, err := c.do(ctx, http.MethodPost, "/api/v1/agents/"+agentID+"/heartbeat", body, &out)
	if err != nil {
		return nil, err
	}
	if code == http.StatusNoContent {
		return nil, nil
	}
	return &out, nil
}

// Claim requests an executable Run for the agent. labels is the list of agent
// labels. req says what the agent is holding (BusyVolumes: the controller will
// not hand it a run that needs one of them) and what it declares (Volumes: so a
// row the claim has to recreate keeps routing volume jobs); see api.ClaimRequest.
//
// The body is always sent, empty or not, so an idle claim and a busy one are the
// same request shape. A server that long-polls on the request context must read
// the body: net/http does not notice a client that disconnects until it has.
func (c *Client) Claim(ctx context.Context, agentID, timeout string, labels []string, req api.ClaimRequest) (api.ClaimResponse, error) {
	return c.claim(ctx, agentID, timeout, labels, &req, false)
}

// ClaimDetached long-polls the claim endpoint for a detached run (kind=detached),
// drawn from the controller's detached pool rather than the normal one. It
// sends no body: a detached job cannot use volumes, so there is nothing to gate.
func (c *Client) ClaimDetached(ctx context.Context, agentID, timeout string, labels []string) (api.ClaimResponse, error) {
	return c.claim(ctx, agentID, timeout, labels, nil, true)
}

func (c *Client) claim(ctx context.Context, agentID, timeout string, labels []string, req *api.ClaimRequest, detached bool) (api.ClaimResponse, error) {
	path := fmt.Sprintf("/api/v1/agents/%s/claim?timeout=%s", agentID, timeout)
	if len(labels) > 0 {
		path += "&labels=" + strings.Join(labels, ",")
	}
	if detached {
		path += "&kind=detached"
	}
	// A nil *ClaimRequest must reach do as a nil interface (no body), not as a
	// non-nil interface holding a nil pointer, which would marshal to "null".
	var body any
	if req != nil {
		body = req
	}
	var out api.ClaimResponse
	_, err := c.do(ctx, http.MethodPost, path, body, &out)
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

// ErrParentRunAlreadyTerminal is returned by CreateChildRun when the parent run
// finalized (e.g. reaper-failed or cancelled) between the agent claiming the
// call: step and spawning the child. The controller declines to create a child
// under a terminal parent and replies with an alreadyFinalized acknowledgement
// (no new run) rather than an error status. Callers must stop — there is no
// child run to poll.
var ErrParentRunAlreadyTerminal = errors.New("parent run already terminal; child run not created")

// CreateChildRun creates a child Run for a call: step, authorized by the
// parent run the agent has claimed. agentID/parentRunID scope the request to
// the agent-authenticated child-run endpoint (the human POST /api/v1/runs is
// not reachable with a uca_ credential). It returns ErrParentRunAlreadyTerminal
// if the parent had already finalized (the controller creates no child in that
// case).
func (c *Client) CreateChildRun(ctx context.Context, agentID, parentRunID, jobName string, params map[string]string) (api.Run, error) {
	body := api.TriggerRunRequest{JobName: jobName, Params: params}
	// The success response is a full api.Run; the terminal-parent response is
	// respondRunWriteVerdict's {"runId":…, "alreadyFinalized":true} (which
	// carries no "id", so run.ID stays empty). Decode both shapes at once.
	var resp struct {
		api.Run
		AlreadyFinalized bool `json:"alreadyFinalized"`
	}
	if _, err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/api/v1/agents/%s/runs/%s/children", agentID, parentRunID),
		body, &resp); err != nil {
		return api.Run{}, err
	}
	if resp.AlreadyFinalized {
		return api.Run{}, ErrParentRunAlreadyTerminal
	}
	return resp.Run, nil
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
func (c *Client) FetchSecrets(ctx context.Context, agentID, runID string, names []string) (map[string]string, error) {
	if len(names) == 0 {
		return map[string]string{}, nil
	}
	path := fmt.Sprintf("/api/v1/agents/%s/secrets/fetch", agentID)
	var out api.AgentFetchSecretsResponse
	_, err := c.do(ctx, http.MethodPost, path, api.AgentFetchSecretsRequest{RunID: runID, Names: names}, &out)
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
	return c.UploadArtifactExcluding(ctx, runID, name, path, nil)
}

// UploadArtifactExcluding is UploadArtifact that leaves the paths in exclude
// out of the archive (see artifact.WriteTarZstdExcluding).
func (c *Client) UploadArtifactExcluding(ctx context.Context, runID, name, path string, exclude []string) error {
	return c.uploadArtifactBody(ctx, runID, name, artifact.StreamTarZstdExcluding(path, exclude))
}

// UploadArtifactIn is UploadArtifactExcluding confined to root: path must lie
// at or below root and every component is resolved beneath root as it is read,
// so a symlink swapped in after the caller's checks is not followed out.
func (c *Client) UploadArtifactIn(ctx context.Context, runID, name, root, path string, exclude []string) error {
	return c.uploadArtifactBody(ctx, runID, name, artifact.StreamTarZstdIn(root, path, exclude))
}

func (c *Client) uploadArtifactBody(ctx context.Context, runID, name string, body io.ReadCloser) error {
	defer body.Close()

	if !c.forceController {
		key, err := artifact.ArtifactKey(runID, name)
		if err != nil {
			return err
		}
		return c.ObjectsFor(runID).Artifacts().Put(ctx, key, body, -1)
	}

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
// runID is the run that produced the artifact; claimRunID is the run being
// executed, whose presign endpoint authorizes a read of another run's artifact.
// In controller mode it fetches the stream from GET /api/v1/runs/{runID}/artifacts/{name}.
func (c *Client) DownloadArtifact(ctx context.Context, claimRunID, runID, name, destDir string) error {
	return c.DownloadArtifactExcluding(ctx, claimRunID, runID, name, destDir, nil)
}

// DownloadArtifactExcluding is DownloadArtifact that extracts nothing at or
// below the paths in exclude: the mount points of the volumes under destDir.
func (c *Client) DownloadArtifactExcluding(ctx context.Context, claimRunID, runID, name, destDir string, exclude []string) error {
	return c.DownloadArtifactWritten(ctx, claimRunID, runID, name, destDir, exclude, nil)
}

// DownloadArtifactWritten is DownloadArtifactExcluding that records what it
// extracted in w (nil records nothing).
func (c *Client) DownloadArtifactWritten(ctx context.Context, claimRunID, runID, name, destDir string, exclude []string, w *artifact.Written) error {
	return c.downloadArtifact(ctx, claimRunID, runID, name, destDir, func(r io.Reader, dest string) error {
		_, err := artifact.ExtractTarZstdDir(r, dest, exclude, true, w)
		return err
	})
}

// DownloadArtifactIn is DownloadArtifactWritten confined to root: destDir must
// lie at or below root and nothing is written through a symlink that leads out
// of root, including one swapped in after the caller's checks.
func (c *Client) DownloadArtifactIn(ctx context.Context, claimRunID, runID, name, root, destDir string, exclude []string, w *artifact.Written) error {
	return c.downloadArtifact(ctx, claimRunID, runID, name, destDir, func(r io.Reader, dest string) error {
		_, err := artifact.ExtractTarZstdDirIn(r, root, dest, exclude, true, w)
		return err
	})
}

func (c *Client) downloadArtifact(ctx context.Context, claimRunID, runID, name, destDir string, extract func(io.Reader, string) error) error {
	if !c.forceController {
		key, err := artifact.ArtifactKey(runID, name)
		if err != nil {
			return err
		}
		// runID is the run that produced the artifact; when it is not the
		// claimed run the controller authorizes the cross-run read.
		rc, err := c.ObjectsFor(claimRunID).Artifacts().Get(ctx, key)
		if err != nil {
			return err
		}
		defer rc.Close()
		if destDir == "" {
			destDir = "."
		}
		return extract(rc, destDir)
	}
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
	// Link entries are refused, exactly as the k8s sidecar's download does.
	return extract(resp.Body, destDir)
}
