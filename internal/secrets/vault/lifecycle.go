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
