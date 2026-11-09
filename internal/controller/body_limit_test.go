package controller

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chunkedBody hides the length from net/http so the declared-Content-Length
// shortcut cannot apply and the read-time cap is what gets exercised.
type chunkedBody struct{ io.Reader }

func bigJSON(n int) string { return `{"token":"` + strings.Repeat("a", n) + `"}` }

func TestRequestBodyLimit_RejectsOversizeOnUnauthenticatedEndpoints(t *testing.T) {
	s := NewServer(Config{MaxRequestBodyBytes: 1024}, nil)
	for _, path := range []string{"/api/v1/agents/enroll", "/api/v1/agents/token/refresh", "/api/v1/store-credentials"} {
		t.Run(path+" declared length", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(bigJSON(4096)))
			rec := httptest.NewRecorder()
			s.Router().ServeHTTP(rec, req)
			assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
		})
		t.Run(path+" chunked", func(t *testing.T) {
			if path != "/api/v1/store-credentials" {
				t.Skip("the enroll and refresh handlers authenticate before reading the body, so a chunked body is never read without credentials")
			}
			req := httptest.NewRequest(http.MethodPost, path, chunkedBody{strings.NewReader(bigJSON(4096))})
			req.ContentLength = -1
			rec := httptest.NewRecorder()
			s.Router().ServeHTTP(rec, req)
			assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
		})
	}
}

func TestRequestBodyLimit_SmallBodyPasses(t *testing.T) {
	s := NewServer(Config{MaxRequestBodyBytes: 1024}, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/store-credentials", strings.NewReader(`{"token":""}`))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "an in-limit but invalid body is still a 400")
}

func TestRequestBodyLimit_DefaultIsOneMiB(t *testing.T) {
	var got int64
	h := requestBodyLimit(0)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		got = n
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader(strings.Repeat("x", 1<<20)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, int64(1<<20), got)

	req = httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader(strings.Repeat("x", 1<<20+1)))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

func TestRequestBodyLimit_ExemptAndSpecialRoutes(t *testing.T) {
	read := func(method, path string, size int) int {
		var status = http.StatusOK
		h := requestBodyLimit(1024)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := io.Copy(io.Discard, r.Body); err != nil {
				status = http.StatusBadRequest
				w.WriteHeader(http.StatusBadRequest)
			}
		}))
		req := httptest.NewRequest(method, path, chunkedBody{strings.NewReader(strings.Repeat("x", size))})
		req.ContentLength = -1
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		_ = status
		return rec.Code
	}
	assert.Equal(t, http.StatusOK, read(http.MethodPut, "/api/v1/runs/r1/artifacts/out", 1<<20), "artifact upload streams without the JSON cap")
	assert.Equal(t, http.StatusOK, read(http.MethodPost, "/webhook/hook", 1<<20), "webhook ingress enforces its own limit")
	assert.Equal(t, http.StatusOK, read(http.MethodPost, "/api/v1/agents/a1/runs/r1/steps/0/logs/bulk", 1<<20), "agent log bulk gets the larger cap")
	assert.Equal(t, http.StatusRequestEntityTooLarge, read(http.MethodPost, "/api/v1/agents/a1/runs/r1/steps/0/logs/bulk", int(maxAgentLogBodyBytes)+1))
	assert.Equal(t, http.StatusRequestEntityTooLarge, read(http.MethodPost, "/api/v1/agents/a1/runs/r1/outputs", 2048), "other agent routes use the default cap")
	assert.Equal(t, http.StatusRequestEntityTooLarge, read(http.MethodGet, "/api/v1/runs/r1/artifacts/out", 2048), "only PUT is exempt on the artifact path")
}

func TestStoreCredentials_RateLimitedPerClientAddress(t *testing.T) {
	s := NewServer(Config{}, nil)
	post := func(remote string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/store-credentials", strings.NewReader(`{}`))
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		return rec.Code
	}
	for i := 0; i < 20; i++ {
		require.Equal(t, http.StatusBadRequest, post("192.0.2.1:1000"), "request %d is within the burst", i)
	}
	assert.Equal(t, http.StatusTooManyRequests, post("192.0.2.1:1001"))
	assert.Equal(t, http.StatusBadRequest, post("192.0.2.2:1000"), "another client address has its own budget")
}
