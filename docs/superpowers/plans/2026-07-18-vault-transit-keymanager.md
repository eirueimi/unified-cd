# Vault/OpenBao Transit KeyManager Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement a `secrets.KeyManager` backed by Vault/OpenBao's Transit engine, so the controller never holds the key-encryption key — only a revocable credential.

**Architecture:** A new `internal/secrets/vault` package holds the Transit client, an auth abstraction with two implementations (static token, Kubernetes), a shared token-lifecycle manager, and an LRU DEK cache. `internal/config`'s `KeySource.Resolve()` constructs it for `hashivault://` URIs. The `hashicorp/vault/api` SDK is used because token renewal is the part that is fiddly to hand-roll.

**Tech Stack:** Go 1.26.2, `hashicorp/vault/api`, testify, `httptest` for unit fakes, dockertest + OpenBao for integration.

**Spec:** `docs/superpowers/specs/2026-07-18-vault-transit-keymanager-design.md`

## Global Constraints

- Base branch `feat/vault-transit`, worktree `C:/Users/arimax/unified-cd-project/unified-cd-vault-transit`. Never commit to `main`.
- **`secrets.Decrypt` zeroes the DEK returned by `DecryptKey` unconditionally via `defer` (`internal/secrets/crypto.go:66-72`).** Any cache MUST return a **copy** on hit, or the caller zeroes the cache entry and every subsequent hit returns zeros.
- **Transit ciphertext is already `vault:v1:…`.** `EncryptKey` must NOT add a prefix — that would produce `vault:vault:v1:`. `DecryptKey` checks for the `vault:` prefix and returns `ErrProviderMismatch` otherwise, mirroring `LocalKeyManager` (`internal/secrets/keymanager.go:69-74`).
- **Renewal renews at half the remaining lease.** A token reporting `renewable: false` is never sent to the renew endpoint.
- **The renewal loop takes an injected clock and an injected sleep.** It must not be tested with `time.Sleep`. Follow `internal/k8sagent/credentials.go` (`now func() time.Time`, `jitter func() time.Duration`, `sleep func(context.Context, time.Duration) error`, each defaulted from `nil` in the constructor). This repo has shipped timing-dependent CI failures before (the `fix/ci-test-races` series; PR #53).
- **`go.uber.org/goleak` is a dependency.** A renewal goroutine that outlives a test will be caught, which is why `Resolved` gains a `Close()`.
- Integration tests gate on **`testing.Short()`**, not a build tag. The repo's `integration` build tag is run by no CI job; `testing.Short()` puts the suite in the existing `integration` job with no CI change.
- Logging: package-level `slog`, lowercase `"subsystem: what happened"` messages, lowerCamelCase keys, `"error", err` last. **Security events log at `Error` and omit `err`; ordinary failures log at `Warn` with `err`** — see `logSecretDecryptFailure` (`internal/controller/api_secrets.go:144-162`).
- Run `./scripts/prepare-shim-placeholders.sh` before any whole-tree `go build ./...` / `go test ./...`. Add `-buildvcs=false` if you hit `error obtaining VCS status` (both are worktree/environment quirks).
- Before pushing, run on Linux with `TZ=UTC`.

---

### Task 1: `Resolve(ctx)` and `Resolved.Close()`

A pure refactor with no Vault code, so the API change is reviewed on its own. The Vault manager needs a context to log in with and a place to stop its renewal goroutine; neither exists today.

**Files:**
- Modify: `internal/config/keysource.go`
- Modify: `internal/config/keysource_test.go`
- Modify: `cmd/controller/main.go:247-256`

**Interfaces:**
- Consumes: nothing
- Produces:
  - `func (k KeySource) Resolve(ctx context.Context) (Resolved, error)`
  - `Resolved.Close() error` — stops any background work; a no-op for local and dev keys

- [ ] **Step 1: Write the failing test**

Add to `internal/config/keysource_test.go`:

```go
// Resolve acquires resources for some key sources (a KMS client with a
// background renewal loop), so every Resolved must be closable, and closing a
// source that acquired nothing must be safe.
func TestResolved_CloseIsAlwaysSafe(t *testing.T) {
	got, err := KeySource{KeyFile: writeKeyFile(t, testKeyHex)}.Resolve(context.Background())
	require.NoError(t, err)
	require.NoError(t, got.Close())
	require.NoError(t, got.Close(), "Close must be idempotent")

	dev, err := KeySource{DevMode: true}.Resolve(context.Background())
	require.NoError(t, err)
	require.NoError(t, dev.Close())
}
```

Add `"context"` to that file's imports.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/config/ -run TestResolved_CloseIsAlwaysSafe -v`
Expected: FAIL — `too many arguments in call to KeySource{...}.Resolve`, `got.Close undefined`

- [ ] **Step 3: Change the signature and add Close**

In `internal/config/keysource.go`, add `"context"` to the imports, then:

```go
// Resolved is the outcome of resolving a KeySource.
//
// Warnings are returned rather than logged because this package does no
// logging: slog is not configured until main.go has parsed flags. This
// mirrors the existing matrixMaxEnvWarning pattern in cmd/controller/main.go,
// which collects a warning during flag registration and logs it once the
// logger exists.
type Resolved struct {
	KeyManager secrets.KeyManager
	// Description names the key's origin, for the startup log.
	Description string
	// Warnings are operator-facing messages main.go should emit via slog.Warn.
	Warnings []string

	// closeFn releases whatever the key source acquired. A local or ephemeral
	// key acquires nothing, so it stays nil and Close is a no-op.
	closeFn func() error
}

// Close releases resources held by the key manager — for a KMS-backed source,
// the background token-renewal loop. It is safe to call on a source that
// acquired nothing, and safe to call more than once.
func (r *Resolved) Close() error {
	if r.closeFn == nil {
		return nil
	}
	fn := r.closeFn
	r.closeFn = nil
	return fn()
}
```

Change the method signature to:

```go
func (k KeySource) Resolve(ctx context.Context) (Resolved, error) {
```

The body is otherwise unchanged. `ctx` goes unused until Task 6 wires the KMS branch; Go does not error on unused function parameters, so leave it named `ctx` rather than blanking and renaming it later.

- [ ] **Step 4: Update the existing call sites**

Every existing test in `keysource_test.go` calls `.Resolve()`. Change each to `.Resolve(context.Background())`.

In `cmd/controller/main.go`, the `ctx` from `signal.NotifyContext` is already in scope above this block:

```go
	resolved, err := eff.KeySource.Resolve(ctx)
	if err != nil {
		slog.Error("encryption key", "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := resolved.Close(); err != nil {
			slog.Warn("encryption key: close failed", "error", err)
		}
	}()
	for _, w := range resolved.Warnings {
		slog.Warn(w)
	}
	slog.Info("encryption key loaded", "source", resolved.Description)
	km := resolved.KeyManager
```

Verify `ctx` is declared before line 247; if `signal.NotifyContext` appears later, move the `Resolve` block after it rather than creating a second context.

- [ ] **Step 5: Verify and commit**

Run: `go build ./... && go test ./internal/config/ -count=1`
Expected: PASS

```bash
git add internal/config/ cmd/controller/main.go
git commit -m "refactor(config): give Resolve a context and Resolved a Close"
```

---

### Task 2: `vaultAuth` abstraction and static token

**Files:**
- Create: `internal/secrets/vault/auth.go`
- Test: `internal/secrets/vault/auth_test.go`

**Interfaces:**
- Produces:
  - `type authResult struct { Token string; TTL time.Duration; Renewable bool }`
  - `type vaultAuth interface { login(ctx context.Context) (authResult, error) }`
  - `func newStaticTokenAuth(token, tokenFile string) (vaultAuth, error)`

- [ ] **Step 1: Write the failing test**

Create `internal/secrets/vault/auth_test.go`:

```go
package vault

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeTokenFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestStaticTokenAuth_ReadsFile(t *testing.T) {
	a, err := newStaticTokenAuth("", writeTokenFile(t, "s.abc123"))
	require.NoError(t, err)
	got, err := a.login(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "s.abc123", got.Token)
}

// Editors and `echo` append newlines; a trailing newline must not break startup.
func TestStaticTokenAuth_TrimsWhitespace(t *testing.T) {
	a, err := newStaticTokenAuth("", writeTokenFile(t, "  s.abc123\n"))
	require.NoError(t, err)
	got, err := a.login(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "s.abc123", got.Token)
}

// The file is re-read on every login, so an operator can replace a rotated
// token without restarting the controller.
func TestStaticTokenAuth_RereadsFileOnEachLogin(t *testing.T) {
	path := writeTokenFile(t, "s.first")
	a, err := newStaticTokenAuth("", path)
	require.NoError(t, err)

	first, err := a.login(context.Background())
	require.NoError(t, err)
	require.Equal(t, "s.first", first.Token)

	require.NoError(t, os.WriteFile(path, []byte("s.second"), 0o600))
	second, err := a.login(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "s.second", second.Token, "a replaced token file must take effect without a restart")
}

func TestStaticTokenAuth_FilePreferredOverLiteral(t *testing.T) {
	a, err := newStaticTokenAuth("s.from-env", writeTokenFile(t, "s.from-file"))
	require.NoError(t, err)
	got, err := a.login(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "s.from-file", got.Token,
		"a file is preferred: it does not leak into docker inspect or child processes")
}

func TestStaticTokenAuth_LiteralUsedWhenNoFile(t *testing.T) {
	a, err := newStaticTokenAuth("s.from-env", "")
	require.NoError(t, err)
	got, err := a.login(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "s.from-env", got.Token)
}

func TestStaticTokenAuth_NeitherIsAnError(t *testing.T) {
	_, err := newStaticTokenAuth("", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "UNIFIED_VAULT_TOKEN_FILE")
	assert.Contains(t, err.Error(), "VAULT_TOKEN")
}

func TestStaticTokenAuth_MissingFileReportsPath(t *testing.T) {
	a, err := newStaticTokenAuth("", filepath.Join(t.TempDir(), "absent"))
	require.NoError(t, err, "a missing file is a login-time failure, not a construction failure")
	_, err = a.login(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absent")
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/secrets/vault/ -v`
Expected: FAIL — the package does not exist

- [ ] **Step 3: Write `internal/secrets/vault/auth.go`**

```go
// Package vault implements a secrets.KeyManager backed by the Transit secrets
// engine of HashiCorp Vault or OpenBao. The key-encryption key never leaves the
// KMS; this controller holds only a revocable credential.
package vault

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// authResult is what every authentication method produces: a usable token and
// what Vault says about its lifetime.
type authResult struct {
	Token     string
	TTL       time.Duration
	Renewable bool
}

// vaultAuth obtains a fresh token. Implementations differ only in how they
// authenticate; the token lifecycle around them is shared, which is what makes
// adding a method (AppRole, cert) one function rather than a second lifecycle.
type vaultAuth interface {
	login(ctx context.Context) (authResult, error)
}

// staticTokenAuth supplies an operator-provided token.
//
// The file is re-read on every login rather than cached, so replacing a rotated
// token on disk takes effect without restarting the controller. That matters
// because a token that is not periodic dies at its max TTL no matter how often
// it is renewed, and must genuinely be replaced.
type staticTokenAuth struct {
	literal string
	file    string
}

func newStaticTokenAuth(literal, file string) (vaultAuth, error) {
	if literal == "" && file == "" {
		return nil, fmt.Errorf("token auth requires a token: set UNIFIED_VAULT_TOKEN_FILE (preferred) or VAULT_TOKEN")
	}
	return &staticTokenAuth{literal: literal, file: file}, nil
}

func (a *staticTokenAuth) login(_ context.Context) (authResult, error) {
	// A file is preferred over the literal: environment values leak into
	// docker inspect, process listings, crash dumps, and child processes, and
	// the controller spawns git.
	if a.file != "" {
		raw, err := os.ReadFile(a.file)
		if err != nil {
			return authResult{}, fmt.Errorf("read vault token file %s: %w", a.file, err)
		}
		token := strings.TrimSpace(string(raw))
		if token == "" {
			return authResult{}, fmt.Errorf("vault token file %s is empty", a.file)
		}
		// TTL and renewability are unknown until the token is used; the
		// lifecycle manager looks them up with a self-lookup.
		return authResult{Token: token}, nil
	}
	return authResult{Token: a.literal}, nil
}
```

- [ ] **Step 4: Verify and commit**

Run: `go test ./internal/secrets/vault/ -v`
Expected: PASS (7 tests)

```bash
git add internal/secrets/vault/
git commit -m "feat(vault): add auth abstraction and static token source"
```

---

### Task 3: Token lifecycle with an injected clock

The highest-risk task: a background goroutine with time-based behaviour. It is isolated so it can be reviewed and tested on its own.

**Files:**
- Create: `internal/secrets/vault/lifecycle.go`
- Test: `internal/secrets/vault/lifecycle_test.go`

**Interfaces:**
- Consumes: `vaultAuth`, `authResult` from Task 2
- Produces:
  - `type tokenManager struct` with `token(ctx) (string, error)` and `stop()`
  - `func newTokenManager(cfg tokenManagerConfig) (*tokenManager, error)`
  - `type tokenManagerConfig struct { Auth vaultAuth; Renew func(ctx context.Context, token string) (time.Duration, error); Now func() time.Time; Sleep func(context.Context, time.Duration) error }`

- [ ] **Step 1: Write the failing test**

Create `internal/secrets/vault/lifecycle_test.go`:

```go
package vault

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAuth records logins and returns a scripted result.
type fakeAuth struct {
	mu      sync.Mutex
	logins  int
	result  authResult
	err     error
}

func (f *fakeAuth) login(context.Context) (authResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logins++
	return f.result, f.err
}

func (f *fakeAuth) loginCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logins
}

// testClock advances only when a test tells it to, so nothing waits on wall time.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock { return &testClock{t: time.Unix(1_700_000_000, 0)} }

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// recordingSleep captures requested delays and returns immediately, so the
// renewal loop runs at full speed under test.
type recordingSleep struct {
	mu     sync.Mutex
	delays []time.Duration
	gate   chan struct{}
}

func newRecordingSleep() *recordingSleep {
	return &recordingSleep{gate: make(chan struct{}, 1024)}
}

func (r *recordingSleep) sleep(ctx context.Context, d time.Duration) error {
	r.mu.Lock()
	r.delays = append(r.delays, d)
	r.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case r.gate <- struct{}{}:
		return nil
	}
}

func (r *recordingSleep) recorded() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration(nil), r.delays...)
}

func TestTokenManager_ReturnsTokenFromLogin(t *testing.T) {
	auth := &fakeAuth{result: authResult{Token: "s.tok", TTL: time.Hour, Renewable: true}}
	clock := newTestClock()
	m, err := newTokenManager(tokenManagerConfig{
		Auth:  auth,
		Renew: func(context.Context, string) (time.Duration, error) { return time.Hour, nil },
		Now:   clock.now,
		Sleep: newRecordingSleep().sleep,
	})
	require.NoError(t, err)
	t.Cleanup(m.stop)

	got, err := m.token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "s.tok", got)
	assert.Equal(t, 1, auth.loginCount())
}

// The renewal point is half the remaining lease, following Concourse.
func TestTokenManager_SleepsHalfTheLease(t *testing.T) {
	auth := &fakeAuth{result: authResult{Token: "s.tok", TTL: time.Hour, Renewable: true}}
	sleeper := newRecordingSleep()
	m, err := newTokenManager(tokenManagerConfig{
		Auth:  auth,
		Renew: func(context.Context, string) (time.Duration, error) { return time.Hour, nil },
		Now:   newTestClock().now,
		Sleep: sleeper.sleep,
	})
	require.NoError(t, err)
	t.Cleanup(m.stop)

	require.Eventually(t, func() bool { return len(sleeper.recorded()) > 0 },
		2*time.Second, 10*time.Millisecond)
	assert.Equal(t, 30*time.Minute, sleeper.recorded()[0])
}

// A batch token, or any token Vault will not renew, must never be sent to the
// renew endpoint — renewing it fails and would mask the real need to re-login.
func TestTokenManager_NeverRenewsNonRenewableToken(t *testing.T) {
	auth := &fakeAuth{result: authResult{Token: "b.tok", TTL: time.Hour, Renewable: false}}
	var renewCalls atomic.Int64
	sleeper := newRecordingSleep()
	m, err := newTokenManager(tokenManagerConfig{
		Auth: auth,
		Renew: func(context.Context, string) (time.Duration, error) {
			renewCalls.Add(1)
			return 0, nil
		},
		Now:   newTestClock().now,
		Sleep: sleeper.sleep,
	})
	require.NoError(t, err)
	t.Cleanup(m.stop)

	require.Eventually(t, func() bool { return auth.loginCount() >= 2 },
		2*time.Second, 10*time.Millisecond)
	assert.Zero(t, renewCalls.Load(), "a non-renewable token must be re-obtained by logging in again")
}

// A failed renewal falls back to a fresh login rather than giving up.
func TestTokenManager_ReLoginsWhenRenewalFails(t *testing.T) {
	auth := &fakeAuth{result: authResult{Token: "s.tok", TTL: time.Hour, Renewable: true}}
	sleeper := newRecordingSleep()
	m, err := newTokenManager(tokenManagerConfig{
		Auth:  auth,
		Renew: func(context.Context, string) (time.Duration, error) { return 0, errors.New("permission denied") },
		Now:   newTestClock().now,
		Sleep: sleeper.sleep,
	})
	require.NoError(t, err)
	t.Cleanup(m.stop)

	require.Eventually(t, func() bool { return auth.loginCount() >= 2 },
		2*time.Second, 10*time.Millisecond)
}

func TestTokenManager_LoginFailureIsReturnedFromConstructor(t *testing.T) {
	auth := &fakeAuth{err: errors.New("connection refused")}
	_, err := newTokenManager(tokenManagerConfig{
		Auth:  auth,
		Renew: func(context.Context, string) (time.Duration, error) { return 0, nil },
		Now:   newTestClock().now,
		Sleep: newRecordingSleep().sleep,
	})
	require.Error(t, err, "startup must fail fast when the first login fails")
	assert.Contains(t, err.Error(), "connection refused")
}

// stop must be safe to call twice — Resolved.Close may run from a defer and an
// explicit path.
func TestTokenManager_StopIsIdempotent(t *testing.T) {
	auth := &fakeAuth{result: authResult{Token: "s.tok", TTL: time.Hour, Renewable: true}}
	m, err := newTokenManager(tokenManagerConfig{
		Auth:  auth,
		Renew: func(context.Context, string) (time.Duration, error) { return time.Hour, nil },
		Now:   newTestClock().now,
		Sleep: newRecordingSleep().sleep,
	})
	require.NoError(t, err)
	m.stop()
	m.stop()
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/secrets/vault/ -run TestTokenManager -v`
Expected: FAIL — `undefined: newTokenManager`

- [ ] **Step 3: Write `internal/secrets/vault/lifecycle.go`**

```go
package vault

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// tokenManagerConfig supplies the token lifecycle's collaborators. Now and
// Sleep are injected so the renewal loop can be tested without wall-clock
// waiting, following the pattern in internal/k8sagent/credentials.go.
type tokenManagerConfig struct {
	Auth  vaultAuth
	Renew func(ctx context.Context, token string) (time.Duration, error)
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

// tokenManager keeps a Vault token alive and hands it to callers.
//
// One loop serves every authentication method, because keeping a TTL-bearing
// token alive is the same work regardless of how it was obtained. Methods
// differ only in vaultAuth.login.
type tokenManager struct {
	cfg tokenManagerConfig

	mu      sync.RWMutex
	current string

	stopOnce sync.Once
	cancel   context.CancelFunc
	done     chan struct{}
}

func newTokenManager(cfg tokenManagerConfig) (*tokenManager, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleepCtx
	}

	// Startup fails fast: at this point a transient outage and a
	// misconfiguration are indistinguishable, and a controller holding a
	// broken key manager fails every secret operation anyway.
	first, err := cfg.Auth.login(context.Background())
	if err != nil {
		return nil, fmt.Errorf("vault login: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	m := &tokenManager{
		cfg:     cfg,
		current: first.Token,
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	go m.run(ctx, first)
	return m, nil
}

// token returns the current token.
func (m *tokenManager) token(_ context.Context) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.current == "" {
		return "", fmt.Errorf("vault: no valid token")
	}
	return m.current, nil
}

func (m *tokenManager) stop() {
	m.stopOnce.Do(func() {
		m.cancel()
		<-m.done
	})
}

func (m *tokenManager) run(ctx context.Context, current authResult) {
	defer close(m.done)
	for {
		// Renew at half the remaining lease, following Concourse. Halving
		// leaves a full half-lease of headroom to retry in if a renewal fails.
		wait := current.TTL / 2
		if wait <= 0 {
			wait = time.Minute
		}
		if err := m.cfg.Sleep(ctx, wait); err != nil {
			return // context cancelled
		}

		next, err := m.refresh(ctx, current)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// Leave the existing token in place: it may still work, and the
			// next iteration retries. This is the "runtime is patient" half of
			// the design's asymmetry.
			slog.Warn("vault: token refresh failed, will retry", "error", err)
			current = authResult{TTL: wait}
			continue
		}
		current = next

		m.mu.Lock()
		m.current = current.Token
		m.mu.Unlock()
	}
}

// refresh extends the current token, or obtains a new one when it cannot be
// extended.
func (m *tokenManager) refresh(ctx context.Context, current authResult) (authResult, error) {
	if current.Renewable && current.Token != "" {
		ttl, err := m.cfg.Renew(ctx, current.Token)
		if err == nil {
			return authResult{Token: current.Token, TTL: ttl, Renewable: true}, nil
		}
		slog.Warn("vault: renewal failed, logging in again", "error", err)
	}
	// Either the token is not renewable (a batch token, or one Vault declines
	// to renew), or renewal failed. Both are answered by a fresh login.
	return m.cfg.Auth.login(ctx)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
```

- [ ] **Step 4: Verify and commit**

Run: `go test ./internal/secrets/vault/ -race -count=1 -v`
Expected: PASS, no race warnings

```bash
git add internal/secrets/vault/
git commit -m "feat(vault): add token lifecycle with injected clock and half-lease renewal"
```

---

### Task 4: `VaultKeyManager` over Transit

**Files:**
- Create: `internal/secrets/vault/keymanager.go`
- Test: `internal/secrets/vault/keymanager_test.go`
- Modify: `go.mod`, `go.sum` (adds `github.com/hashicorp/vault/api`)

**Interfaces:**
- Consumes: `tokenManager` from Task 3
- Produces:
  - `type Config struct { Address, Mount, Key string; Auth string; AuthParams map[string]string; Token, TokenFile string; Now func() time.Time; Sleep func(context.Context, time.Duration) error }`
  - `func New(ctx context.Context, cfg Config) (*KeyManager, error)` — satisfies `secrets.KeyManager`
  - `func (m *KeyManager) Close() error`

- [ ] **Step 1: Add the dependency**

```bash
go get github.com/hashicorp/vault/api
go mod tidy
```

Report the resulting direct and indirect additions in your report — the dependency's footprint was reviewed and approved, but the exact set should be recorded.

- [ ] **Step 2: Write the failing test**

Create `internal/secrets/vault/keymanager_test.go`:

```go
package vault

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/secrets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeVault is a minimal Transit-speaking server: it base64s on encrypt and
// reverses that on decrypt, which is enough to verify the wire contract.
func fakeVault(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/token/lookup-self", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": map[string]any{"ttl": 3600, "renewable": true}})
	})
	mux.HandleFunc("/v1/auth/token/renew-self", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"auth": map[string]any{"lease_duration": 3600, "renewable": true}})
	})
	mux.HandleFunc("/v1/transit/encrypt/", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Plaintext string `json:"plaintext"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		writeJSON(w, map[string]any{"data": map[string]any{"ciphertext": "vault:v1:" + req.Plaintext}})
	})
	mux.HandleFunc("/v1/transit/decrypt/", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Ciphertext string `json:"ciphertext"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		writeJSON(w, map[string]any{"data": map[string]any{
			"plaintext": strings.TrimPrefix(req.Ciphertext, "vault:v1:"),
		}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func newTestKeyManager(t *testing.T, addr string) *KeyManager {
	t.Helper()
	m, err := New(context.Background(), Config{
		Address: addr, Mount: "transit", Key: "unified-cd-kek",
		Auth: "token", Token: "s.test",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestKeyManager_RoundTrip(t *testing.T) {
	m := newTestKeyManager(t, fakeVault(t).URL)
	dek := []byte("0123456789abcdef0123456789abcdef")

	wrapped, err := m.EncryptKey(context.Background(), dek)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(wrapped), "vault:"),
		"Transit ciphertext is already self-describing")

	got, err := m.DecryptKey(context.Background(), wrapped)
	require.NoError(t, err)
	assert.Equal(t, dek, got)
}

// Transit ciphertext already begins with "vault:". Adding a provider prefix of
// our own would produce "vault:vault:v1:".
func TestKeyManager_DoesNotDoublePrefix(t *testing.T) {
	m := newTestKeyManager(t, fakeVault(t).URL)
	wrapped, err := m.EncryptKey(context.Background(), []byte("dek"))
	require.NoError(t, err)
	assert.False(t, strings.HasPrefix(string(wrapped), "vault:vault:"))
}

// The mirror of LocalKeyManager.DecryptKey: data wrapped by the other provider
// must report precisely, not as an opaque decrypt failure.
func TestKeyManager_RejectsForeignProvider(t *testing.T) {
	m := newTestKeyManager(t, fakeVault(t).URL)
	_, err := m.DecryptKey(context.Background(), []byte("local:deadbeef"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, secrets.ErrProviderMismatch), "got %v", err)
	assert.Contains(t, err.Error(), "hashivault")
}

func TestKeyManager_UnreachableAddressFailsFast(t *testing.T) {
	_, err := New(context.Background(), Config{
		Address: "http://127.0.0.1:1", Mount: "transit", Key: "k",
		Auth: "token", Token: "s.test",
	})
	require.Error(t, err)
}

// Transit works on base64; a DEK is arbitrary bytes and must survive intact.
func TestKeyManager_HandlesArbitraryBytes(t *testing.T) {
	m := newTestKeyManager(t, fakeVault(t).URL)
	dek := []byte{0x00, 0xff, 0x10, 0x00, 0x7f}

	wrapped, err := m.EncryptKey(context.Background(), dek)
	require.NoError(t, err)
	got, err := m.DecryptKey(context.Background(), wrapped)
	require.NoError(t, err)
	assert.Equal(t, dek, got)
	_ = base64.StdEncoding
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/secrets/vault/ -run TestKeyManager -v`
Expected: FAIL — `undefined: New`, `undefined: KeyManager`

- [ ] **Step 4: Write `internal/secrets/vault/keymanager.go`**

```go
package vault

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	vaultapi "github.com/hashicorp/vault/api"

	"github.com/eirueimi/unified-cd/internal/secrets"
)

// vaultCiphertextPrefix is what Transit ciphertext always begins with. Unlike
// LocalKeyManager, this provider does not add a tag on encrypt — Transit's
// output is already self-describing, and prefixing it would yield
// "vault:vault:v1:".
const vaultCiphertextPrefix = "vault:"

// Config describes a Transit-backed key manager. Now and Sleep are optional and
// exist for tests.
type Config struct {
	Address    string
	Mount      string
	Key        string
	Auth       string
	AuthParams map[string]string
	Token      string
	TokenFile  string

	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

// KeyManager wraps and unwraps DEKs using Vault's Transit engine, so the
// key-encryption key never leaves the KMS.
type KeyManager struct {
	client *vaultapi.Client
	tokens *tokenManager
	mount  string
	key    string
}

// New constructs a Transit key manager, authenticating immediately so a
// misconfiguration surfaces at startup rather than at the first secret read.
func New(ctx context.Context, cfg Config) (*KeyManager, error) {
	apiCfg := vaultapi.DefaultConfig()
	apiCfg.Address = cfg.Address
	client, err := vaultapi.NewClient(apiCfg)
	if err != nil {
		return nil, fmt.Errorf("vault client: %w", err)
	}

	auth, err := newAuth(cfg, client)
	if err != nil {
		return nil, err
	}

	tokens, err := newTokenManager(tokenManagerConfig{
		Auth: auth,
		Renew: func(ctx context.Context, token string) (time.Duration, error) {
			c, err := client.Clone()
			if err != nil {
				return 0, err
			}
			c.SetToken(token)
			secret, err := c.Auth().Token().RenewSelfWithContext(ctx, 0)
			if err != nil {
				return 0, err
			}
			return time.Duration(secret.Auth.LeaseDuration) * time.Second, nil
		},
		Now:   cfg.Now,
		Sleep: cfg.Sleep,
	})
	if err != nil {
		return nil, err
	}

	mount := cfg.Mount
	if mount == "" {
		mount = "transit"
	}
	return &KeyManager{client: client, tokens: tokens, mount: mount, key: cfg.Key}, nil
}

// Close stops the background token renewal loop.
func (m *KeyManager) Close() error {
	m.tokens.stop()
	return nil
}

// EncryptKey wraps a DEK with the Transit key.
func (m *KeyManager) EncryptKey(ctx context.Context, plaintext []byte) ([]byte, error) {
	out, err := m.write(ctx, "encrypt", map[string]any{
		"plaintext": base64.StdEncoding.EncodeToString(plaintext),
	})
	if err != nil {
		return nil, err
	}
	ct, ok := out["ciphertext"].(string)
	if !ok {
		return nil, fmt.Errorf("vault transit: encrypt response had no ciphertext")
	}
	return []byte(ct), nil
}

// DecryptKey unwraps a DEK.
func (m *KeyManager) DecryptKey(ctx context.Context, ciphertext []byte) ([]byte, error) {
	// Mirror of LocalKeyManager: report a provider mismatch precisely instead
	// of letting it surface as an opaque decrypt failure.
	if len(ciphertext) < len(vaultCiphertextPrefix) ||
		string(ciphertext[:len(vaultCiphertextPrefix)]) != vaultCiphertextPrefix {
		return nil, fmt.Errorf("%w: this controller is configured for the hashivault key provider", secrets.ErrProviderMismatch)
	}
	out, err := m.write(ctx, "decrypt", map[string]any{"ciphertext": string(ciphertext)})
	if err != nil {
		return nil, err
	}
	b64, ok := out["plaintext"].(string)
	if !ok {
		return nil, fmt.Errorf("vault transit: decrypt response had no plaintext")
	}
	dek, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("vault transit: decode plaintext: %w", err)
	}
	return dek, nil
}

func (m *KeyManager) write(ctx context.Context, op string, body map[string]any) (map[string]any, error) {
	token, err := m.tokens.token(ctx)
	if err != nil {
		return nil, err
	}
	c, err := m.client.Clone()
	if err != nil {
		return nil, err
	}
	c.SetToken(token)
	path := fmt.Sprintf("%s/%s/%s", m.mount, op, m.key)
	secret, err := c.Logical().WriteWithContext(ctx, path, body)
	if err != nil {
		return nil, fmt.Errorf("vault transit %s %s: %w", op, path, err)
	}
	if secret == nil || secret.Data == nil {
		return nil, fmt.Errorf("vault transit %s %s: empty response", op, path)
	}
	return secret.Data, nil
}
```

Add `newAuth` to `auth.go`, dispatching on `cfg.Auth`:

```go
// newAuth selects an authentication method. An unknown method is a startup
// error rather than a silent fallback: a typo in a security-relevant setting
// must not fail open.
func newAuth(cfg Config, client *vaultapi.Client) (vaultAuth, error) {
	switch cfg.Auth {
	case "", "token":
		return newStaticTokenAuth(cfg.Token, cfg.TokenFile)
	default:
		return nil, fmt.Errorf("UNIFIED_VAULT_AUTH %q is not supported; supported methods: token", cfg.Auth)
	}
}
```

(Task 7 adds the `kubernetes` case and extends the message.)

- [ ] **Step 5: Verify and commit**

Run: `go test ./internal/secrets/vault/ -race -count=1 -v`
Expected: PASS

```bash
git add go.mod go.sum internal/secrets/vault/
git commit -m "feat(vault): add Transit-backed KeyManager"
```

---

### Task 5: DEK cache

**Files:**
- Create: `internal/secrets/vault/cache.go`
- Test: `internal/secrets/vault/cache_test.go`
- Modify: `internal/secrets/vault/keymanager.go` (consult the cache in `DecryptKey`)

**Interfaces:**
- Produces: `type dekCache struct` with `get(key string) ([]byte, bool)`, `put(key string, dek []byte)`, `len() int`

- [ ] **Step 1: Write the failing test**

Create `internal/secrets/vault/cache_test.go`:

```go
package vault

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDEKCache_HitAndMiss(t *testing.T) {
	clock := newTestClock()
	c := newDEKCache(4, time.Minute, clock.now)

	_, ok := c.get("a")
	assert.False(t, ok)

	c.put("a", []byte("dek-a"))
	got, ok := c.get("a")
	require.True(t, ok)
	assert.Equal(t, []byte("dek-a"), got)
}

// secrets.Decrypt zeroes the DEK returned by DecryptKey on every call, via a
// defer. If the cache handed out its own slice, the caller would zero the cache
// entry and every later hit would return zeros.
func TestDEKCache_ReturnsACopy(t *testing.T) {
	clock := newTestClock()
	c := newDEKCache(4, time.Minute, clock.now)
	c.put("a", []byte("dek-a"))

	first, ok := c.get("a")
	require.True(t, ok)
	for i := range first {
		first[i] = 0 // simulate the caller's zeroing defer
	}

	second, ok := c.get("a")
	require.True(t, ok)
	assert.Equal(t, []byte("dek-a"), second,
		"a caller zeroing its copy must not poison the cache")
}

// put must copy too, or the caller's buffer and the cache alias.
func TestDEKCache_CopiesOnPut(t *testing.T) {
	clock := newTestClock()
	c := newDEKCache(4, time.Minute, clock.now)

	dek := []byte("dek-a")
	c.put("a", dek)
	for i := range dek {
		dek[i] = 0
	}

	got, ok := c.get("a")
	require.True(t, ok)
	assert.Equal(t, []byte("dek-a"), got)
}

// The TTL is what makes a Transit key rotation or revocation take effect within
// a bounded window instead of never.
func TestDEKCache_ExpiresAfterTTL(t *testing.T) {
	clock := newTestClock()
	c := newDEKCache(4, time.Minute, clock.now)
	c.put("a", []byte("dek-a"))

	clock.advance(59 * time.Second)
	_, ok := c.get("a")
	assert.True(t, ok)

	clock.advance(2 * time.Second)
	_, ok = c.get("a")
	assert.False(t, ok, "an entry past its TTL must not be served")
}

func TestDEKCache_EvictsLeastRecentlyUsed(t *testing.T) {
	clock := newTestClock()
	c := newDEKCache(2, time.Minute, clock.now)

	c.put("a", []byte("dek-a"))
	c.put("b", []byte("dek-b"))
	_, _ = c.get("a") // a is now more recently used than b
	c.put("c", []byte("dek-c"))

	assert.Equal(t, 2, c.len())
	_, ok := c.get("b")
	assert.False(t, ok, "the least recently used entry is evicted")
	_, ok = c.get("a")
	assert.True(t, ok)
}

// An evicted plaintext DEK must not linger in memory, matching the zeroing
// discipline in internal/secrets/crypto.go.
func TestDEKCache_ZeroesEvictedEntries(t *testing.T) {
	clock := newTestClock()
	c := newDEKCache(1, time.Minute, clock.now)

	c.put("a", []byte("dek-a"))
	evicted := c.peekStored("a")
	require.NotNil(t, evicted)

	c.put("b", []byte("dek-b"))

	assert.Equal(t, make([]byte, len(evicted)), evicted,
		"the evicted entry's backing array must be zeroed")
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/secrets/vault/ -run TestDEKCache -v`
Expected: FAIL — `undefined: newDEKCache`

- [ ] **Step 3: Write `internal/secrets/vault/cache.go`**

Follow the house LRU idiom — `map[string]*list.Element` plus `container/list`, as in `internal/controller/failure_backoff.go` and `internal/controller/credential_touch_limiter.go`. No third-party LRU library is used anywhere in this repo.

```go
package vault

import (
	"container/list"
	"sync"
	"time"
)

// dekCache caches unwrapped DEKs so a job claim carrying several secrets does
// not call Transit once per secret.
//
// This is load reduction and a cushion for brief outages — NOT a substitute for
// Vault HA. A job requesting an uncached secret while Vault is unreachable
// still fails.
//
// Relative to local-key mode, where the KEK sits in process memory for the
// controller's whole life, holding a few DEKs for a few minutes is a stricter
// posture, not a new class of exposure.
type dekCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	lru     *list.List // front = most recently used
	cap     int
	ttl     time.Duration
	now     func() time.Time
}

type dekEntry struct {
	key       string
	dek       []byte
	expiresAt time.Time
}

func newDEKCache(capacity int, ttl time.Duration, now func() time.Time) *dekCache {
	if now == nil {
		now = time.Now
	}
	return &dekCache{
		entries: make(map[string]*list.Element),
		lru:     list.New(),
		cap:     capacity,
		ttl:     ttl,
		now:     now,
	}
}

// get returns a COPY of the cached DEK.
//
// A copy is mandatory, not defensive style: secrets.Decrypt zeroes the DEK
// returned by DecryptKey on every call via a defer
// (internal/secrets/crypto.go:66-72). Handing out the cache's own slice would
// let the caller zero the entry, and every later hit would return zeros.
func (c *dekCache) get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	entry := el.Value.(*dekEntry)
	if !c.now().Before(entry.expiresAt) {
		c.removeLocked(el)
		return nil, false
	}
	c.lru.MoveToFront(el)
	out := make([]byte, len(entry.dek))
	copy(out, entry.dek)
	return out, true
}

// put stores a copy of dek, so a caller reusing or zeroing its buffer cannot
// corrupt the entry.
func (c *dekCache) put(key string, dek []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.entries[key]; ok {
		c.removeLocked(el)
	}
	stored := make([]byte, len(dek))
	copy(stored, dek)
	el := c.lru.PushFront(&dekEntry{key: key, dek: stored, expiresAt: c.now().Add(c.ttl)})
	c.entries[key] = el

	for c.lru.Len() > c.cap {
		c.removeLocked(c.lru.Back())
	}
}

func (c *dekCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len()
}

// removeLocked drops an entry and zeroes its plaintext, matching the zeroing
// discipline in internal/secrets/crypto.go.
func (c *dekCache) removeLocked(el *list.Element) {
	entry := el.Value.(*dekEntry)
	for i := range entry.dek {
		entry.dek[i] = 0
	}
	c.lru.Remove(el)
	delete(c.entries, entry.key)
}

// peekStored exposes the stored backing array for tests that verify zeroing.
// It deliberately does not copy.
func (c *dekCache) peekStored(key string) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return nil
	}
	return el.Value.(*dekEntry).dek
}
```

- [ ] **Step 4: Wire the cache into `DecryptKey`**

In `keymanager.go`, add a `cache *dekCache` field, construct it in `New` (capacity 1024, TTL 5 minutes, `cfg.Now`), and consult it at the top of `DecryptKey` after the provider check:

```go
	cacheKey := string(ciphertext)
	if dek, ok := m.cache.get(cacheKey); ok {
		return dek, nil
	}
```

and store the result before returning:

```go
	m.cache.put(cacheKey, dek)
	return dek, nil
```

The cache key is the wrapped-DEK blob, which is ciphertext and not itself secret.

- [ ] **Step 5: Add an integration-level cache test**

Append to `keymanager_test.go`:

```go
// A second decrypt of the same wrapped DEK must not call Transit again, and
// must still return usable bytes after the first caller zeroed its copy.
func TestKeyManager_CachesUnwrappedDEKs(t *testing.T) {
	var decrypts atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/token/lookup-self", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": map[string]any{"ttl": 3600, "renewable": true}})
	})
	mux.HandleFunc("/v1/transit/decrypt/", func(w http.ResponseWriter, r *http.Request) {
		decrypts.Add(1)
		var req struct {
			Ciphertext string `json:"ciphertext"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		writeJSON(w, map[string]any{"data": map[string]any{
			"plaintext": strings.TrimPrefix(req.Ciphertext, "vault:v1:"),
		}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	m := newTestKeyManager(t, srv.URL)
	wrapped := []byte("vault:v1:" + base64.StdEncoding.EncodeToString([]byte("dek")))

	first, err := m.DecryptKey(context.Background(), wrapped)
	require.NoError(t, err)
	for i := range first {
		first[i] = 0 // the caller's zeroing defer
	}

	second, err := m.DecryptKey(context.Background(), wrapped)
	require.NoError(t, err)
	assert.Equal(t, []byte("dek"), second)
	assert.EqualValues(t, 1, decrypts.Load(), "the second decrypt must be served from cache")
}
```

Add `"sync/atomic"` to the imports.

- [ ] **Step 6: Verify and commit**

Run: `go test ./internal/secrets/vault/ -race -count=1 -v`
Expected: PASS

```bash
git add internal/secrets/vault/
git commit -m "feat(vault): cache unwrapped DEKs with TTL, LRU eviction, and copy-on-read"
```

---

### Task 6: Wire into `KeySource.Resolve`

**Files:**
- Modify: `internal/config/keysource.go`
- Modify: `internal/config/keysource_test.go` (delete `TestKeySource_KMSURINotImplementedYet`)
- Modify: `internal/config/controller.go` (new env vars)
- Modify: `docs/configuration.md:55`

**Interfaces:**
- Consumes: `vault.New`, `vault.Config`
- Produces: `hashivault://` URIs resolve to a working `KeyManager`

- [ ] **Step 1: Write the failing test**

Replace `TestKeySource_KMSURINotImplementedYet` in `keysource_test.go` with:

```go
// The URI names the key, and optionally the mount it lives on.
func TestParseKMSURI(t *testing.T) {
	mount, key, err := parseHashiVaultURI("hashivault://unified-cd-kek")
	require.NoError(t, err)
	assert.Equal(t, "transit", mount, "the default mount is transit")
	assert.Equal(t, "unified-cd-kek", key)

	mount, key, err = parseHashiVaultURI("hashivault://kms-transit/unified-cd-kek")
	require.NoError(t, err)
	assert.Equal(t, "kms-transit", mount)
	assert.Equal(t, "unified-cd-kek", key)
}

// More than two segments is a configuration error rather than a guess.
func TestParseKMSURI_RejectsTooManySegments(t *testing.T) {
	_, _, err := parseHashiVaultURI("hashivault://a/b/c")
	require.Error(t, err)
}

func TestParseKMSURI_RejectsEmptyKey(t *testing.T) {
	_, _, err := parseHashiVaultURI("hashivault://")
	require.Error(t, err)
}

// A KMS URI with no address cannot work, and must say which variable is missing.
func TestKeySource_KMSRequiresAddress(t *testing.T) {
	_, err := KeySource{KMSURI: "hashivault://kek"}.Resolve(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "UNIFIED_VAULT_ADDR")
}

func TestParseAuthParams(t *testing.T) {
	got, err := parseAuthParams("role=unified-cd,mount=kubernetes")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"role": "unified-cd", "mount": "kubernetes"}, got)

	empty, err := parseAuthParams("")
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// A malformed parameter must not be silently dropped: a typo in a
// security-relevant setting must not fail open.
func TestParseAuthParams_RejectsMalformed(t *testing.T) {
	_, err := parseAuthParams("role")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "key=value")
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/config/ -run 'TestParseKMSURI|TestParseAuthParams|TestKeySource_KMS' -v`
Expected: FAIL — `undefined: parseHashiVaultURI`, `undefined: parseAuthParams`

- [ ] **Step 3: Implement the wiring in `keysource.go`**

Add the `VaultAddr`, `VaultAuth`, `VaultAuthParam`, `VaultToken`, `VaultTokenFile` fields to `KeySource`, then:

```go
// parseHashiVaultURI splits hashivault://[<mount>/]<key>.
func parseHashiVaultURI(uri string) (mount, key string, err error) {
	_, rest, found := strings.Cut(uri, "://")
	if !found {
		return "", "", fmt.Errorf("UNIFIED_KMS_URI %q is malformed; expected hashivault://[<mount>/]<key>", uri)
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	switch {
	case len(parts) == 1 && parts[0] != "":
		return "transit", parts[0], nil
	case len(parts) == 2 && parts[0] != "" && parts[1] != "":
		return parts[0], parts[1], nil
	default:
		return "", "", fmt.Errorf("UNIFIED_KMS_URI %q is malformed; expected hashivault://[<mount>/]<key>", uri)
	}
}

// parseAuthParams reads comma-separated key=value pairs.
func parseAuthParams(s string) (map[string]string, error) {
	out := map[string]string{}
	if strings.TrimSpace(s) == "" {
		return out, nil
	}
	for _, pair := range strings.Split(s, ",") {
		k, v, found := strings.Cut(strings.TrimSpace(pair), "=")
		if !found || k == "" {
			return nil, fmt.Errorf("UNIFIED_VAULT_AUTH_PARAM entry %q is malformed; expected key=value", pair)
		}
		out[k] = v
	}
	return out, nil
}
```

Replace the `case k.KMSURI != "":` arm:

```go
	case k.KMSURI != "":
		return k.resolveKMS(ctx)
```

```go
func (k KeySource) resolveKMS(ctx context.Context) (Resolved, error) {
	scheme, _, _ := strings.Cut(k.KMSURI, "://")
	if scheme != "hashivault" {
		return Resolved{}, kmsError(k.KMSURI)
	}
	if k.VaultAddr == "" {
		return Resolved{}, fmt.Errorf("UNIFIED_KMS_URI is set but UNIFIED_VAULT_ADDR is not")
	}
	mount, key, err := parseHashiVaultURI(k.KMSURI)
	if err != nil {
		return Resolved{}, err
	}
	params, err := parseAuthParams(k.VaultAuthParam)
	if err != nil {
		return Resolved{}, err
	}
	km, err := vault.New(ctx, vault.Config{
		Address: k.VaultAddr, Mount: mount, Key: key,
		Auth: k.VaultAuth, AuthParams: params,
		Token: k.VaultToken, TokenFile: k.VaultTokenFile,
	})
	if err != nil {
		return Resolved{}, err
	}
	return Resolved{
		KeyManager:  km,
		Description: fmt.Sprintf("vault transit %s/%s at %s", mount, key, k.VaultAddr),
		closeFn:     km.Close,
	}, nil
}
```

Delete the "not implemented in this build" arm from `kmsError`, keeping the malformed-URI and unknown-scheme arms.

- [ ] **Step 4: Add the env vars in `controller.go`**

In `ControllerEffective`'s `KeySource` literal:

```go
		KeySource: KeySource{
			KeyFile:        os.Getenv("UNIFIED_CONTROLLER_KEY_FILE"),
			KMSURI:         os.Getenv("UNIFIED_KMS_URI"),
			DevMode:        envBool("UNIFIED_DEV_MODE"),
			VaultAddr:      os.Getenv("UNIFIED_VAULT_ADDR"),
			VaultAuth:      os.Getenv("UNIFIED_VAULT_AUTH"),
			VaultAuthParam: os.Getenv("UNIFIED_VAULT_AUTH_PARAM"),
			VaultToken:     os.Getenv("VAULT_TOKEN"),
			VaultTokenFile: os.Getenv("UNIFIED_VAULT_TOKEN_FILE"),
		},
```

- [ ] **Step 5: Update `docs/configuration.md`**

Line 55 currently reads "No provider is implemented yet" — that becomes false with this task. Replace the row and add the new variables:

```markdown
| `UNIFIED_KMS_URI` | Optional | External KMS: `hashivault://[<mount>/]<key>` (default mount `transit`). The controller wraps DEKs with Vault/OpenBao Transit and never holds the key itself. |
| `UNIFIED_VAULT_ADDR` | With KMS | Vault/OpenBao address. |
| `UNIFIED_VAULT_AUTH` | Optional | `token` (default) or `kubernetes`. |
| `UNIFIED_VAULT_AUTH_PARAM` | With `kubernetes` | Comma-separated `key=value`; `kubernetes` requires `role`. |
| `UNIFIED_VAULT_TOKEN_FILE` | With `token` | Path to a file holding the token. Preferred over `VAULT_TOKEN`: a file does not leak into `docker inspect` or child processes, and can be replaced without a restart. |
| `VAULT_TOKEN` | With `token` | Fallback when no token file is set. |
```

- [ ] **Step 6: Distinguish runtime failure classes in the controller's log**

Spec §5 requires that an unreachable KMS — an *availability* event — never share a log line or severity with `ErrBindingMismatch`, which is a *security* event. `internal/controller/api_secrets.go:144-162` already implements that split for binding mismatch; extend it rather than adding a second logging path.

Add a test to `internal/controller/api_secrets_test.go`:

```go
// A provider mismatch means someone is presenting locally-wrapped data to a
// KMS-configured controller: security-relevant, and distinct from a KMS being
// briefly unreachable.
func TestLogSecretDecryptFailure_SeparatesProviderMismatch(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	logSecretDecryptFailure("agent-fetch", "MY_SECRET",
		fmt.Errorf("%w: this controller is configured for the hashivault key provider", secrets.ErrProviderMismatch))

	out := buf.String()
	assert.Contains(t, out, "level=ERROR", "a provider mismatch is a security event")
	assert.Contains(t, out, "MY_SECRET")
	assert.NotContains(t, out, "binding mismatch", "it must not be reported as a binding mismatch")
}

// An unreachable KMS is an availability event: Warn, with the error attached so
// an operator can see what failed.
func TestLogSecretDecryptFailure_KMSUnreachableIsWarn(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	logSecretDecryptFailure("agent-fetch", "MY_SECRET", errors.New("dial tcp: connection refused"))

	out := buf.String()
	assert.Contains(t, out, "level=WARN")
	assert.Contains(t, out, "connection refused")
}
```

Run it, watch the first test fail (a provider mismatch currently falls into the generic `Warn` branch), then add the branch:

```go
func logSecretDecryptFailure(site, id string, err error) {
	if errors.Is(err, secrets.ErrBindingMismatch) {
		slog.Error("secret decrypt: binding mismatch (possible ciphertext tampering or substitution)",
			"site", site, "id", id)
		return
	}
	if errors.Is(err, secrets.ErrProviderMismatch) {
		slog.Error("secret decrypt: wrapped key came from a different key provider (check UNIFIED_KMS_URI / UNIFIED_CONTROLLER_KEY_FILE)",
			"site", site, "id", id)
		return
	}
	slog.Warn("secret decrypt failed", "site", site, "id", id, "error", err)
}
```

Both security branches omit `err` and log only identifiers, matching the existing contract stated in that function's doc comment.

- [ ] **Step 7: Verify and commit**

Run: `go build ./... && go test ./internal/config/ ./internal/secrets/... ./internal/controller/ -race -count=1`
Expected: PASS

```bash
git add internal/config/ internal/controller/ docs/configuration.md
git commit -m "feat(config): resolve hashivault:// URIs to a Transit key manager"
```

---

### Task 7: Kubernetes authentication

**Files:**
- Modify: `internal/secrets/vault/auth.go`
- Test: `internal/secrets/vault/auth_test.go`

**Interfaces:**
- Produces: `UNIFIED_VAULT_AUTH=kubernetes` with `role` in `UNIFIED_VAULT_AUTH_PARAM`

- [ ] **Step 1: Write the failing test**

Append to `auth_test.go`:

```go
func TestKubernetesAuth_LogsInWithServiceAccountToken(t *testing.T) {
	var gotPath, gotRole, gotJWT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var req map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		gotRole, gotJWT = req["role"], req["jwt"]
		writeJSON(w, map[string]any{"auth": map[string]any{
			"client_token": "s.k8s", "lease_duration": 1800, "renewable": true,
		}})
	}))
	t.Cleanup(srv.Close)

	jwtFile := writeTokenFile(t, "projected-sa-jwt")
	client, err := vaultapi.NewClient(&vaultapi.Config{Address: srv.URL})
	require.NoError(t, err)

	a, err := newKubernetesAuth(client, map[string]string{"role": "unified-cd"}, jwtFile)
	require.NoError(t, err)
	got, err := a.login(context.Background())
	require.NoError(t, err)

	assert.Equal(t, "/v1/auth/kubernetes/login", gotPath)
	assert.Equal(t, "unified-cd", gotRole)
	assert.Equal(t, "projected-sa-jwt", gotJWT)
	assert.Equal(t, "s.k8s", got.Token)
	assert.Equal(t, 30*time.Minute, got.TTL)
	assert.True(t, got.Renewable)
}

