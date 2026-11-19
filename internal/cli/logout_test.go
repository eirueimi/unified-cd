package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
)

type logoutServer struct {
	srv         *httptest.Server
	calls       atomic.Int32
	revokeCalls atomic.Int32

	mu         sync.Mutex
	relayBody  api.CLIRefreshRequest
	directForm url.Values
	status     int           // status for the revoke call (default per kind)
	hang       time.Duration // delay before answering the revoke call
}

func newLogoutServer(t *testing.T, withRevocation bool) *logoutServer {
	l := &logoutServer{}
	l.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.calls.Add(1)
		switch r.URL.Path {
		case "/api/v1/auth/oidc-config":
			m := map[string]any{"issuer": "https://idp.invalid", "clientId": "web", "deviceClientId": "cli-dev"}
			if withRevocation {
				m["revocationEndpoint"] = l.srv.URL + "/idp/revoke"
			}
			_ = json.NewEncoder(w).Encode(m)
		case "/api/v1/auth/cli/revoke":
			l.revokeCalls.Add(1)
			l.mu.Lock()
			hang := l.hang
			l.mu.Unlock()
			time.Sleep(hang)
			l.mu.Lock()
			_ = json.NewDecoder(r.Body).Decode(&l.relayBody)
			st := l.status
			l.mu.Unlock()
			if st == 0 {
				st = http.StatusNoContent
			}
			w.WriteHeader(st)
		case "/idp/revoke":
			l.revokeCalls.Add(1)
			l.mu.Lock()
			hang := l.hang
			l.mu.Unlock()
			time.Sleep(hang)
			_ = r.ParseForm()
			l.mu.Lock()
			l.directForm = r.PostForm
			st := l.status
			l.mu.Unlock()
			if st == 0 {
				st = http.StatusOK
			}
			w.WriteHeader(st)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(l.srv.Close)
	return l
}

func runLogout(t *testing.T, path string) (string, string, error) {
	cmd := NewRoot()
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs([]string{"logout", "--config", path})
	err := cmd.Execute()
	return out.String(), errb.String(), err
}

func seedLogin(t *testing.T, server, client string, relay bool) string {
	path := filepath.Join(t.TempDir(), "cfg.yaml")
	require.NoError(t, SaveConfig(path, Config{
		Server: server, Token: "id-token", Headers: []string{"X-H: 1"},
		RefreshToken: "refresh-secret", TokenExpiry: time.Now().Add(time.Hour),
		LoginClient: client, LoginRelay: relay,
	}))
	return path
}

func assertLoggedOut(t *testing.T, path, server string) {
	saved, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, Config{Server: server, Headers: []string{"X-H: 1"}}, saved)
}

func TestLogout_RelayRevokeAndClear(t *testing.T) {
	l := newLogoutServer(t, true)
	path := seedLogin(t, l.srv.URL, "browser", false)
	out, errs, err := runLogout(t, path)
	require.NoError(t, err)
	assert.Contains(t, out, "Logged out.")
	assert.Empty(t, errs)
	assert.Equal(t, api.CLIRefreshRequest{RefreshToken: "refresh-secret", Client: "browser"}, l.relayBody)
	assertLoggedOut(t, path, l.srv.URL)
}

func TestLogout_RelayDeviceClient(t *testing.T) {
	l := newLogoutServer(t, true)
	path := seedLogin(t, l.srv.URL, "device", true)
	_, _, err := runLogout(t, path)
	require.NoError(t, err)
	assert.Equal(t, "device", l.relayBody.Client)
	assertLoggedOut(t, path, l.srv.URL)
}

func TestLogout_DirectDeviceRevokesAtIdP(t *testing.T) {
	l := newLogoutServer(t, true)
	path := seedLogin(t, l.srv.URL, "device", false)
	_, errs, err := runLogout(t, path)
	require.NoError(t, err)
	assert.Empty(t, errs)
	assert.Equal(t, "cli-dev", l.directForm.Get("client_id"))
	assert.Equal(t, "refresh-secret", l.directForm.Get("token"))
	assert.Equal(t, "refresh_token", l.directForm.Get("token_type_hint"))
	assert.Zero(t, l.relayBody.RefreshToken, "direct logins must not use the relay")
	assertLoggedOut(t, path, l.srv.URL)
}

func TestLogout_RevokeFailureWarnsAndStillClears(t *testing.T) {
	for _, kind := range []string{"browser", "device"} {
		l := newLogoutServer(t, true)
		l.status = http.StatusInternalServerError
		path := seedLogin(t, l.srv.URL, kind, false)
		out, errs, err := runLogout(t, path)
		require.NoError(t, err)
		assert.Contains(t, out, "Logged out.")
		assert.Contains(t, errs, "warning: could not revoke the refresh token: ")
		assert.NotContains(t, errs, "refresh-secret")
		assertLoggedOut(t, path, l.srv.URL)
	}
}

func TestLogout_NoRevocationEndpointSkipsRevoke(t *testing.T) {
	l := newLogoutServer(t, false)
	path := seedLogin(t, l.srv.URL, "browser", false)
	_, errs, err := runLogout(t, path)
	require.NoError(t, err)
	assert.Empty(t, errs)
	assert.Zero(t, l.revokeCalls.Load())
	assertLoggedOut(t, path, l.srv.URL)
}

func TestLogout_PATOnlyMakesNoNetworkCall(t *testing.T) {
	l := newLogoutServer(t, true)
	path := filepath.Join(t.TempDir(), "cfg.yaml")
	require.NoError(t, SaveConfig(path, Config{Server: l.srv.URL, Token: "pat", Headers: []string{"X-H: 1"}}))
	out, _, err := runLogout(t, path)
	require.NoError(t, err)
	assert.Contains(t, out, "Logged out.")
	assert.Zero(t, l.calls.Load())
	assertLoggedOut(t, path, l.srv.URL)
}

func TestLogout_RevokeIsBounded(t *testing.T) {
	l := newLogoutServer(t, true)
	l.hang = 2 * time.Second
	old := refreshTimeout
	refreshTimeout = 100 * time.Millisecond
	t.Cleanup(func() { refreshTimeout = old })
	path := seedLogin(t, l.srv.URL, "browser", false)
	start := time.Now()
	_, errs, err := runLogout(t, path)
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 1500*time.Millisecond)
	assert.Contains(t, errs, "warning: could not revoke the refresh token")
	assertLoggedOut(t, path, l.srv.URL)
}

func TestLogout_TakesTheConfigLock(t *testing.T) {
	l := newLogoutServer(t, true)
	path := seedLogin(t, l.srv.URL, "browser", false)
	unlock, err := lockFile(path + ".lock")
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { _, _, err := runLogout(t, path); done <- err }()
	select {
	case <-done:
		t.Fatal("logout finished while the config lock was held")
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("logout did not finish after the lock was released")
	}
	assertLoggedOut(t, path, l.srv.URL)
}
