package objtransfer

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
)

const (
	// MinPartSize is the size of the first parts of a multipart upload.
	MinPartSize = 64 << 20
	// MaxPartSize is the S3 limit on one part.
	MaxPartSize = 5 << 30
	// MaxParts is the S3 limit on parts in one multipart upload.
	MaxParts = api.MaxMultipartParts

	// partURLBatch is how many part URLs are requested at once.
	partURLBatch = 10
	// partAttempts bounds the tries of one PUT to the store (403 re-presigns,
	// 5xx and transport errors back off).
	partAttempts = 4
	// urlRefreshMargin: a presigned URL this close to expiry is requested
	// again before use.
	urlRefreshMargin = time.Minute
)

// Package variables so tests can use tiny parts.
var (
	minPartSize       int64 = MinPartSize
	partDoublingEvery       = 1000
)

// PartSize is the size of part n (1-based): MinPartSize for the first 1,000
// parts, doubling every 1,000 parts after that, capped at MaxPartSize. With
// 10,000 parts this lifts the largest uploadable object well past what a fixed
// 64 MiB part would allow (625 GiB) while small objects still use small parts.
func PartSize(n int) int64 {
	if n < 1 {
		n = 1
	}
	size := minPartSize
	for i := (n - 1) / partDoublingEvery; i > 0; i-- {
		size *= 2
		if size >= MaxPartSize {
			return MaxPartSize
		}
	}
	return min(size, MaxPartSize)
}

// upload streams body to the store through presigned URLs. tmpl carries the
// kind and the name or key that identify the object. The stream is buffered one
// part at a time in a temp file under c.TempDir, so a failed part is resent
// byte for byte. A stream that ends within the first part is sent with a single
// PUT; a larger one uses a multipart upload, or, when the store does not accept
// presigned multipart uploads, is spooled to the temp file and sent with a
// single PUT.
func (c *Client) upload(ctx context.Context, tmpl api.PresignRequest, body io.Reader) error {
	if c.ForceController {
		return errControllerMode
	}
	br := bufio.NewReader(body)
	f, err := c.tempFile()
	if err != nil {
		return err
	}
	defer func() {
		f.Close()
		os.Remove(f.Name())
	}()

	n, eof, err := fillPart(f, br, PartSize(1))
	if err != nil {
		return fmt.Errorf("buffer upload: %w", err)
	}
	if eof {
		return c.putFile(ctx, tmpl, f, n)
	}

	start, err := c.presign(ctx, withOp(tmpl, api.ObjectOpMultipartStart))
	if errors.Is(err, errMultipartUnavailable) {
		// Spool the rest of the stream behind part 1 and send it whole.
		if _, err := io.Copy(f, br); err != nil {
			return fmt.Errorf("buffer upload: %w", err)
		}
		st, err := f.Stat()
		if err != nil {
			return fmt.Errorf("buffer upload: %w", err)
		}
		return c.putFile(ctx, tmpl, f, st.Size())
	}
	if err != nil {
		return err
	}
	if start.UploadID == "" {
		return errors.New("presign multipart-start: the controller returned no upload ID")
	}
	parts, err := c.uploadParts(ctx, tmpl, start.UploadID, f, n, br)
	if err == nil {
		complete := withOp(tmpl, api.ObjectOpMultipartComplete)
		complete.UploadID, complete.Parts = start.UploadID, parts
		_, err = c.presign(ctx, complete)
	}
	if err != nil {
		c.abort(tmpl, start.UploadID)
		return err
	}
	return nil
}

func withOp(tmpl api.PresignRequest, op string) api.PresignRequest {
	tmpl.Op = op
	return tmpl
}

// abort drops an unfinished multipart upload, best effort and independent of
// the caller's context (which may be the reason the upload failed).
func (c *Client) abort(tmpl api.PresignRequest, uploadID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req := withOp(tmpl, api.ObjectOpMultipartAbort)
	req.UploadID = uploadID
	_, _ = c.presign(ctx, req)
}

// fillPart copies up to limit bytes of br into f (which must be empty and
// positioned at 0). eof reports that br has no more data after this part.
func fillPart(f *os.File, br *bufio.Reader, limit int64) (n int64, eof bool, err error) {
	n, err = io.CopyN(f, br, limit)
	if errors.Is(err, io.EOF) {
		return n, true, nil
	}
	if err != nil {
		return n, false, err
	}
	// The part is full; look ahead so a stream that ends exactly here does not
	// produce an empty trailing part.
	if _, perr := br.Peek(1); perr != nil {
		if errors.Is(perr, io.EOF) {
			return n, true, nil
		}
		return n, false, perr
	}
	return n, false, nil
}