// The mount is configurable for operators who mount the method elsewhere.
func TestKubernetesAuth_HonoursMountParam(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeJSON(w, map[string]any{"auth": map[string]any{"client_token": "s.k8s", "lease_duration": 60}})
	}))
	t.Cleanup(srv.Close)

	client, err := vaultapi.NewClient(&vaultapi.Config{Address: srv.URL})
	require.NoError(t, err)
	a, err := newKubernetesAuth(client, map[string]string{"role": "r", "mount": "k8s-prod"}, writeTokenFile(t, "jwt"))
	require.NoError(t, err)
	_, err = a.login(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "/v1/auth/k8s-prod/login", gotPath)
}

func TestKubernetesAuth_RequiresRole(t *testing.T) {
	client, err := vaultapi.NewClient(vaultapi.DefaultConfig())
	require.NoError(t, err)
	_, err = newKubernetesAuth(client, map[string]string{}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "role")
}

// An unrecognised parameter is a startup error: a typo in a security-relevant
// setting must not fail open.
func TestNewAuth_RejectsUnknownParam(t *testing.T) {
	client, err := vaultapi.NewClient(vaultapi.DefaultConfig())
	require.NoError(t, err)
	_, err = newKubernetesAuth(client, map[string]string{"role": "r", "rolle": "typo"}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rolle")
}

func TestNewAuth_RejectsUnknownMethod(t *testing.T) {
	_, err := newAuth(Config{Auth: "approle"}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "approle")
	assert.Contains(t, err.Error(), "token")
	assert.Contains(t, err.Error(), "kubernetes")
}
```

Add `"encoding/json"`, `"net/http"`, `"net/http/httptest"`, `"time"`, and `vaultapi "github.com/hashicorp/vault/api"` to the imports.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/secrets/vault/ -run 'TestKubernetesAuth|TestNewAuth' -v`
Expected: FAIL — `undefined: newKubernetesAuth`

- [ ] **Step 3: Implement in `auth.go`**

```go
// defaultServiceAccountTokenFile is where the kubelet projects a pod's
// ServiceAccount token. Nothing has to be distributed for this method to work,
// which is what makes it the right answer at scale.
const defaultServiceAccountTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"

type kubernetesAuth struct {
	client    *vaultapi.Client
	mount     string
	role      string
	tokenFile string
}

func newKubernetesAuth(client *vaultapi.Client, params map[string]string, tokenFile string) (vaultAuth, error) {
	role := params["role"]
	if role == "" {
		return nil, fmt.Errorf("UNIFIED_VAULT_AUTH=kubernetes requires role= in UNIFIED_VAULT_AUTH_PARAM")
	}
	mount := params["mount"]
	if mount == "" {
		mount = "kubernetes"
	}
	for k := range params {
		if k != "role" && k != "mount" {
			return nil, fmt.Errorf("UNIFIED_VAULT_AUTH_PARAM key %q is not recognised for kubernetes auth; supported: role, mount", k)
		}
	}
	if tokenFile == "" {
		tokenFile = defaultServiceAccountTokenFile
	}
	return &kubernetesAuth{client: client, mount: mount, role: role, tokenFile: tokenFile}, nil
}

func (a *kubernetesAuth) login(ctx context.Context) (authResult, error) {
	// Re-read on every login: the kubelet rotates the projected token.
	raw, err := os.ReadFile(a.tokenFile)
	if err != nil {
		return authResult{}, fmt.Errorf("read service account token %s: %w", a.tokenFile, err)
	}
	secret, err := a.client.Logical().WriteWithContext(ctx, a.mount+"/login", map[string]any{
		"role": a.role,
		"jwt":  strings.TrimSpace(string(raw)),
	})
	if err != nil {
		return authResult{}, fmt.Errorf("vault kubernetes login (role %s): %w", a.role, err)
	}
	if secret == nil || secret.Auth == nil {
		return authResult{}, fmt.Errorf("vault kubernetes login (role %s): empty auth response", a.role)
	}
	return authResult{
		Token:     secret.Auth.ClientToken,
		TTL:       time.Duration(secret.Auth.LeaseDuration) * time.Second,
		Renewable: secret.Auth.Renewable,
	}, nil
}
```

Extend `newAuth`:

```go
func newAuth(cfg Config, client *vaultapi.Client) (vaultAuth, error) {
	switch cfg.Auth {
	case "", "token":
		return newStaticTokenAuth(cfg.Token, cfg.TokenFile)
	case "kubernetes":
		return newKubernetesAuth(client, cfg.AuthParams, cfg.AuthParams["token_file"])
	default:
		return nil, fmt.Errorf("UNIFIED_VAULT_AUTH %q is not supported; supported methods: token, kubernetes", cfg.Auth)
	}
}
```

Note `token_file` must then be accepted by `newKubernetesAuth`'s parameter allowlist — add it alongside `role` and `mount`.

- [ ] **Step 4: Verify and commit**

Run: `go test ./internal/secrets/vault/ -race -count=1 -v`
Expected: PASS

```bash
git add internal/secrets/vault/
git commit -m "feat(vault): add Kubernetes authentication"
```

---

### Task 8: OpenBao integration test and compose overlay

**Files:**
- Create: `internal/secrets/vault/integration_test.go`
- Create: `docker-compose.openbao.yml`

**Interfaces:**
- Consumes: everything above
- Produces: a real end-to-end round trip against OpenBao

- [ ] **Step 1: Write the integration test**

Follow `internal/store/testutil.go`'s shared-container pattern: one container per test binary via `sync.Once`, `AutoRemove`, `Expire(900)`, `pool.MaxWait` + `pool.Retry`, and **`testing.Short()` as the first gate** so the `unit` CI job (which runs `-short` on three OSes) skips it while the existing `integration` job picks it up with no CI change.

Per-test isolation comes from a **distinct transit key name per test** (`fmt.Sprintf("test_%d", seq.Add(1))`), mirroring how `NewTestPostgres` clones a database per test rather than starting a container per test.

```go
package vault

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const openBaoRootToken = "dev-root-token"

var (
	sharedBaoOnce sync.Once
	sharedBaoAddr string
	sharedBaoErr  error
	testKeySeq    atomic.Int64
)

func startSharedOpenBao() {
	pool, err := dockertest.NewPool("")
	if err != nil {
		sharedBaoErr = fmt.Errorf("dockertest pool: %w", err)
		return
	}
	resource, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository: "openbao/openbao",
		Tag:        "latest",
		Env: []string{
			"BAO_DEV_ROOT_TOKEN_ID=" + openBaoRootToken,
			"BAO_DEV_LISTEN_ADDRESS=0.0.0.0:8200",
		},
		Cmd: []string{"server", "-dev"},
	}, func(c *docker.HostConfig) {
		c.AutoRemove = true
		c.RestartPolicy = docker.RestartPolicy{Name: "no"}
	})
	if err != nil {
		sharedBaoErr = fmt.Errorf("dockertest run: %w", err)
		return
	}
	_ = resource.Expire(900)

	addr := "http://localhost:" + resource.GetPort("8200/tcp")
	pool.MaxWait = 60 * time.Second
	if err := pool.Retry(func() error { return pingBao(addr) }); err != nil {
		sharedBaoErr = fmt.Errorf("openbao not ready: %w", err)
		return
	}
	if err := enableTransit(addr); err != nil {
		sharedBaoErr = fmt.Errorf("enable transit: %w", err)
		return
	}
	sharedBaoAddr = addr
}

// newTestTransitKey returns an address and a freshly created key name, so tests
// do not share key state.
func newTestTransitKey(t *testing.T) (addr, key string) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	sharedBaoOnce.Do(startSharedOpenBao)
	if sharedBaoErr != nil {
		t.Fatalf("shared openbao: %v", sharedBaoErr)
	}
	key = fmt.Sprintf("test_%d", testKeySeq.Add(1))
	require.NoError(t, createTransitKey(sharedBaoAddr, key))
	return sharedBaoAddr, key
}

func TestIntegration_TransitRoundTrip(t *testing.T) {
	addr, key := newTestTransitKey(t)
	m, err := New(context.Background(), Config{
		Address: addr, Mount: "transit", Key: key,
		Auth: "token", Token: openBaoRootToken,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = m.Close() })

	dek := []byte("0123456789abcdef0123456789abcdef")
	wrapped, err := m.EncryptKey(context.Background(), dek)
	require.NoError(t, err)
	got, err := m.DecryptKey(context.Background(), wrapped)
	require.NoError(t, err)
	assert.Equal(t, dek, got)
}

// The whole point of the change: a DEK wrapped by Transit round-trips through
// the real envelope layer that stores secrets.
func TestIntegration_EnvelopeThroughTransit(t *testing.T) {
	addr, key := newTestTransitKey(t)
	m, err := New(context.Background(), Config{
		Address: addr, Mount: "transit", Key: key,
		Auth: "token", Token: openBaoRootToken,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = m.Close() })

	b := secrets.SecretBinding("MY_SECRET", "global", "")
	encDEK, ct, err := secrets.Encrypt(context.Background(), m, []byte("hunter2"), b)
	require.NoError(t, err)
	plain, err := secrets.Decrypt(context.Background(), m, encDEK, ct, b)
	require.NoError(t, err)
	assert.Equal(t, "hunter2", string(plain))
}
```

The three helpers, using the API client with the root token:

```go
func baoClient(addr string) (*vaultapi.Client, error) {
	cfg := vaultapi.DefaultConfig()
	cfg.Address = addr
	c, err := vaultapi.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	c.SetToken(openBaoRootToken)
	return c, nil
}

func pingBao(addr string) error {
	c, err := baoClient(addr)
	if err != nil {
		return err
	}
	health, err := c.Sys().Health()
	if err != nil {
		return err
	}
	if !health.Initialized || health.Sealed {
		return fmt.Errorf("openbao not ready: initialized=%v sealed=%v", health.Initialized, health.Sealed)
	}
	return nil
}

func enableTransit(addr string) error {
	c, err := baoClient(addr)
	if err != nil {
		return err
	}
	err = c.Sys().Mount("transit", &vaultapi.MountInput{Type: "transit"})
	if err != nil && strings.Contains(err.Error(), "path is already in use") {
		return nil // idempotent: the shared container may already have it
	}
	return err
}

func createTransitKey(addr, name string) error {
	c, err := baoClient(addr)
	if err != nil {
		return err
	}
	_, err = c.Logical().Write("transit/keys/"+name, map[string]any{})
	return err
}
```

Add `"strings"`, `vaultapi "github.com/hashicorp/vault/api"`, and `"github.com/eirueimi/unified-cd/internal/secrets"` to the file's imports.

- [ ] **Step 2: Run it**

Run: `go test ./internal/secrets/vault/ -run TestIntegration -count=1 -v`
Expected: PASS (pulls the OpenBao image on first run)

Then confirm the short-mode gate: `go test ./internal/secrets/vault/ -short -count=1` must skip them.

- [ ] **Step 3: Write `docker-compose.openbao.yml`**

Follow `docker-compose.sso.yml`: a header comment with the literal `-f` invocation and a "How it works" narrative, and **`depends_on` restated in full** including the pre-existing `postgres` and `garage`.

```yaml
# Compose override for local Vault/OpenBao Transit development
#
# Usage:
#   docker compose -f docker-compose.yaml -f docker-compose.openbao.yml up -d
#
# How it works:
#   OpenBao runs in dev mode: in-memory, auto-unsealed, with a fixed root token.
#   Nothing it holds survives the container, so the fixed token below is a
#   development convenience, not a secret. Every value has a ${VAR:-default}
#   escape hatch so you can point at your own instance instead.
#
#   NOT FOR PRODUCTION. In production, Vault/OpenBao is operator-provided —
#   either an existing organisational instance or a deployment made with the
#   official Helm chart. See docs/high-availability.md.
#
#   openbao-init enables the transit engine and creates the key, then exits.
#   The controller waits for it so the key exists before the first login.
#
#   UNIFIED_KMS_URI takes precedence over the UNIFIED_DEV_MODE=1 inherited from
#   docker-compose.yaml (Resolve checks KMSURI first), so you do not need to
#   unset the latter.

services:

  openbao:
    image: openbao/openbao:latest
    command: server -dev
    environment:
      BAO_DEV_ROOT_TOKEN_ID: ${BAO_DEV_ROOT_TOKEN:-dev-root-token}
      BAO_DEV_LISTEN_ADDRESS: 0.0.0.0:8200
    ports:
      - "8200:8200"
    healthcheck:
      test: [ "CMD", "bao", "status" ]
      interval: 5s
      timeout: 3s
      retries: 10
    restart: unless-stopped

  openbao-init:
    image: openbao/openbao:latest
    depends_on:
      openbao:
        condition: service_healthy
    environment:
      BAO_ADDR: http://openbao:8200
      BAO_TOKEN: ${BAO_DEV_ROOT_TOKEN:-dev-root-token}
    entrypoint:
      - sh
      - -c
      - |
        bao secrets enable transit 2>/dev/null || true
        bao write -f transit/keys/${UNIFIED_VAULT_KEY:-unified-cd-kek}
    restart: "no"

  controller:
    depends_on:
      postgres:
        condition: service_healthy
      garage:
        condition: service_healthy
      openbao-init:
        condition: service_completed_successfully
    environment:
      UNIFIED_KMS_URI: hashivault://${UNIFIED_VAULT_KEY:-unified-cd-kek}
      UNIFIED_VAULT_ADDR: http://openbao:8200
      UNIFIED_VAULT_AUTH: token
      VAULT_TOKEN: ${BAO_DEV_ROOT_TOKEN:-dev-root-token}
```

- [ ] **Step 4: Verify the overlay**

```bash
docker compose -f docker-compose.yaml -f docker-compose.openbao.yml config
```
Expected: resolves with no error; the controller shows `UNIFIED_KMS_URI` set.

Then bring it up and confirm the controller logs `encryption key loaded source=vault transit …`.

- [ ] **Step 5: Commit**

```bash
git add internal/secrets/vault/ docker-compose.openbao.yml
git commit -m "test(vault): add OpenBao integration suite and dev compose overlay"
```

---

### Task 9: Documentation

**Files:**
- Modify: `docs/high-availability.md` (new Vault section)
- Create or modify: `docs/secrets.md` (Transit setup and policy)
- Modify: `manifests/README.md` (Kubernetes auth configuration)

- [ ] **Step 1: Add the Vault policy and setup**

In `docs/secrets.md`, add a section giving the operator exactly what unified-cd needs — this is the part we own, as opposed to how to deploy Vault:

````markdown
## Using Vault or OpenBao (Transit)

The controller wraps each data-encryption key with Transit, so the key-encryption
key never leaves the KMS.

Enable the engine and create the key:

```sh
vault secrets enable transit
vault write -f transit/keys/unified-cd-kek
```

The controller needs exactly two capabilities:

```hcl
path "transit/encrypt/unified-cd-kek" {
  capabilities = ["update"]
}

path "transit/decrypt/unified-cd-kek" {
  capabilities = ["update"]
}
```

Then configure the controller:

```sh
UNIFIED_KMS_URI=hashivault://unified-cd-kek
UNIFIED_VAULT_ADDR=https://vault.example.com:8200
UNIFIED_VAULT_TOKEN_FILE=/run/secrets/vault-token
```

### Tokens need renewing, not rotating

A **periodic** token's TTL resets on every renewal, so the controller keeps it
alive indefinitely on its own. A token that is not periodic dies at its max TTL
(32 days by default) however often it is renewed, and must genuinely be
replaced — put it in a file and the controller picks up the replacement without
a restart.

### Kubernetes

On Kubernetes, use the Kubernetes auth method instead: the pod's ServiceAccount
token is already projected by the kubelet, so there is no credential to
distribute or rotate.

```sh
UNIFIED_VAULT_AUTH=kubernetes
UNIFIED_VAULT_AUTH_PARAM=role=unified-cd
```
````

- [ ] **Step 2: Add the HA section**

In `docs/high-availability.md`, add a section in the same shape as the existing PostgreSQL (`:299`) and S3 (`:307`) sections:

```markdown
### Vault / OpenBao (when `UNIFIED_KMS_URI` is used)

- **Operator-provided.** Like Postgres and object storage, unified-cd does not
  ship a production Vault. Use an existing instance, or deploy with the official
  Helm chart.
- **Every replica points at the same Vault**, and at the Vault *service* rather
  than a specific node: Vault HA is active/standby and standbys forward requests
  to the active node.
- **Vault HA gives failover, not throughput.** All Transit calls are served by
  the active node; HA shortens unplanned outages rather than scaling capacity.
- **The controller fails closed.** It will not start if Vault is unreachable, so
  a Vault outage during a rollout crash-loops pods until Vault returns. This is
  deliberate: a controller that started without a key manager would fail every
  secret operation anyway.
- **The DEK cache is not a substitute for HA.** It absorbs brief blips and
  reduces load on the active node, but a job needing an uncached secret while
  Vault is down still fails.
- **Auto-unseal is a prerequisite for unattended HA** — without it, every node
  restart needs a manual unseal.
```

- [ ] **Step 3: Verify no stale claims remain**

Run: `go test ./internal/config/ -run TestNoStaleControllerKeyReferences -count=1`

Then grep for anything still saying the provider is unimplemented:

```bash
grep -rn "not implemented" docs/ examples/ --include=*.md --include=*.yaml
```
Expected: no hit referring to the KMS provider.

- [ ] **Step 4: Full suite on Linux**

```bash
wsl.exe -e bash -lc 'cd /mnt/c/Users/arimax/unified-cd-project/unified-cd-vault-transit && ./scripts/prepare-shim-placeholders.sh && TZ=UTC go test ./... -count=1'
```

- [ ] **Step 5: Commit**

```bash
git add docs/ manifests/README.md
git commit -m "docs: document Transit setup, policy, and HA implications"
```

---

## Final verification

- [ ] **Confirm the local path is unaffected**

`go test ./internal/config/ ./internal/secrets/... -race -count=1` — local and dev-mode key sources must behave exactly as before.

- [ ] **Confirm a local-wrapped DEK reports precisely under Vault**

The `TestKeyManager_RejectsForeignProvider` case covers this; verify the message names `hashivault`, so an operator can tell which provider is configured.

- [ ] **Push and open the PR**

The PR body must state: a new dependency (`hashicorp/vault/api`) is added; `Resolve` gained a context parameter and `Resolved` a `Close`; and no existing deployment changes behaviour unless it sets `UNIFIED_KMS_URI`.
