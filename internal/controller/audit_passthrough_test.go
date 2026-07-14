package controller

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// auditRecorderStore captures InsertAuditLog calls; satisfies the middleware's
// structural interface.
type auditRecorderStore struct {
	actions   []string
	resources []string
}

func (f *auditRecorderStore) InsertAuditLog(ctx context.Context, actor, method, path, action, resource string, status int) error {
	f.actions = append(f.actions, action)
	f.resources = append(f.resources, resource)
	return nil
}

// TestAuditMiddleware_PassesFullBodyDownstream: the 64 KiB audit peek must
// never truncate what the handler sees (a >64 KiB job YAML or secret used to
// be silently cut).
func TestAuditMiddleware_PassesFullBodyDownstream(t *testing.T) {
	big := strings.Repeat("a", 100_000) // well past auditBodyPeekLimit

	var got string
	router := chi.NewRouter()
	router.Use(auditLogMiddleware(&auditRecorderStore{}))
	router.Post("/api/v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		got = string(b)
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader(big))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Len(t, got, len(big), "handler must receive the full body, not the audit peek")
	assert.Equal(t, big, got)
}

// TestAuditMiddleware_SmallBodyNameExtractionUnchanged: audit still extracts
// body-derived resource names for normal-sized envelopes.
func TestAuditMiddleware_SmallBodyNameExtractionUnchanged(t *testing.T) {
	st := &auditRecorderStore{}
	router := chi.NewRouter()
	router.Use(auditLogMiddleware(st))
	router.Post("/api/v1/secrets", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", strings.NewReader(`{"name":"AWS_KEY","value":"x"}`))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, st.resources, 1)
	assert.Equal(t, "AWS_KEY", st.resources[0])
}
