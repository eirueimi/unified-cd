package agent

import (
	"context"
	"encoding/json"
	"fmt"
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
		go func() {
			defer wg.Done()
			token, err := m.Token(context.Background())
			if token != "uca_new" {
				errs <- assert.AnError
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
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
	srv := credentialServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(tokenResponse("uca_access", "ucr_refresh", now))
	})
	defer srv.Close()
	m := NewCredentialManager(CredentialManagerConfig{Server: srv.URL, AgentID: "vm-agent-01", EnrollmentTokenFile: enrollmentPath, CredentialFile: filepath.Join(dir, "credentials.json"), HTTPClient: srv.Client(), Now: func() time.Time { return now }, Jitter: func() time.Duration { return 0 }})
	m.persist = func(string, persistedCredential) error { return assert.AnError }
	token, err := m.Token(t.Context())
	require.Error(t, err)
	assert.Empty(t, token)
}

func TestCredentialManagerDoesNotEnrollAfterInvalidCredentialFile(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	credentialPath := filepath.Join(dir, "credentials.json")
	enrollmentPath := filepath.Join(dir, "enrollment")
	require.NoError(t, os.WriteFile(credentialPath, []byte("not-json"), 0o600))
	require.NoError(t, os.WriteFile(enrollmentPath, []byte("enrollment-secret"), 0o600))
	var calls int
	srv := credentialServer(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusInternalServerError) })
	defer srv.Close()

	m := NewCredentialManager(CredentialManagerConfig{Server: srv.URL, AgentID: "vm-agent-01", CredentialFile: credentialPath, EnrollmentTokenFile: enrollmentPath, HTTPClient: srv.Client(), Now: func() time.Time { return now }})
	for range 2 {
		_, err := m.Token(t.Context())
		require.Error(t, err)
	}
	assert.Zero(t, calls)
}

func TestCredentialManagerRestoresInterruptedCredentialReplacement(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	backup := credentialBackupPath(path)
	require.NoError(t, writeCredentialFile(backup, persistedCredential{Version: 1, AgentID: "vm-agent-01", RefreshToken: "previous-refresh", RefreshExpiresAt: now.Add(time.Hour)}))
	srv := credentialServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(tokenResponse("new-access", "new-refresh", now))
	})
	defer srv.Close()
	m := NewCredentialManager(CredentialManagerConfig{Server: srv.URL, AgentID: "vm-agent-01", CredentialFile: path, HTTPClient: srv.Client(), Now: func() time.Time { return now }})
	_, err := m.Token(t.Context())
	require.NoError(t, err)
	assert.FileExists(t, path)
	assert.NoFileExists(t, backup)
}

func TestCredentialManagerDoesNotEnrollWhenInterruptedBackupIsInvalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	require.NoError(t, os.WriteFile(credentialBackupPath(path), []byte("invalid"), 0o600))
	enrollmentPath := filepath.Join(dir, "enrollment")
	require.NoError(t, os.WriteFile(enrollmentPath, []byte("enrollment-secret"), 0o600))
	calls := 0
	srv := credentialServer(t, func(w http.ResponseWriter, r *http.Request) { calls++ })
	defer srv.Close()
	m := NewCredentialManager(CredentialManagerConfig{Server: srv.URL, AgentID: "vm-agent-01", CredentialFile: path, EnrollmentTokenFile: enrollmentPath, HTTPClient: srv.Client()})
	_, err := m.Token(t.Context())
	require.Error(t, err)
	assert.Zero(t, calls)
}

func TestCredentialManagerDoesNotEnrollWhenBackupRestoreFails(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	require.NoError(t, writeCredentialFile(credentialBackupPath(path), persistedCredential{Version: 1, AgentID: "vm-agent-01", RefreshToken: "previous-refresh", RefreshExpiresAt: now.Add(time.Hour)}))
	enrollmentPath := filepath.Join(dir, "enrollment")
	require.NoError(t, os.WriteFile(enrollmentPath, []byte("enrollment-secret"), 0o600))
	original := syncCredentialDirectoryFn
	syncCredentialDirectoryFn = func(string) error { return fmt.Errorf("sync failed") }
	t.Cleanup(func() { syncCredentialDirectoryFn = original })
	calls := 0
	srv := credentialServer(t, func(w http.ResponseWriter, r *http.Request) { calls++ })
	defer srv.Close()
	m := NewCredentialManager(CredentialManagerConfig{Server: srv.URL, AgentID: "vm-agent-01", CredentialFile: path, EnrollmentTokenFile: enrollmentPath, HTTPClient: srv.Client(), Now: func() time.Time { return now }})
	_, err := m.Token(t.Context())
	require.Error(t, err)
	assert.Zero(t, calls)
}

