package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
)

func testJWT(exp time.Time) string {
	enc := base64.RawURLEncoding
	hdr := enc.EncodeToString([]byte(`{"alg":"none"}`))
	pl := enc.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix())))
	return hdr + "." + pl + ".sig"
}

func TestIDTokenExpiry(t *testing.T) {
	exp := time.Unix(1893456000, 0)
	got, err := idTokenExpiry(testJWT(exp))
	require.NoError(t, err)
	assert.True(t, exp.Equal(got))

	_, err = idTokenExpiry("not-a-jwt")
	assert.Error(t, err)
	_, err = idTokenExpiry("a." + base64.RawURLEncoding.EncodeToString([]byte(`{}`)) + ".c")
	assert.Error(t, err)
}

// fakeSSO is a combined controller and IdP for refresh tests.
type fakeSSO struct {
	srv          *httptest.Server
	idpCalls     atomic.Int32
	relayCalls   atomic.Int32
	relayClient  atomic.Value // string
	idpSawSecret atomic.Bool

	mu           sync.Mutex
	validRefresh string // the one refresh token the IdP currently accepts
	rotate       bool
	failCode     string // when set, the IdP/relay answers 400 with this error
	newExp       time.Time
}

func newFakeSSO(t *testing.T) *fakeSSO {
	f := &fakeSSO{validRefresh: "rt-0", rotate: true, newExp: time.Now().Add(time.Hour)}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/oidc-config":
			_, _ = w.Write([]byte(`{"issuer":"` + f.srv.URL + `","clientId":"web","deviceClientId":"cli","tokenEndpoint":"` + f.srv.URL + `/token"}`))
		case "/token":
			f.idpCalls.Add(1)
			_ = r.ParseForm()
			if r.Form.Get("client_secret") != "" {
				f.idpSawSecret.Store(true)
			}
			f.answer(w, r.Form.Get("refresh_token"), func(id, rt string) any {
				return map[string]any{"id_token": id, "refresh_token": rt, "access_token": "at"}
			}, func(code string) any { return map[string]string{"error": code} })
		case "/api/v1/auth/cli/refresh":
			f.relayCalls.Add(1)
			var req api.CLIRefreshRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.relayClient.Store(req.Client)
			f.answer(w, req.RefreshToken, func(id, rt string) any {
				return api.CLITokenResponse{IDToken: id, RefreshToken: rt, Expiry: f.newExp}
			}, func(code string) any { return api.CLIOAuthError{Error: code} })
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSSO) answer(w http.ResponseWriter, presented string, ok func(id, rt string) any, bad func(code string) any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if f.failCode != "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(bad(f.failCode))
		return
	}
	if presented != f.validRefresh {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(bad("invalid_grant"))
		return
	}
	next := presented
	if f.rotate {
		next = "rt-rotated-" + fmt.Sprint(f.idpCalls.Load()+f.relayCalls.Load())
		f.validRefresh = next
	}
	_ = json.NewEncoder(w).Encode(ok(testJWT(f.newExp), next))
}

func staleConfig(t *testing.T, f *fakeSSO, mut func(*Config)) (string, Config) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "unified-cd", "config.yaml")
	c := Config{
		Server:       f.srv.URL,
		Token:        "old-token",
		RefreshToken: "rt-0",
		TokenExpiry:  time.Now().Add(-time.Minute),
		LoginClient:  "device",
	}
	if mut != nil {
		mut(&c)
	}
	require.NoError(t, SaveConfig(path, c))
	return path, c
}

func TestRefresh_NotExpired_NoCalls(t *testing.T) {
	f := newFakeSSO(t)
	path, c := staleConfig(t, f, func(c *Config) { c.TokenExpiry = time.Now().Add(10 * time.Minute) })
	got, err := refreshIfNeeded(context.Background(), path, c, time.Now())
	require.NoError(t, err)
	assert.Equal(t, "old-token", got.Token)
	assert.Zero(t, f.idpCalls.Load()+f.relayCalls.Load())
}

