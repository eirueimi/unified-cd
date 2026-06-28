package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/unified-cd/unified-cd/internal/api"
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
	base  string
	token string
	http  *http.Client
}

// NewClient creates a new client with the given base URL and token.
func NewClient(baseURL, token string) *Client {
	return &Client{
		base:  baseURL,
		token: token,
		http:  &http.Client{Timeout: 60 * time.Second},
	}
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
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, &HTTPError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode, nil
}

// Register registers the agent with the master server.
func (c *Client) Register(ctx context.Context, req api.AgentRegisterRequest) error {
	_, err := c.do(ctx, http.MethodPost, "/api/v1/agents/register", req, nil)
	return err
}

// Deregister removes the agent from the master server.
func (c *Client) Deregister(ctx context.Context, agentID string) error {
	_, err := c.do(ctx, http.MethodDelete, "/api/v1/agents/"+agentID, nil, nil)
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

// SetStepOutputs sends the step outputs to the master server.
func (c *Client) SetStepOutputs(ctx context.Context, agentID, runID string, stepIndex int, outputs map[string]string) error {
	path := fmt.Sprintf("/api/v1/agents/%s/runs/%s/steps/%d/outputs", agentID, runID, stepIndex)
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

// UploadArtifact archives path as tar+zstd and uploads it to the master server.
// Sends to PUT /api/v1/runs/{runID}/artifacts/{name}.
func (c *Client) UploadArtifact(ctx context.Context, runID, name, path string) error {
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf)
	if err != nil {
		return fmt.Errorf("zstd writer: %w", err)
	}
	tw := tar.NewWriter(enc)

	if err := filepath.WalkDir(path, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(path, p)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !d.IsDir() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(tw, f)
			return err
		}
		return nil
	}); err != nil {
		return fmt.Errorf("tar walk %q: %w", path, err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("tar close: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("zstd close: %w", err)
	}

	url := c.base + fmt.Sprintf("/api/v1/runs/%s/artifacts/%s", runID, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(buf.Bytes()))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(buf.Len())

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload artifact http %d: %s", resp.StatusCode, string(b))
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
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("download artifact http %d: %s", resp.StatusCode, string(b))
	}

	if destDir == "" {
		destDir = "."
	}
	return extractTarZstd(resp.Body, destDir)
}

// extractTarZstd extracts a tar+zstd stream into dest.
// Includes path checks to prevent path traversal attacks.
func extractTarZstd(r io.Reader, dest string) error {
	dec, err := zstd.NewReader(r)
	if err != nil {
		return fmt.Errorf("zstd reader: %w", err)
	}
	defer dec.Close()

	cleanDest := filepath.Clean(dest) + string(filepath.Separator)
	tr := tar.NewReader(dec)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}
		target := filepath.Join(dest, filepath.FromSlash(hdr.Name))
		if !strings.HasPrefix(target+string(filepath.Separator), cleanDest) {
			return fmt.Errorf("invalid path %q in artifact archive", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
		default:
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return err
			}
			f, err := os.Create(target)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
	return nil
}
