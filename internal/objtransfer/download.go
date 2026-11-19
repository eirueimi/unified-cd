package objtransfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/objectstore"
)

// maxResumes bounds consecutive resumes that make no progress.
const maxResumes = 5

// openDownload presigns a get for req and opens the object. The returned
// reader resumes a dropped transfer from the last byte read.
func (c *Client) openDownload(ctx context.Context, req api.PresignRequest) (io.ReadCloser, api.PresignResponse, error) {
	resp, err := c.presign(ctx, req)
	if err != nil {
		return nil, api.PresignResponse{}, err
	}
	if resp.Request == nil {
		return nil, api.PresignResponse{}, errors.New("presign get: the controller returned no request")
	}
	r := &resumingReader{ctx: ctx, c: c, req: req, etag: resp.ETag, size: resp.Size}
	if err := r.open(*resp.Request); err != nil {
		return nil, api.PresignResponse{}, err
	}
	return r, resp, nil
}

// resumingReader reads an object through presigned GETs. When the body fails
// part way it presigns again and continues with Range: bytes=<offset>- and
// If-Match: <etag>, so a replaced object fails the read instead of yielding a
// mix of two versions.
type resumingReader struct {
	ctx  context.Context
	c    *Client
	req  api.PresignRequest // the get request, repeated to refresh the URL
	etag string
	size int64

	off      int64
	body     io.ReadCloser
	failures int
	closed   bool
}

// quoteETag returns etag in header form (S3 stores answer with quotes; the
// controller may report it unquoted).
func quoteETag(etag string) string {
	if strings.HasPrefix(etag, `"`) || strings.HasPrefix(etag, "W/") {
		return etag
	}
	return `"` + etag + `"`
}

// open sends the GET for pr, from r.off on.
func (r *resumingReader) open(pr api.PresignedRequest) error {
	req, err := storeRequest(r.ctx, pr, nil)
	if err != nil {
		return err
	}
	if r.off > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", r.off))
		if r.etag != "" {
			req.Header.Set("If-Match", quoteETag(r.etag))
		}
	}
	resp, err := r.c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", redactErr(err))
	}
	switch {
	case resp.StatusCode == http.StatusPartialContent && r.off > 0:
		if cr := resp.Header.Get("Content-Range"); cr != "" && !strings.HasPrefix(cr, fmt.Sprintf("bytes %d-", r.off)) {
			resp.Body.Close()
			return fmt.Errorf("download: the store answered an unexpected range %q", cr)
		}
	case resp.StatusCode == http.StatusOK && r.off == 0:
	case resp.StatusCode == http.StatusPreconditionFailed:
		resp.Body.Close()
		return fmt.Errorf("object changed during download (at byte %d)", r.off)
	case resp.StatusCode == http.StatusNotFound && r.off > 0:
		resp.Body.Close()
		return fmt.Errorf("object removed during download (at byte %d): %w", r.off, objectstore.ErrNotFound)
	case resp.StatusCode == http.StatusNotFound:
		resp.Body.Close()
		return fmt.Errorf("download: %w", objectstore.ErrNotFound)
	default:
		resp.Body.Close()
		return fmt.Errorf("download: store answered %s", resp.Status)
	}
	r.body = resp.Body
	return nil
}

// reopen presigns again and resumes at the current offset.
func (r *resumingReader) reopen() error {
	resp, err := r.c.presign(r.ctx, r.req)
	if err != nil {
		return err
	}
	if resp.Request == nil {
		return errors.New("presign get: the controller returned no request")
	}
	if r.etag != "" && resp.ETag != "" && resp.ETag != r.etag {
		return fmt.Errorf("object changed during download (at byte %d)", r.off)
	}
	return r.open(*resp.Request)
}

// Read implements io.Reader.
func (r *resumingReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, errors.New("read of a closed download")
	}
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if r.body == nil {
			if r.failures > maxResumes {
				return 0, errors.New("download: giving up after repeated dropped connections")
			}
			if r.failures > 0 {
				if err := sleep(r.ctx, backoff(r.failures-1, 0)); err != nil {
					return 0, err
				}
			}
			if err := r.reopen(); err != nil {
				return 0, err
			}
		}
		n, err := r.body.Read(p)
		r.off += int64(n)
		if n > 0 {
			r.failures = 0
		}
		if err == nil {
			return n, nil
		}
		if errors.Is(err, io.EOF) {
			if r.size <= 0 || r.off >= r.size {
				return n, io.EOF
			}
			err = io.ErrUnexpectedEOF
		} else if r.size > 0 && r.off >= r.size {
			return n, io.EOF
		}
		if r.ctx.Err() != nil {
			return n, r.ctx.Err()
		}
		// The connection dropped before the whole object arrived: resume.
		r.body.Close()
		r.body = nil
		r.failures++
		if n > 0 {
			return n, nil
		}
		if r.failures > maxResumes {
			return 0, fmt.Errorf("download interrupted at byte %d: %w", r.off, redactErr(err))
		}
	}
}

// Close implements io.Closer.
func (r *resumingReader) Close() error {
	r.closed = true
	if r.body == nil {
		return nil
	}
	err := r.body.Close()
	r.body = nil
	return err
}