func TestRefresh_NoRefreshToken_Unchanged(t *testing.T) {
	f := newFakeSSO(t)
	path, c := staleConfig(t, f, func(c *Config) { c.RefreshToken = "" })
	got, err := refreshIfNeeded(context.Background(), path, c, time.Now())
	require.NoError(t, err)
	assert.Equal(t, "old-token", got.Token)
	assert.Zero(t, f.idpCalls.Load()+f.relayCalls.Load())
}

func TestRefresh_Direct_SavesNewTokens(t *testing.T) {
	f := newFakeSSO(t)
	path, c := staleConfig(t, f, nil)
	got, err := refreshIfNeeded(context.Background(), path, c, time.Now())
	require.NoError(t, err)
	assert.Equal(t, int32(1), f.idpCalls.Load())
	assert.False(t, f.idpSawSecret.Load(), "direct refresh is a public client: no client_secret")
	assert.NotEqual(t, "old-token", got.Token)

	saved, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, got.Token, saved.Token)
	assert.True(t, strings.HasPrefix(saved.RefreshToken, "rt-rotated-"))
	assert.WithinDuration(t, f.newExp, saved.TokenExpiry, 2*time.Second)
	assert.Equal(t, "device", saved.LoginClient)
}

func TestRefresh_Direct_KeepsOldRefreshTokenWhenNotRotated(t *testing.T) {
	f := newFakeSSO(t)
	f.rotate = false
	path, c := staleConfig(t, f, nil)
	_, err := refreshIfNeeded(context.Background(), path, c, time.Now())
	require.NoError(t, err)
	saved, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, "rt-0", saved.RefreshToken)
}

func TestRefresh_Relay_DeviceAndBrowser(t *testing.T) {
	for name, tc := range map[string]struct {
		mut        func(*Config)
		wantClient string
	}{
		"device relay":   {func(c *Config) { c.LoginRelay = true }, "device"},
		"browser client": {func(c *Config) { c.LoginClient = "browser" }, "browser"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeSSO(t)
			path, c := staleConfig(t, f, tc.mut)
			got, err := refreshIfNeeded(context.Background(), path, c, time.Now())
			require.NoError(t, err)
			assert.Equal(t, int32(1), f.relayCalls.Load())
			assert.Zero(t, f.idpCalls.Load())
			assert.Equal(t, tc.wantClient, f.relayClient.Load())
			assert.NotEqual(t, "old-token", got.Token)
			saved, err := LoadConfig(path)
			require.NoError(t, err)
			assert.Equal(t, got.Token, saved.Token)
			assert.True(t, strings.HasPrefix(saved.RefreshToken, "rt-rotated-"))
		})
	}
}

func TestRefresh_OverridesSkipRefresh(t *testing.T) {
	f := newFakeSSO(t)
	path, c := staleConfig(t, f, nil)
	var stderr bytes.Buffer
	got := applyRefresh(context.Background(), path, c, true, time.Now(), &stderr)
	assert.Equal(t, "old-token", got.Token)
	assert.Zero(t, f.idpCalls.Load()+f.relayCalls.Load())
	assert.Empty(t, stderr.String())
}

func TestRefresh_InvalidGrant_WarnsOnceAndKeepsFile(t *testing.T) {
	f := newFakeSSO(t)
	f.failCode = "invalid_grant"
	path, c := staleConfig(t, f, nil)
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	var stderr bytes.Buffer
	got := applyRefresh(context.Background(), path, c, false, time.Now(), &stderr)
	assert.Equal(t, "old-token", got.Token)
	assert.Equal(t, "token refresh failed: invalid_grant; run \"unified-cli login\" again\n", stderr.String())

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
	_, err = LoadConfig(path)
	require.NoError(t, err)
	_, statErr := os.Stat(path + ".lock")
	assert.True(t, os.IsNotExist(statErr), "lock must be released")
}

