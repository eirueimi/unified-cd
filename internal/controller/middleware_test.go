package controller

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withCapturedLog runs fn with slog's default logger temporarily replaced by
// a JSON handler writing to a buffer, and returns the buffer's lines.
func withCapturedLog(t *testing.T, fn func()) []string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	fn()

	out := strings.TrimSpace(buf.String())
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func TestAccessLogMiddleware_LogsRequest(t *testing.T) {
	handler := accessLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
	req.RemoteAddr = "203.0.113.5:1234"
	rec := httptest.NewRecorder()

	lines := withCapturedLog(t, func() {
		handler.ServeHTTP(rec, req)
	})

	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, lines, 1)

	var entry map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &entry))
	assert.Equal(t, "GET", entry["method"])
	assert.Equal(t, "/api/v1/jobs", entry["path"])
	assert.Equal(t, float64(200), entry["status"])
	assert.Equal(t, "203.0.113.5:1234", entry["remoteAddr"])
	_, hasDuration := entry["duration_ms"]
	assert.True(t, hasDuration, "expected duration_ms field, got %v", entry)
}

func TestAccessLogMiddleware_CapturesNon200Status(t *testing.T) {
	handler := accessLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/missing", nil)
	rec := httptest.NewRecorder()

	lines := withCapturedLog(t, func() {
		handler.ServeHTTP(rec, req)
	})

	require.Len(t, lines, 1)
	var entry map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &entry))
	assert.Equal(t, float64(404), entry["status"])
}