// uploadParts sends part 1 (already in f, n bytes) and then the rest of br,
// one part at a time, and returns the ETags in part order.
func (c *Client) uploadParts(ctx context.Context, tmpl api.PresignRequest, uploadID string, f *os.File, n int64, br *bufio.Reader) ([]api.ObjectPart, error) {
	urls := map[int]api.PresignedRequest{}
	fetch := func(from, to int) error {
		req := withOp(tmpl, api.ObjectOpMultipartParts)
		req.UploadID = uploadID
		for p := from; p <= to; p++ {
			req.PartNumbers = append(req.PartNumbers, p)
		}
		resp, err := c.presign(ctx, req)
		if err != nil {
			return err
		}
		for _, pr := range resp.Parts {
			urls[pr.PartNumber] = pr
		}
		if _, ok := urls[from]; !ok {
			return fmt.Errorf("presign multipart-parts: the controller returned no URL for part %d", from)
		}
		return nil
	}

	var parts []api.ObjectPart
	for part := 1; ; part++ {
		if part > MaxParts {
			return nil, fmt.Errorf("upload exceeds the maximum of %d parts", MaxParts)
		}
		eof := false
		if part > 1 {
			if err := f.Truncate(0); err != nil {
				return nil, fmt.Errorf("reset part buffer: %w", err)
			}
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return nil, fmt.Errorf("reset part buffer: %w", err)
			}
			var err error
			if n, eof, err = fillPart(f, br, PartSize(part)); err != nil {
				return nil, fmt.Errorf("buffer part %d: %w", part, err)
			}
		}
		pr, ok := urls[part]
		switch {
		case !ok:
			if err := fetch(part, min(part+partURLBatch-1, MaxParts)); err != nil {
				return nil, err
			}
		case time.Until(pr.ExpiresAt) < urlRefreshMargin:
			if err := fetch(part, part); err != nil {
				return nil, err
			}
		}
		etag, err := c.sendPart(ctx, f, n, urls[part], func() (api.PresignedRequest, error) {
			if err := fetch(part, part); err != nil {
				return api.PresignedRequest{}, err
			}
			return urls[part], nil
		})
		if err != nil {
			return nil, fmt.Errorf("upload part %d: %w", part, err)
		}
		if etag == "" {
			return nil, fmt.Errorf("upload part %d: the store returned no ETag", part)
		}
		parts = append(parts, api.ObjectPart{PartNumber: part, ETag: etag})
		if eof {
			return parts, nil
		}
	}
}

// putFile sends the first size bytes of f as one object with a single
// presigned PUT.
func (c *Client) putFile(ctx context.Context, tmpl api.PresignRequest, f *os.File, size int64) error {
	get := func() (api.PresignedRequest, error) {
		resp, err := c.presign(ctx, withOp(tmpl, api.ObjectOpPut))
		if err != nil {
			return api.PresignedRequest{}, err
		}
		if resp.Request == nil {
			return api.PresignedRequest{}, errors.New("presign put: the controller returned no request")
		}
		return *resp.Request, nil
	}
	pr, err := get()
	if err != nil {
		return err
	}
	_, err = c.sendPart(ctx, f, size, pr, get)
	return err
}

// sendPart PUTs the first size bytes of f to pr and returns the ETag the store
// answered. A 403 (an expired or rejected URL) fetches a fresh URL with
// refresh; 5xx and transport errors back off and retry the same URL. The
// bytes are re-read from f each time, so every attempt sends identical data.
func (c *Client) sendPart(ctx context.Context, f *os.File, size int64, pr api.PresignedRequest, refresh func() (api.PresignedRequest, error)) (string, error) {
	var lastErr error
	for attempt := 0; attempt < partAttempts; attempt++ {
		if attempt > 0 {
			if err := sleep(ctx, backoff(attempt-1, 0)); err != nil {
				return "", err
			}
		}
		var body io.Reader = http.NoBody
		if size > 0 {
			body = io.NewSectionReader(f, 0, size)
		}
		req, err := storeRequest(ctx, pr, body)
		if err != nil {
			return "", err
		}
		req.ContentLength = size
		resp, err := c.httpClient().Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			lastErr = redactErr(err)
			continue
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		switch {
		case resp.StatusCode/100 == 2:
			return resp.Header.Get("ETag"), nil
		case resp.StatusCode == http.StatusForbidden:
			lastErr = fmt.Errorf("store answered %s", resp.Status)
			if attempt == partAttempts-1 {
				break
			}
			next, err := refresh()
			if err != nil {
				return "", err
			}
			pr = next
		case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
			lastErr = fmt.Errorf("store answered %s", resp.Status)
		default:
			return "", fmt.Errorf("store answered %s", resp.Status)
		}
	}
	return "", fmt.Errorf("giving up after %d attempts: %w", partAttempts, lastErr)
}