func TestRefresh_Concurrent_OneIdPCall(t *testing.T) {
	f := newFakeSSO(t)
	path, c := staleConfig(t, f, nil)

	var wg sync.WaitGroup
	results := make([]Config, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = refreshIfNeeded(context.Background(), path, c, time.Now())
		}()
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	assert.Equal(t, int32(1), f.idpCalls.Load(), "exactly one IdP refresh call")
	assert.NotEqual(t, "old-token", results[0].Token)
	assert.Equal(t, results[0].Token, results[1].Token)
	saved, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, results[0].Token, saved.Token)
}

// TestHelperProcess is not a test: it is the body of the child processes
// started by TestRefresh_TwoProcesses_OneIdPCall. It does nothing unless
// GO_WANT_HELPER_PROCESS is set.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	path := os.Getenv("UCD_HELPER_CONFIG")
	// Wait for the parent's start signal so both children contend for the lock.
	for i := 0; i < 400; i++ {
		if _, err := os.Stat(path + ".go"); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	c, err := LoadConfig(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load:", err)
		os.Exit(2)
	}
	got, err := refreshIfNeeded(context.Background(), path, c, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "refresh:", err)
		os.Exit(3)
	}
	fmt.Println("token=" + got.Token)
	os.Exit(0)
}

func TestRefresh_TwoProcesses_OneIdPCall(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns child processes")
	}
	f := newFakeSSO(t)
	path, _ := staleConfig(t, f, nil)

	type result struct {
		out []byte
		err error
	}
	results := make([]chan result, 2)
	for i := range results {
		results[i] = make(chan result, 1)
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
		cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1", "UCD_HELPER_CONFIG="+path)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		require.NoError(t, cmd.Start())
		go func(ch chan result) {
			err := cmd.Wait()
			if err != nil {
				err = fmt.Errorf("%w: %s", err, stderr.String())
			}
			ch <- result{stdout.Bytes(), err}
		}(results[i])
	}
	require.NoError(t, os.WriteFile(path+".go", nil, 0o600))

	var tokens []string
	for _, ch := range results {
		r := <-ch
		require.NoError(t, r.err)
		var tok string
		for _, line := range strings.Split(string(r.out), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "token="); ok {
				tok = v
			}
		}
		require.NotEmpty(t, tok, "child output: %s", r.out)
		tokens = append(tokens, tok)
	}
	assert.Equal(t, int32(1), f.idpCalls.Load(), "exactly one IdP refresh call across both processes")
	saved, err := LoadConfig(path)
	require.NoError(t, err)
	assert.NotEqual(t, "old-token", saved.Token)
	assert.Equal(t, saved.Token, tokens[0])
	assert.Equal(t, saved.Token, tokens[1])
}

func TestSaveConfig_RetriesRenameOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("rename over an open file only fails on Windows")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, SaveConfig(path, Config{Server: "https://x", Token: "t1"}))

	// A reader holds the file open without FILE_SHARE_DELETE, so renaming over
	// it fails until the reader closes.
	r, err := os.Open(path)
	require.NoError(t, err)
	released := time.AfterFunc(400*time.Millisecond, func() { _ = r.Close() })
	defer released.Stop()

	start := time.Now()
	require.NoError(t, SaveConfig(path, Config{Server: "https://x", Token: "t2"}))
	assert.GreaterOrEqual(t, time.Since(start), 300*time.Millisecond, "the first attempts must have failed and been retried")
	c, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, "t2", c.Token)
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.HasSuffix(e.Name(), ".tmp"), "leftover %s", e.Name())
	}
}

func TestSaveConfig_ModeAndNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "config.yaml")
	require.NoError(t, SaveConfig(path, Config{Server: "https://x", Token: "t"}))
	require.NoError(t, SaveConfig(path, Config{Server: "https://x", Token: "t2"}))
	if runtime.GOOS != "windows" {
		st, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.HasSuffix(e.Name(), ".tmp"), "leftover %s", e.Name())
	}
	c, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, "t2", c.Token)
}

