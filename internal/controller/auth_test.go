package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unified-cd/unified-cd/internal/store"
)

func TestBearerAuth_AcceptsValidToken(t *testing.T) {
	mw := BearerAuth("secret")
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestBearerAuth_RejectsMissingHeader(t *testing.T) {
	mw := BearerAuth("secret")
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestBearerAuth_RejectsEmptyExpectedToken(t *testing.T) {
	mw := BearerAuth("")
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer ")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestBearerAuth_RejectsWrongToken(t *testing.T) {
	mw := BearerAuth("secret")
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestServer_Healthz(t *testing.T) {
	s := NewServer(Config{Token: "t"}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestServer_Healthz_ShuttingDown(t *testing.T) {
	s := NewServer(Config{Token: "t"}, nil)
	s.SetShuttingDown()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestServer_Readyz_OK(t *testing.T) {
	s := NewServer(Config{Token: "t"}, nil)
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestServer_Readyz_ShuttingDown(t *testing.T) {
	s := NewServer(Config{Token: "t"}, nil)
	s.SetShuttingDown()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// failingPingStore is a minimal mock that overrides only Ping.
// Other methods panic via the nil-embedded store — for /readyz tests only.
type failingPingStore struct {
	store.Store
}

func (failingPingStore) Ping(_ context.Context) error {
	return errors.New("db down")
}

func TestServer_Readyz_DBDown(t *testing.T) {
	s := NewServer(Config{Token: "t"}, failingPingStore{})
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "db unavailable")
}

// fakePATStore is a minimal mock that implements only GetPATByHash and TouchPAT.
// Because UNIFIED_TOKEN is synced to the DB as a PAT at startup, ServerAuth has no
// static-token branch, so it can be tested purely through the PAT lookup path.
type fakePATStore struct {
	store.Store
	pats map[string]*store.PAT
}

func (f *fakePATStore) GetPATByHash(_ context.Context, hash string) (*store.PAT, error) {
	if p, ok := f.pats[hash]; ok {
		return p, nil
	}
	return nil, errors.New("not found")
}

func (f *fakePATStore) TouchPAT(_ context.Context, _ string) error { return nil }

func TestServerAuth_AcceptsPAT(t *testing.T) {
	st := &fakePATStore{pats: map[string]*store.PAT{
		HashToken("exc_abc"): {ID: "1", Name: "ci"},
	}}
	handler := ServerAuth(st, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer exc_abc")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestServerAuth_RejectsUnknownToken(t *testing.T) {
	st := &fakePATStore{pats: map[string]*store.PAT{}}
	handler := ServerAuth(st, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestServer_OIDCConfig_NotConfigured(t *testing.T) {
	s := NewServer(Config{Token: "t"}, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc-config", nil)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestServer_OIDCConfig_Configured(t *testing.T) {
	s := NewServer(Config{Token: "t"}, nil)
	s.SetOIDCConfig(&OIDCConfig{Issuer: "https://accounts.google.com", ClientID: "client123"})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc-config", nil)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var result map[string]interface{}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&result))
	assert.Equal(t, "https://accounts.google.com", result["issuer"])
	assert.Equal(t, "client123", result["clientId"])
}

func TestServer_UI_Served(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/index.html", []byte("<html></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewServer(Config{Token: "t", WebDir: dir}, nil)
	req := httptest.NewRequest(http.MethodGet, "/ui/", nil)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/html")
}

func TestServer_Root_Redirects_To_UI(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/index.html", []byte("<html></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewServer(Config{Token: "t", WebDir: dir}, nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusFound, rec.Code)
	assert.Contains(t, rec.Header().Get("Location"), "/ui/")
}

