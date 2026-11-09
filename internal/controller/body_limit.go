package controller

import (
	"errors"
	"io"
	"net/http"
	"strings"
)

const (
	// defaultMaxRequestBodyBytes is the body cap applied to every API route
	// that has no more specific limit (Config.MaxRequestBodyBytes == 0).
	defaultMaxRequestBodyBytes int64 = 1 << 20

	// maxAgentLogBodyBytes caps the bulk log routes an agent posts to. A flush
	// can carry many batches of lines (each line up to 256 KiB), so these need
	// far more headroom than a JSON control request, but they are still bounded.
	maxAgentLogBodyBytes int64 = 16 << 20
)

// requestBodyLimit rejects request bodies larger than the cap with 413.
//
// Exempt routes keep their own handling: webhook ingress enforces its own
// configurable limit (WebhookMaxBodyBytes) and the artifact upload streams an
// arbitrarily large archive to the object store. Agent log routes get
// maxAgentLogBodyBytes.
//
// A declared Content-Length over the cap is refused before the handler runs.
// For chunked or understated bodies the limit trips during the read; handlers
// answer a failed body decode with 400, which is rewritten to 413 when the
// read failed because of the cap.
func requestBodyLimit(defaultMax int64) func(http.Handler) http.Handler {
	if defaultMax <= 0 {
		defaultMax = defaultMaxRequestBodyBytes
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			limit, limited := bodyLimitFor(r, defaultMax)
			if !limited || r.Body == nil || r.Body == http.NoBody {
				next.ServeHTTP(w, r)
				return
			}
			if r.ContentLength > limit {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			lw := &limitedResponseWriter{ResponseWriter: w}
			r.Body = &trippingBody{rc: http.MaxBytesReader(lw, r.Body, limit), w: lw}
			next.ServeHTTP(lw, r)
		})
	}
}

func bodyLimitFor(r *http.Request, defaultMax int64) (int64, bool) {
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/webhook/"):
		return 0, false
	case r.Method == http.MethodPut && strings.HasPrefix(p, "/api/v1/runs/") && strings.Contains(p, "/artifacts/"):
		return 0, false
	case strings.HasPrefix(p, "/api/v1/agents/") && (strings.HasSuffix(p, "/logs") || strings.HasSuffix(p, "/logs/bulk")):
		return max(defaultMax, maxAgentLogBodyBytes), true
	}
	return defaultMax, true
}

// trippingBody records that a read failed because the size cap was exceeded.
type trippingBody struct {
	rc io.ReadCloser
	w  *limitedResponseWriter
}

func (b *trippingBody) Close() error { return b.rc.Close() }

func (b *trippingBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		b.w.tripped = true
	}
	return n, err
}

// limitedResponseWriter turns the 400 a handler writes after a failed body
// read into 413 when the read failed on the size cap.
type limitedResponseWriter struct {
	http.ResponseWriter
	tripped     bool
	wroteHeader bool
}

func (w *limitedResponseWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		if w.tripped && code == http.StatusBadRequest {
			code = http.StatusRequestEntityTooLarge
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *limitedResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *limitedResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *limitedResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