// --- login ---

func setHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

func runLogin(t *testing.T, srvURL string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newLoginCmd()
	cmd.SetArgs([]string{"--server", srvURL})
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	err = cmd.ExecuteContext(context.Background())
	return out.String(), errb.String(), err
}

func TestLogin_Relay_StoresRefreshToken(t *testing.T) {
	setHome(t)
	exp := time.Now().Add(time.Hour).Truncate(time.Second)
	var polls atomic.Int32
	var direct atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/oidc-config":
			_, _ = w.Write([]byte(`{"issuer":"` + srv.URL + `","deviceClientId":"cli","deviceRelay":true,"deviceAuthEndpoint":"` + srv.URL + `/device","tokenEndpoint":"` + srv.URL + `/token"}`))
		case "/api/v1/auth/cli/device/start":
			assert.Equal(t, http.MethodPost, r.Method)
			_ = json.NewEncoder(w).Encode(api.CLIDeviceStartResponse{DeviceCode: "dc", UserCode: "UC", VerificationURI: srv.URL + "/v", VerificationURIComplete: srv.URL + "/v?uc=UC", ExpiresIn: 60, Interval: 1})
		case "/api/v1/auth/cli/device/token":
			var req api.CLIDeviceTokenRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			assert.Equal(t, "dc", req.DeviceCode)
			if polls.Add(1) == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(api.CLIOAuthError{Error: "authorization_pending"})
				return
			}
			_ = json.NewEncoder(w).Encode(api.CLITokenResponse{IDToken: testJWT(exp), RefreshToken: "rt-login", Expiry: exp})
		case "/device", "/token":
			direct.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	_, stderr, err := runLogin(t, srv.URL)
	require.NoError(t, err)
	assert.Zero(t, direct.Load(), "relay login must not talk to the IdP directly")
	assert.NotContains(t, stderr, "no refresh token")

	saved, err := LoadConfig(DefaultConfigPath())
	require.NoError(t, err)
	assert.Equal(t, testJWT(exp), saved.Token)
	assert.Equal(t, "rt-login", saved.RefreshToken)
	assert.True(t, saved.LoginRelay)
	assert.Equal(t, "device", saved.LoginClient)
	assert.WithinDuration(t, exp, saved.TokenExpiry, time.Second)
	assert.Equal(t, srv.URL, saved.Server)
}

func TestLogin_OldController_UsesDirectDeviceFlow(t *testing.T) {
	setHome(t)
	exp := time.Now().Add(time.Hour).Truncate(time.Second)
	var relay atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/oidc-config":
			_, _ = w.Write([]byte(`{"issuer":"` + srv.URL + `","deviceClientId":"cli","deviceAuthEndpoint":"` + srv.URL + `/device","tokenEndpoint":"` + srv.URL + `/token"}`))
		case "/device":
			_, _ = w.Write([]byte(`{"device_code":"dc","user_code":"UC","verification_uri":"` + srv.URL + `/v","verification_uri_complete":"` + srv.URL + `/v?uc=UC","expires_in":60,"interval":1}`))
		case "/token":
			_, _ = w.Write([]byte(`{"access_token":"at","token_type":"Bearer","refresh_token":"rt-direct","id_token":"` + testJWT(exp) + `","expires_in":300}`))
		default:
			if strings.HasPrefix(r.URL.Path, "/api/v1/auth/cli/") {
				relay.Add(1)
			}
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	_, _, err := runLogin(t, srv.URL)
	require.NoError(t, err)
	assert.Zero(t, relay.Load())
	saved, err := LoadConfig(DefaultConfigPath())
	require.NoError(t, err)
	assert.Equal(t, testJWT(exp), saved.Token)
	assert.Equal(t, "rt-direct", saved.RefreshToken)
	assert.False(t, saved.LoginRelay)
	assert.Equal(t, "device", saved.LoginClient)
	// Expiry is the id_token exp (1h), not the access token's expires_in (5m).
	assert.WithinDuration(t, exp, saved.TokenExpiry, time.Second)
}

