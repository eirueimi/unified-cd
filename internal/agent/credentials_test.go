package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func credentialServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(handler)
}

func tokenResponse(access, refresh string, now time.Time) api.AgentTokenResponse {
	refreshExpiry := now.Add(30 * 24 * time.Hour)
	return api.AgentTokenResponse{AgentID: "vm-agent-01", AccessToken: access, AccessExpiresAt: now.Add(time.Hour), RefreshToken: refresh, RefreshExpiresAt: &refreshExpiry}
}

func TestCredentialManagerEnrollsAndPersistsRefreshOnly(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	enrollmentPath := filepath.Join(dir, "enrollment")
	credentialPath := filepath.Join(dir, "credentials.json")
	require.NoError(t, os.WriteFile(enrollmentPath, []byte("uce_enroll"), 0o600))
	srv := credentialServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/agents/enroll", r.URL.Path)
		assert.Equal(t, "Bearer uce_enroll", r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(tokenResponse("uca_access", "ucr_refresh", now))
	})
	defer srv.Close()

	m := NewCredentialManager(CredentialManagerConfig{Server: srv.URL, AgentID: "vm-agent-01", EnrollmentTokenFile: enrollmentPath, CredentialFile: credentialPath, HTTPClient: srv.Client(), Now: func() time.Time { return now }, Jitter: func() time.Duration { return 0 }})
	token, err := m.Token(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "uca_access", token)
	data, err := os.ReadFile(credentialPath)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "uca_access")
	assert.Contains(t, string(data), "ucr_refresh")
	assert.NotContains(t, string(data), "uce_enroll")
}

func TestCredentialManagerRefreshesOnceForConcurrentCallers(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	credentialPath := filepath.Join(dir, "credentials.json")
	require.NoError(t, writeCredentialFile(credentialPath, persistedCredential{Version: 1, AgentID: "vm-agent-01", RefreshToken: "ucr_old", RefreshExpiresAt: now.Add(time.Hour)}))
	var calls int
	var mu sync.Mutex
	srv := credentialServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		assert.Equal(t, "Bearer ucr_old", r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(tokenResponse("uca_new", "ucr_new", now))
	})
	defer srv.Close()
	m := NewCredentialManager(CredentialManagerConfig{Server: srv.URL, AgentID: "vm-agent-01", CredentialFile: credentialPath, HTTPClient: srv.Client(), Now: func() time.Time { return now }, Jitter: func() time.Duration { return 0 }})
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); token, err := m.Token(context.Background()); if token != "uca_new" { errs <- assert.AnError }; errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs { require.NoError(t, err) }
	assert.Equal(t, 1, calls)
}

func TestCredentialManagerRecoversAfterRestartWithRefresh(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	credentialPath := filepath.Join(dir, "credentials.json")
	require.NoError(t, writeCredentialFile(credentialPath, persistedCredential{Version: 1, AgentID: "vm-agent-01", RefreshToken: "ucr_before_restart", RefreshExpiresAt: now.Add(time.Hour)}))
	srv := credentialServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/agents/token/refresh", r.URL.Path)
		assert.Equal(t, "Bearer ucr_before_restart", r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(tokenResponse("uca_recovered", "ucr_rotated", now))
	})
	defer srv.Close()
	m := NewCredentialManager(CredentialManagerConfig{Server: srv.URL, AgentID: "vm-agent-01", CredentialFile: credentialPath, HTTPClient: srv.Client(), Now: func() time.Time { return now }, Jitter: func() time.Duration { return 0 }})
	token, err := m.Token(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "uca_recovered", token)
}

func TestCredentialManagerDoesNotUseAccessWhenPersistenceFails(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	enrollmentPath := filepath.Join(dir, "enrollment")
	require.NoError(t, os.WriteFile(enrollmentPath, []byte("uce_enroll"), 0o600))
	srv := credentialServer(t, func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(tokenResponse("uca_access", "ucr_refresh", now)) })
	defer srv.Close()
	m := NewCredentialManager(CredentialManagerConfig{Server: srv.URL, AgentID: "vm-agent-01", EnrollmentTokenFile: enrollmentPath, CredentialFile: filepath.Join(dir, "credentials.json"), HTTPClient: srv.Client(), Now: func() time.Time { return now }, Jitter: func() time.Duration { return 0 }})
	m.persist = func(string, persistedCredential) error { return assert.AnError }
	token, err := m.Token(t.Context())
	require.Error(t, err)
	assert.Empty(t, token)
}

func TestCredentialManagerRedactsTokenResponseErrors(t *testing.T) {
	dir := t.TempDir()
	enrollmentPath := filepath.Join(dir, "enrollment")
	require.NoError(t, os.WriteFile(enrollmentPath, []byte("uce_enroll"), 0o600))
	srv := credentialServer(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, `{"accessToken":"uca_leak","refreshToken":"ucr_leak"}`, http.StatusBadRequest) })
	defer srv.Close()
	m := NewCredentialManager(CredentialManagerConfig{Server: srv.URL, AgentID: "vm-agent-01", EnrollmentTokenFile: enrollmentPath, CredentialFile: filepath.Join(dir, "credentials.json"), HTTPClient: srv.Client()})
	_, err := m.Token(t.Context())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "uca_leak")
	assert.NotContains(t, err.Error(), "ucr_leak")
	assert.NotContains(t, err.Error(), "uce_enroll")
}

func TestCredentialFileRejectsLooseUnixPermissions(t *testing.T) {
	if runtime.GOOS == "windows" { t.Skip("Unix permission semantics do not apply") }
	path := filepath.Join(t.TempDir(), "credentials.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"version":1,"agentId":"vm-agent-01","refreshToken":"ucr_secret","refreshExpiresAt":"2030-01-01T00:00:00Z"}`), 0o644))
	_, err := readCredentialFile(path)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "permissions"), err)
}