func TestCredentialManagerDoesNotEnrollAfterExpiredCredentialFile(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	credentialPath := filepath.Join(dir, "credentials.json")
	enrollmentPath := filepath.Join(dir, "enrollment")
	require.NoError(t, writeCredentialFile(credentialPath, persistedCredential{Version: 1, AgentID: "vm-agent-01", RefreshToken: "expired-refresh", RefreshExpiresAt: now.Add(-time.Minute)}))
	require.NoError(t, os.WriteFile(enrollmentPath, []byte("enrollment-secret"), 0o600))
	var calls int
	srv := credentialServer(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusInternalServerError) })
	defer srv.Close()

	m := NewCredentialManager(CredentialManagerConfig{Server: srv.URL, AgentID: "vm-agent-01", CredentialFile: credentialPath, EnrollmentTokenFile: enrollmentPath, HTTPClient: srv.Client(), Now: func() time.Time { return now }})
	_, err := m.Token(t.Context())
	require.Error(t, err)
	assert.Zero(t, calls)
}

func TestCredentialManagerDoesNotEnrollAfterInsecureCredentialFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission semantics do not apply")
	}
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	credentialPath := filepath.Join(dir, "credentials.json")
	enrollmentPath := filepath.Join(dir, "enrollment")
	require.NoError(t, os.WriteFile(credentialPath, []byte(`{"version":1,"agentId":"vm-agent-01","refreshToken":"insecure-refresh","refreshExpiresAt":"2030-01-02T00:00:00Z"}`), 0o600))
	require.NoError(t, os.Chmod(credentialPath, 0o644))
	require.NoError(t, os.WriteFile(enrollmentPath, []byte("enrollment-secret"), 0o600))
	var calls int
	srv := credentialServer(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusInternalServerError) })
	defer srv.Close()

	m := NewCredentialManager(CredentialManagerConfig{Server: srv.URL, AgentID: "vm-agent-01", CredentialFile: credentialPath, EnrollmentTokenFile: enrollmentPath, HTTPClient: srv.Client(), Now: func() time.Time { return now }})
	_, err := m.Token(t.Context())
	require.Error(t, err)
	assert.Zero(t, calls)
}

func TestCredentialManagerRestoresOldCredentialAfterDirectorySyncFailure(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	enrollmentPath := filepath.Join(dir, "enrollment")
	credentialPath := filepath.Join(dir, "credentials.json")
	require.NoError(t, writeCredentialFile(credentialPath, persistedCredential{Version: 1, AgentID: "vm-agent-01", RefreshToken: "old-refresh", RefreshExpiresAt: now.Add(time.Hour)}))
	require.NoError(t, os.WriteFile(enrollmentPath, []byte("enrollment-secret"), 0o600))
	srv := credentialServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(tokenResponse("access-token", "replacement-refresh", now))
	})
	defer srv.Close()

	original := syncCredentialDirectoryFn
	calls := 0
	syncCredentialDirectoryFn = func(string) error {
		calls++
		if calls == 1 {
			return fmt.Errorf("directory sync failed")
		}
		return nil
	}
	t.Cleanup(func() { syncCredentialDirectoryFn = original })
	m := NewCredentialManager(CredentialManagerConfig{Server: srv.URL, AgentID: "vm-agent-01", EnrollmentTokenFile: enrollmentPath, CredentialFile: credentialPath, HTTPClient: srv.Client(), Now: func() time.Time { return now }})
	token, err := m.Token(t.Context())
	require.Error(t, err)
	assert.Empty(t, token)
	credential, err := readCredentialFile(credentialPath)
	require.NoError(t, err)
	assert.Equal(t, "old-refresh", credential.RefreshToken)
}