func TestLogin_NoRefreshToken_Warns(t *testing.T) {
	setHome(t)
	exp := time.Now().Add(time.Hour).Truncate(time.Second)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/oidc-config":
			_, _ = w.Write([]byte(`{"issuer":"` + srv.URL + `","deviceClientId":"cli","deviceRelay":true}`))
		case "/api/v1/auth/cli/device/start":
			_ = json.NewEncoder(w).Encode(api.CLIDeviceStartResponse{DeviceCode: "dc", UserCode: "UC", VerificationURI: srv.URL + "/v", ExpiresIn: 60, Interval: 1})
		case "/api/v1/auth/cli/device/token":
			_ = json.NewEncoder(w).Encode(api.CLITokenResponse{IDToken: testJWT(exp), Expiry: exp})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	_, stderr, err := runLogin(t, srv.URL)
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(stderr, "the identity provider issued no refresh token; you will need to log in again when the token expires"), stderr)
	saved, err := LoadConfig(DefaultConfigPath())
	require.NoError(t, err)
	assert.Empty(t, saved.RefreshToken)
}

func TestRefresh_ServerOverride_NeverSendsStoredToken(t *testing.T) {
	stored := newFakeSSO(t)
	other := newFakeSSO(t)
	path, c := staleConfig(t, stored, nil)

	c.Server = other.srv.URL // --server / UNIFIED_SERVER override
	got, err := refreshIfNeeded(context.Background(), path, c, time.Now())
	require.NoError(t, err)
	assert.Equal(t, "old-token", got.Token)
	for _, f := range []*fakeSSO{stored, other} {
		assert.Zero(t, f.idpCalls.Load()+f.relayCalls.Load())
	}
	saved, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, "old-token", saved.Token)

	// Relay-stored token: same guarantee.
	path2, c2 := staleConfig(t, stored, func(c *Config) { c.LoginRelay = true })
	c2.Server = other.srv.URL
	_, err = refreshIfNeeded(context.Background(), path2, c2, time.Now())
	require.NoError(t, err)
	assert.Zero(t, other.relayCalls.Load()+other.idpCalls.Load())
	assert.Zero(t, stored.relayCalls.Load())
}

func TestRefresh_MatchingServerOverride_Refreshes(t *testing.T) {
	f := newFakeSSO(t)
	path, c := staleConfig(t, f, nil)
	c.Server = f.srv.URL + "/" // same server, spelled differently
	got, err := refreshIfNeeded(context.Background(), path, c, time.Now())
	require.NoError(t, err)
	assert.NotEqual(t, "old-token", got.Token)
	assert.Equal(t, int32(1), f.idpCalls.Load())
}

func TestRefresh_HungServer_BoundedByTimeout(t *testing.T) {
	prev := refreshTimeout
	refreshTimeout = 200 * time.Millisecond
	t.Cleanup(func() { refreshTimeout = prev })

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	path := filepath.Join(t.TempDir(), "config.yaml")
	c := Config{Server: srv.URL, Token: "old-token", RefreshToken: "rt-0", TokenExpiry: time.Now().Add(-time.Minute), LoginClient: "device"}
	require.NoError(t, SaveConfig(path, c))

	var stderr bytes.Buffer
	start := time.Now()
	got := applyRefresh(context.Background(), path, c, false, time.Now(), &stderr)
	assert.Less(t, time.Since(start), 3*time.Second)
	assert.Equal(t, "old-token", got.Token)
	assert.Contains(t, stderr.String(), "token refresh failed:")
	_, statErr := os.Stat(path + ".lock")
	assert.True(t, os.IsNotExist(statErr), "lock must be released")
}
