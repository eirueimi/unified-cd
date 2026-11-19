// Package objtransfer moves artifact and cache archives between a run and the
// object store through presigned URLs issued by the controller. The caller
// holds no store credentials: every request to the store carries only the
// presigned URL it was handed. Those URLs are bearer credentials, so they are
// never logged and never appear in an error (see redactErr).
package objtransfer

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
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/cache"
	"github.com/eirueimi/unified-cd/internal/objectstore"
)

// Client requests presigned URLs for one run and transfers objects with them.
type Client struct {
	// BaseURL is the controller's base URL.
	BaseURL string
	// RunID is the run whose presign endpoint is used.
	RunID string
	// Auth sets the credentials of a controller request. It is called for
	// presign requests only, never for requests to the store.
	Auth func(context.Context, *http.Request) error
	// HTTP is used for both controller and store requests; nil means
	// http.DefaultClient.
	HTTP *http.Client
	// TempDir holds the part buffers of an upload; empty means os.TempDir().
	TempDir string
	// ForceController skips the presign endpoint and reports controller mode
	// (see errControllerMode), for stores the client cannot reach directly.
	ForceController bool
}

// errControllerMode means the transfer must stream through the controller
// instead of using presigned URLs.
var errControllerMode = errors.New("objtransfer: the controller streams objects for this store")

// errMultipartUnavailable means the store does not accept presigned multipart
// uploads; the caller falls back to a single PUT.
var errMultipartUnavailable = errors.New("objtransfer: multipart upload unavailable")

// presignError is a non-2xx answer of the presign endpoint.
type presignError struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
	op         string
}

func (e *presignError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("presign %s: %d %s", e.op, e.Status, e.Message)
	}
	return fmt.Sprintf("presign %s: %d %s: %s", e.op, e.Status, e.Code, e.Message)
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return defaultHTTP
}

// defaultHTTP is used when the caller supplies no client: it bounds the wait
// for response headers but not the whole request, which can legitimately last
// as long as a large transfer.
var defaultHTTP = func() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 60 * time.Second
	return &http.Client{Transport: tr}
}()

// redactURL drops the query string and userinfo of raw: presigned URLs carry
// their credential there.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		if i := strings.IndexAny(raw, "?#"); i >= 0 {
			raw = raw[:i]
		}
		return raw
	}
	u.RawQuery, u.Fragment, u.User = "", "", nil
	return u.String()
}

// redactErr returns err with the query string stripped from the URL a
// *url.Error carries (net/http includes the full request URL in transport
// errors).
func redactErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s %s: %w", ue.Op, redactURL(ue.URL), ue.Err)
	}
	return err
}

// presign asks the controller for a presigned request or an upload/commit
// step. A 409 binding-pending is retried for up to bindingWait; 429 and 503
// unavailable are retried with backoff. Anything else fails.
func (c *Client) presign(ctx context.Context, req api.PresignRequest) (api.PresignResponse, error) {
	if c.ForceController {
		return api.PresignResponse{}, errControllerMode
	}
	start := time.Now()
	var waited time.Duration
	for attempt := 0; ; attempt++ {
		resp, err := c.presignOnce(ctx, req)
		if err == nil {
			if resp.Mode == api.ObjectModeController {
				return api.PresignResponse{}, errControllerMode
			}
			return resp, nil
		}
		var pe *presignError
		if !errors.As(err, &pe) {
			return api.PresignResponse{}, err
		}
		var delay time.Duration
		switch {
		case pe.Status == http.StatusConflict && pe.Code == api.ObjectErrBindingPending:
			delay = pe.RetryAfter
			if delay <= 0 {
				delay = bindingRetryDelay
			}
			if time.Since(start)+delay > bindingWait || waited+delay > bindingWait {
				return api.PresignResponse{}, fmt.Errorf("%w (still %s after %s)", err, api.ObjectErrBindingPending, bindingWait)
			}
		case pe.Status == http.StatusTooManyRequests,
			pe.Status == http.StatusServiceUnavailable && pe.Code == api.ObjectErrUnavailable:
			if attempt == retryAttempts-1 {
				return api.PresignResponse{}, err
			}
			delay = backoff(attempt, pe.RetryAfter)
		default:
			return api.PresignResponse{}, classify(pe)
		}
		waited += delay
		if err := sleep(ctx, delay); err != nil {
			return api.PresignResponse{}, err
		}
	}
}

// classify maps a presign error to the sentinel errors callers match on.
func classify(pe *presignError) error {
	switch pe.Code {
	case api.ObjectErrNotFound:
		return fmt.Errorf("%w: %w", objectstore.ErrNotFound, pe)
	case api.ObjectErrCacheMiss:
		return fmt.Errorf("%w: %w", cache.ErrCacheMiss, pe)
	case api.ObjectErrMultipartUnavailable:
		return fmt.Errorf("%w: %w", errMultipartUnavailable, pe)
	}
	return pe
}

func (c *Client) presignOnce(ctx context.Context, req api.PresignRequest) (api.PresignResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return api.PresignResponse{}, fmt.Errorf("encode presign request: %w", err)
	}
	endpoint := strings.TrimRight(c.BaseURL, "/") + "/api/v1/runs/" + url.PathEscape(c.RunID) + "/objects/presign"
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return api.PresignResponse{}, fmt.Errorf("build presign request: %w", err)
	}
	hr.Header.Set("Content-Type", "application/json")
	if c.Auth != nil {
		if err := c.Auth(ctx, hr); err != nil {
			return api.PresignResponse{}, fmt.Errorf("authenticate presign request: %w", err)
		}
	}
	resp, err := c.httpClient().Do(hr)
	if err != nil {
		return api.PresignResponse{}, fmt.Errorf("presign %s: %w", req.Op, redactErr(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		pe := &presignError{
			Status:     resp.StatusCode,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			op:         req.Kind + "/" + req.Op,
		}
		var oe api.ObjectError
		if json.Unmarshal(raw, &oe) == nil && oe.Code != "" {
			pe.Code, pe.Message = oe.Code, oe.Error
		} else {
			pe.Message = http.StatusText(resp.StatusCode)
		}
		return api.PresignResponse{}, pe
	}
	var out api.PresignResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&out); err != nil {
		return api.PresignResponse{}, fmt.Errorf("decode presign response: %w", err)
	}
	return out, nil
}

// storeRequest builds a request for a presigned URL, applying the headers the
// controller said to send as-is. No controller credentials are attached.
func storeRequest(ctx context.Context, pr api.PresignedRequest, body io.Reader) (*http.Request, error) {
	method := pr.Method
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, pr.URL, body)
	if err != nil {
		return nil, fmt.Errorf("build store request for %s: %w", redactURL(pr.URL), redactErr(err))
	}
	for k, v := range pr.Headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

// tempFile creates a part buffer in TempDir.
func (c *Client) tempFile() (*os.File, error) {
	f, err := os.CreateTemp(c.TempDir, "objtransfer-*")
	if err != nil {
		return nil, fmt.Errorf("create temp file: %w", err)
	}
	return f, nil
}