func TestCredentialManagerRedactsTokenResponseErrors(t *testing.T) {
	dir := t.TempDir()
	enrollmentPath := filepath.Join(dir, "enrollment")
	require.NoError(t, os.WriteFile(enrollmentPath, []byte("uce_enroll"), 0o600))
	srv := credentialServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"accessToken":"uca_leak","refreshToken":"ucr_leak"}`, http.StatusBadRequest)
	})
	defer srv.Close()
	m := NewCredentialManager(CredentialManagerConfig{Server: srv.URL, AgentID: "vm-agent-01", EnrollmentTokenFile: enrollmentPath, CredentialFile: filepath.Join(dir, "credentials.json"), HTTPClient: srv.Client()})
	_, err := m.Token(t.Context())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "uca_leak")
	assert.NotContains(t, err.Error(), "ucr_leak")
	assert.NotContains(t, err.Error(), "uce_enroll")
}

func TestSafeResponseBodyRedactsEveryCredentialField(t *testing.T) {
	for _, body := range []string{
		`{"accessToken":"access-secret"}`,
		`{"ACCESS_TOKEN":"access-secret"}`,
		`{"refresh_token":"refresh-secret"}`,
		`{"credential":"credential-secret"}`,
		"Bearer authorization-secret",
	} {
		assert.NotContains(t, safeResponseBody([]byte(body)), "secret")
	}
}

func TestCredentialManagerRetriesOnlyRetryableHTTPResponses(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, status string
		code, calls  int
	}{
		{name: "unauthorized", code: http.StatusUnauthorized, calls: 1},
		{name: "forbidden", code: http.StatusForbidden, calls: 1},
		{name: "unavailable", code: http.StatusServiceUnavailable, calls: credentialRetryAttempts},
		{name: "rate limited", code: http.StatusTooManyRequests, calls: credentialRetryAttempts},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			credentialPath := filepath.Join(dir, "credentials.json")
			require.NoError(t, writeCredentialFile(credentialPath, persistedCredential{Version: 1, AgentID: "vm-agent-01", RefreshToken: "refresh-secret", RefreshExpiresAt: now.Add(time.Hour)}))
			calls := 0
			srv := credentialServer(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(tc.code) })
			defer srv.Close()
			m := NewCredentialManager(CredentialManagerConfig{Server: srv.URL, AgentID: "vm-agent-01", CredentialFile: credentialPath, HTTPClient: srv.Client(), Now: func() time.Time { return now }})
			_, err := m.Token(t.Context())
			require.Error(t, err)
			assert.Equal(t, tc.calls, calls)
		})
	}
}

func TestCredentialManagerHonorsRetryAfterWithBoundedBackoff(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	credentialPath := filepath.Join(dir, "credentials.json")
	require.NoError(t, writeCredentialFile(credentialPath, persistedCredential{Version: 1, AgentID: "vm-agent-01", RefreshToken: "refresh-secret", RefreshExpiresAt: now.Add(time.Hour)}))
	calls := 0
	srv := credentialServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	defer srv.Close()
	m := NewCredentialManager(CredentialManagerConfig{Server: srv.URL, AgentID: "vm-agent-01", CredentialFile: credentialPath, HTTPClient: srv.Client(), Now: func() time.Time { return now }})
	var delays []time.Duration
	m.sleep = func(_ context.Context, delay time.Duration) error { delays = append(delays, delay); return nil }
	_, err := m.Token(t.Context())
	require.Error(t, err)
	assert.Equal(t, credentialRetryAttempts, calls)
	assert.Equal(t, []time.Duration{2 * time.Second, 2 * time.Second}, delays)
}

func TestClientNeverIncludesCredentialResponseBodyInErrors(t *testing.T) {
	for _, body := range []string{`{"access_token":"secret"}`, `{\"refresh_token\":\"secret\"}`, "Bearer secret", `<credential>secret</credential>`} {
		t.Run(body[:1], func(t *testing.T) {
			srv := credentialServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(body))
			})
			defer srv.Close()
			err := NewClient(srv.URL, "static-token").Register(t.Context(), api.AgentRegisterRequest{AgentID: "a"})
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "secret")
		})
	}
}

func TestCredentialFileRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("covered by Windows handle-based credential reads")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	path := filepath.Join(dir, "credentials.json")
	require.NoError(t, writeCredentialFile(target, persistedCredential{Version: 1, AgentID: "vm-agent-01", RefreshToken: "refresh-secret", RefreshExpiresAt: time.Now().Add(time.Hour)}))
	require.NoError(t, os.Symlink(target, path))
	_, err := readCredentialFile(path)
	require.Error(t, err)
}

func TestCredentialManagerDefaultJitterIsBoundedAndNonZero(t *testing.T) {
	m := NewCredentialManager(CredentialManagerConfig{})
	jitter := m.refreshJitter
	assert.Greater(t, jitter, time.Duration(0))
	assert.LessOrEqual(t, jitter, maxCredentialJitter)
}

func TestCredentialFileRejectsLooseUnixPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission semantics do not apply")
	}
	path := filepath.Join(t.TempDir(), "credentials.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"version":1,"agentId":"vm-agent-01","refreshToken":"ucr_secret","refreshExpiresAt":"2030-01-01T00:00:00Z"}`), 0o644))
	_, err := readCredentialFile(path)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "permissions"), err)
}
