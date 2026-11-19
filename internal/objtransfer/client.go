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
	"sync/atomic"
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
	// ForceController skips the presign endpoint and streams every transfer
	// through the controller (controller_mode.go), for stores the client
	// cannot reach directly.
	ForceController bool

	// controller is set once the presign endpoint answered mode "controller";
	// later transfers of this Client go straight to the streaming routes.
	controller atomic.Bool
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
// unavailable are retried with backoff. Anything else fails. When the
// controller answers mode "controller" (or ForceController is set) the result
// is errControllerMode, and the decision is remembered for this Client.
func (c *Client) presign(ctx context.Context, req api.PresignRequest) (api.PresignResponse, error) {
	if c.useController() {
		return api.PresignResponse{}, errControllerMode
	}
	resp, err := retryObjectCall(ctx, func() (api.PresignResponse, error) { return c.presignOnce(ctx, req) })
	if err != nil {
		return api.PresignResponse{}, err
	}
	if resp.Mode == api.ObjectModeController {
		c.controller.Store(true)
		return api.PresignResponse{}, errControllerMode
	}
	return resp, nil
}

// useController reports whether transfers stream through the controller:
// forced by the caller, or learned from an earlier presign answer.
func (c *Client) useController() bool { return c.ForceController || c.controller.Load() }

// retryObjectCall runs fn, retrying the answers the controller marks as
// transient (see presign). Errors that are not a *presignError pass through.
func retryObjectCall[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	var zero T
	start := time.Now()
	var waited time.Duration
	for attempt := 0; ; attempt++ {
		resp, err := fn()
		if err == nil {
			return resp, nil
		}
		var pe *presignError
		if !errors.As(err, &pe) {
			return zero, err
		}
		var delay time.Duration
		switch {
		case pe.Status == http.StatusConflict && pe.Code == api.ObjectErrBindingPending:
			delay = pe.RetryAfter
			if delay <= 0 {
				delay = bindingRetryDelay
			}
			if time.Since(start)+delay > bindingWait || waited+delay > bindingWait {
				return zero, fmt.Errorf("%w (still %s after %s)", err, api.ObjectErrBindingPending, bindingWait)
			}
		case pe.Status == http.StatusTooManyRequests,
			pe.Status == http.StatusServiceUnavailable && pe.Code == api.ObjectErrUnavailable:
			if attempt == retryAttempts-1 {
				return zero, err
			}
			delay = backoff(attempt, pe.RetryAfter)
		default:
			return zero, classify(pe)
		}
		waited += delay
		if err := sleep(ctx, delay); err != nil {
			return zero, err
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
		return api.PresignResponse{}, objectResponseError(resp, req.Kind+"/"+req.Op)
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

// objectResponseError turns a non-2xx answer of a controller object route into
// a *presignError. It reads a bounded part of the body, not the whole of it.
func objectResponseError(resp *http.Response, op string) *presignError {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	pe := &presignError{
		Status:     resp.StatusCode,
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		op:         op,
	}
	var oe api.ObjectError
	if json.Unmarshal(raw, &oe) == nil && oe.Code != "" {
		pe.Code, pe.Message = oe.Code, oe.Error
	} else {
		pe.Message = http.StatusText(resp.StatusCode)
	}
	return pe
}
