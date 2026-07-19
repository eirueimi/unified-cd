package vault

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

const (
	// retryInitialWait is the delay before the first retry after a failed
	// refresh, and the value the backoff resets to after any success.
	retryInitialWait = time.Minute
	// retryMaxWait bounds the failure-retry backoff so an extended outage
	// settles at a fixed cadence instead of retrying ever less often.
	retryMaxWait = 15 * time.Minute
)

// tokenManagerConfig supplies the token lifecycle's collaborators. Now and
// Sleep are injected so the renewal loop can be tested without wall-clock
// waiting, following the pattern in internal/k8sagent/credentials.go.
type tokenManagerConfig struct {
	Auth  vaultAuth
	Renew func(ctx context.Context, token string) (time.Duration, error)
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error

	// LoginCtx is used for the first login only, so a deadline set by the
	// caller of vault.New (e.g. in main) actually bounds startup. It defaults
	// to context.Background() when nil. The background renewal loop
	// deliberately does NOT derive from this context: the loop outlives
	// startup and must keep running even after a startup deadline expires.
	LoginCtx context.Context
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

	// reauthMu serialises reauthenticate calls so that several operations
	// hitting a 403 concurrently coalesce into one login instead of each
	// triggering its own. See reauthenticate.
	reauthMu sync.Mutex

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
	if cfg.LoginCtx == nil {
		cfg.LoginCtx = context.Background()
	}

	// Startup fails fast: at this point a transient outage and a
	// misconfiguration are indistinguishable, and a controller holding a
	// broken key manager fails every secret operation anyway. LoginCtx (not
	// context.Background()) is used here deliberately: it is the only hop
	// that still honours a caller-supplied deadline.
	first, err := cfg.Auth.login(cfg.LoginCtx)
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

// reauthenticate forces a fresh login and returns the resulting token,
// coalescing concurrent callers so that several operations that all observed
// the same now-rejected token (staleToken) trigger exactly one login between
// them rather than one each.
//
// reauthMu serialises the whole operation, including the login round trip
// itself. A caller that acquires the lock after another caller already
// refreshed the token notices via the staleToken comparison — the current
// token no longer matches what it read before its request failed — and
// returns the already-current token without logging in again. This is
// simpler than singleflight and sufficient here: a burst of 403s all racing
// in only needs to produce one login, not the lowest possible latency for it.
func (m *tokenManager) reauthenticate(ctx context.Context, staleToken string) (string, error) {
	m.reauthMu.Lock()
	defer m.reauthMu.Unlock()

	m.mu.RLock()
	current := m.current
	m.mu.RUnlock()
	if current != staleToken {
		// Someone else already refreshed the token since the caller read it.
		return current, nil
	}

	result, err := m.cfg.Auth.login(ctx)
	if err != nil {
		return "", fmt.Errorf("vault re-login: %w", err)
	}

	m.mu.Lock()
	m.current = result.Token
	m.mu.Unlock()

	return result.Token, nil
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
			wait = retryInitialWait
		}
		if err := m.cfg.Sleep(ctx, wait); err != nil {
			return // context cancelled
		}

		next, err := m.refresh(ctx, current)

		// Failure cadence is a bounded exponential backoff, independent of
		// the lease: the token is not being renewed while this runs, so
		// there is no lease to halve. Deriving the retry wait from the lease
		// (as a prior version did) decays geometrically toward zero and
		// turns an outage into a retry storm; growing and capping here is
		// what makes "runtime is patient" actually true. retryWait is local
		// to this loop iteration, so it starts fresh at retryInitialWait
		// every time a new failure streak begins.
		retryWait := retryInitialWait
		for err != nil {
			if ctx.Err() != nil {
				return
			}
			// Leave the existing token in place: it may still work, and the
			// next attempt retries. This is the "runtime is patient" half of
			// the design's asymmetry.
			slog.Warn("vault: token refresh failed, will retry", "retryIn", retryWait, "error", err)
			if serr := m.cfg.Sleep(ctx, retryWait); serr != nil {
				return // context cancelled
			}
			next, err = m.refresh(ctx, current)
			if err == nil {
				break
			}
			if retryWait < retryMaxWait {
				retryWait *= 2
				if retryWait > retryMaxWait {
					retryWait = retryMaxWait
				}
			}
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
