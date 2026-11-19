package objtransfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/eirueimi/unified-cd/internal/api"
)

// cacheKeyHeader is the header the controller reports a restore's matched key
// in, percent-encoded with url.PathEscape.
const cacheKeyHeader = "X-Unified-Cache-Key"

// Controller mode: when the controller answers mode "controller" (its store
// cannot presign) or ForceController is set, archives stream through these
// routes instead of presigned URLs. The controller authenticates and scopes
// them like the presign endpoint, derives every key itself and, for a cache
// upload, writes .meta server-side.

// streamURL is the controller route for req's object. For uploads req carries
// the kind and name or key; ttlDays applies to cache uploads.
func (c *Client) streamURL(req api.PresignRequest, ttlDays int) (string, error) {
	base := strings.TrimRight(c.BaseURL, "/") + "/api/v1/runs/" + url.PathEscape(c.RunID) + "/objects/"
	q := url.Values{}
	switch req.Kind {
	case api.ObjectKindArtifact:
		if req.Name == "" {
			return "", errors.New("artifact name is required")
		}
		if req.SourceRunID != "" {
			q.Set("sourceRunId", req.SourceRunID)
		}
		base += "artifacts/" + url.PathEscape(req.Name)
	case api.ObjectKindCache:
		if req.Key == "" {
			return "", errors.New("cache key is required")
		}
		q.Set("key", req.Key)
		for _, k := range req.RestoreKeys {
			q.Add("restoreKey", k)
		}
		if ttlDays > 0 {
			q.Set("ttlDays", strconv.Itoa(ttlDays))
		}
		base += "caches"
	default:
		return "", fmt.Errorf("unknown object kind %q", req.Kind)
	}
	if len(q) > 0 {
		base += "?" + q.Encode()
	}
	return base, nil
}

// controllerUpload PUTs body to the controller with chunked transfer encoding,
// so the archive is never buffered whole. A body that can be rewound (the
// buffered replay of a small stream, an io.ReadSeeker) is retried on the
// transient answers presign retries (429, 503 unavailable, 409
// binding-pending); a one-shot stream is sent once and not retried, so the
// step's own retry semantics apply on top.
func (c *Client) controllerUpload(ctx context.Context, tmpl api.PresignRequest, ttlDays int, body io.Reader) error {
	endpoint, err := c.streamURL(tmpl, ttlDays)
	if err != nil {
		return err
	}
	rs, rewindable := body.(io.ReadSeeker)
	if !rewindable {
		return c.controllerUploadOnce(ctx, endpoint, tmpl.Kind, body)
	}
	start, err := rs.Seek(0, io.SeekCurrent)
	if err != nil {
		return c.controllerUploadOnce(ctx, endpoint, tmpl.Kind, body)
	}
	_, err = retryObjectCall(ctx, func() (struct{}, error) {
		if _, err := rs.Seek(start, io.SeekStart); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, c.controllerUploadOnce(ctx, endpoint, tmpl.Kind, rs)
	})
	return err
}

func (c *Client) controllerUploadOnce(ctx context.Context, endpoint, kind string, body io.Reader) error {
	// Hide the concrete type of body so net/http cannot infer a length: the
	// request is chunked.
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, struct{ io.Reader }{body})
	if err != nil {
		return fmt.Errorf("build controller upload request: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if c.Auth != nil {
		if err := c.Auth(ctx, req); err != nil {
			return fmt.Errorf("authenticate controller upload: %w", err)
		}
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("controller upload: %w", redactErr(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return classify(objectResponseError(resp, kind+"/stream-put"))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return nil
}

// lengthChecked fails a read that ends before the Content-Length the
// controller declared, so a truncated download is never taken for a complete
// one.
type lengthChecked struct {
	rc   io.ReadCloser
	want int64
	got  int64
}

func (l *lengthChecked) Read(p []byte) (int, error) {
	n, err := l.rc.Read(p)
	l.got += int64(n)
	if errors.Is(err, io.EOF) && l.got != l.want {
		return n, fmt.Errorf("controller download ended after %d of %d bytes: %w", l.got, l.want, io.ErrUnexpectedEOF)
	}
	return n, err
}

func (l *lengthChecked) Close() error { return l.rc.Close() }

// controllerDownload opens a GET of req (an artifact or cache get) on the
// controller. A transient answer (binding pending, rate limit) is retried like
// a presign request. Unlike presigned downloads the stream is not resumed.
func (c *Client) controllerDownload(ctx context.Context, req api.PresignRequest) (io.ReadCloser, api.PresignResponse, error) {
	endpoint, err := c.streamURL(req, 0)
	if err != nil {
		return nil, api.PresignResponse{}, err
	}
	resp, err := retryObjectCall(ctx, func() (*http.Response, error) {
		hr, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("build controller download request: %w", err)
		}
		if c.Auth != nil {
			if err := c.Auth(ctx, hr); err != nil {
				return nil, fmt.Errorf("authenticate controller download: %w", err)
			}
		}
		resp, err := c.httpClient().Do(hr)
		if err != nil {
			return nil, fmt.Errorf("controller download: %w", redactErr(err))
		}
		if resp.StatusCode != http.StatusOK {
			defer resp.Body.Close()
			return nil, objectResponseError(resp, req.Kind+"/stream-get")
		}
		return resp, nil
	})
	if err != nil {
		return nil, api.PresignResponse{}, err
	}
	out := api.PresignResponse{Mode: api.ObjectModeController, Size: resp.ContentLength}
	if h := resp.Header.Get(cacheKeyHeader); h != "" {
		matched, err := url.PathUnescape(h)
		if err != nil {
			matched = h
		}
		out.MatchedKey = matched
	} else if req.Kind == api.ObjectKindCache {
		out.MatchedKey = req.Key
	}
	if resp.ContentLength >= 0 {
		return &lengthChecked{rc: resp.Body, want: resp.ContentLength}, out, nil
	}
	return resp.Body, out, nil
}
