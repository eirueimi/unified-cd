package agent

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
)

const (
	tokenRefreshLeadTime    = 15 * time.Minute
	maxCredentialJitter     = 5 * time.Minute
	credentialRetryAttempts = 3
)

var (
	syncCredentialDirectoryFn = syncCredentialDirectory
)

type credentialRequestError struct {
	status     int
	retryable  bool
	retryAfter time.Duration
}

func (e *credentialRequestError) Error() string {
	if e.status != 0 {
		return fmt.Sprintf("credential request returned http %d", e.status)
	}
	return "credential request failed"
}

type persistedCredential struct {
	Version          int       `json:"version"`
	AgentID          string    `json:"agentId"`
	RefreshToken     string    `json:"refreshToken"`
	RefreshExpiresAt time.Time `json:"refreshExpiresAt"`
}

// CredentialManagerConfig configures VM enrollment and refresh credentials.
// The refresh credential file must be distinct from the one-time enrollment
// token file so the latter can remain a separately delivered secret.
type CredentialManagerConfig struct {
	Server              string
	AgentID             string
	EnrollmentTokenFile string
	EnrollmentToken     string
	CredentialFile      string
	HTTPClient          *http.Client
	Now                 func() time.Time
	Jitter              func() time.Duration
}

// CredentialManager obtains short-lived access tokens and persists only the
// rotated VM refresh credential. Its mutex serializes network exchanges so a
// burst of callers never consumes a refresh credential more than once.
type CredentialManager struct {
	server              string
	agentID             string
	enrollmentTokenFile string
	enrollmentToken     string
	credentialFile      string
	http                *http.Client
	now                 func() time.Time
	jitter              func() time.Duration
	refreshJitter       time.Duration
	sleep               func(context.Context, time.Duration) error

	mu            sync.Mutex
	loaded        bool
	bootstrapDone bool // true once the first enroll/refresh resolution has run
	refresh       persistedCredential
	accessToken   string
	accessExpires time.Time
	persist       func(string, persistedCredential) error
}

func NewCredentialManager(cfg CredentialManagerConfig) *CredentialManager {
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	jitter := cfg.Jitter
	if jitter == nil {
		jitter = defaultCredentialJitter
	}
	return &CredentialManager{
		server: cfg.Server, agentID: cfg.AgentID, enrollmentTokenFile: cfg.EnrollmentTokenFile, enrollmentToken: cfg.EnrollmentToken,
		credentialFile: cfg.CredentialFile, http: httpClient, now: now, jitter: jitter, refreshJitter: jitter(),
		persist: writeCredentialFile, sleep: sleepContext,
	}
}

// Token returns a cached access token while it has more than the refresh lead
// time remaining. Otherwise it atomically refreshes or performs first-time
// enrollment and writes the replacement refresh credential before use.
func (m *CredentialManager) Token(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.accessToken != "" && m.now().Add(tokenRefreshLeadTime+m.refreshJitter).Before(m.accessExpires) {
		return m.accessToken, nil
	}
	if err := m.loadRefreshCredential(); err != nil {
		return "", err
	}

	var response api.AgentTokenResponse
	var err error
	enrollTok, tokErr := m.enrollmentTokenValue()
	switch {
	case !m.bootstrapDone && enrollTok != "":
		// An explicit enrollment token means "(re-)enroll" — prefer it even when
		// a credential already exists, so authorized-label changes take effect.
		response, err = m.exchangeWithRetry(ctx, "/api/v1/agents/enroll", enrollTok)
		if err != nil {
			var reqErr *credentialRequestError
			if errors.As(err, &reqErr) && reqErr.status == http.StatusUnauthorized && m.refresh.RefreshToken != "" {
				// The token is definitively rejected (expired/already consumed),
				// but we hold a working credential — keep running on it rather
				// than bricking. Labels are not updated in this case.
				slog.Warn("enrollment token rejected (expired or already consumed); continuing with the existing credential", "agentId", m.agentID)
				response, err = m.exchangeWithRetry(ctx, "/api/v1/agents/token/refresh", m.refresh.RefreshToken)
			}
		}
	case m.refresh.RefreshToken != "":
		response, err = m.exchangeWithRetry(ctx, "/api/v1/agents/token/refresh", m.refresh.RefreshToken)
	case tokErr != nil:
		return "", tokErr
	default:
		return "", fmt.Errorf("agent credentials are required")
	}
	if err != nil {
		return "", err
	}
	m.bootstrapDone = true
	// Adopt the agent ID from the response when --id was omitted; otherwise
	// assert the server agrees with the configured identity.
	if m.agentID == "" {
		m.agentID = response.AgentID
	} else if response.AgentID != m.agentID {
		return "", fmt.Errorf("credential response agent ID %q does not match configured agent ID %q", response.AgentID, m.agentID)
	}
	if response.AgentID == "" || response.AccessToken == "" || response.RefreshToken == "" || response.RefreshExpiresAt == nil {
		return "", fmt.Errorf("credential response is invalid")
	}

	next := persistedCredential{Version: 1, AgentID: response.AgentID, RefreshToken: response.RefreshToken, RefreshExpiresAt: response.RefreshExpiresAt.UTC()}
	// Do not expose the new access token until its paired refresh credential is
	// durable. Otherwise a process crash could strand the agent after rotation.
	if err := m.persist(m.credentialFile, next); err != nil {
		return "", fmt.Errorf("persist agent credentials: %w", err)
	}
	m.refresh = next
	m.loaded = true
	m.accessToken = response.AccessToken
	m.accessExpires = response.AccessExpiresAt
	return m.accessToken, nil
}

// enrollmentTokenValue returns the explicitly-configured one-time enrollment
// token (inline value preferred over file), or "" when none is configured.
func (m *CredentialManager) enrollmentTokenValue() (string, error) {
	if strings.TrimSpace(m.enrollmentToken) != "" {
		return strings.TrimSpace(m.enrollmentToken), nil
	}
	if m.enrollmentTokenFile != "" {
		v, err := readSecretFile(m.enrollmentTokenFile)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(v), nil
	}
	return "", nil
}

// EnsureIdentity resolves the agent's canonical ID before the run loop starts.
// With --id set it is returned as-is. With --id omitted it is adopted from the
// persisted credential (no network) when one exists, or by performing the
// first enrollment when there is none.
func (m *CredentialManager) EnsureIdentity(ctx context.Context) (string, error) {
	m.mu.Lock()
	if m.agentID != "" {
		id := m.agentID
		m.mu.Unlock()
		return id, nil
	}
	err := m.loadRefreshCredential()
	id := m.agentID
	m.mu.Unlock()
	if err != nil {
		return "", err
	}
	if id != "" {
		return id, nil // adopted from the persisted credential, no network
	}
	// No credential and no configured ID → must enroll to learn the identity.
	if _, err := m.Token(ctx); err != nil {
		return "", err
	}
	m.mu.Lock()
	id = m.agentID
	m.mu.Unlock()
	return id, nil
}

func (m *CredentialManager) loadRefreshCredential() error {
	if m.loaded {
		return nil
	}
	if m.credentialFile == "" {
		m.loaded = true
		return nil
	}
	credential, err := readCredentialFile(m.credentialFile)
	if err == nil {
		if semanticErr := m.validateCredential(credential); semanticErr == nil {
			if err := m.removeStaleCredentialBackup(); err != nil {
				return err
			}
			return m.useCredential(credential)
		} else {
			err = semanticErr
		}
	}
	backup, backupErr := readCredentialFile(credentialBackupPath(m.credentialFile))
	if backupErr == nil {
		if !os.IsNotExist(err) {
			if removeErr := os.Remove(m.credentialFile); removeErr != nil {
				return fmt.Errorf("remove invalid credential file: %w", removeErr)
			}
		}
		if restoreErr := os.Rename(credentialBackupPath(m.credentialFile), m.credentialFile); restoreErr != nil {
			return fmt.Errorf("restore credential backup: %w", restoreErr)
		}
		if syncErr := syncCredentialDirectoryFn(filepath.Dir(m.credentialFile)); syncErr != nil {
			return fmt.Errorf("restore credential backup: %w", syncErr)
		}
		return m.useCredential(backup)
	}
	if !os.IsNotExist(backupErr) {
		return fmt.Errorf("read credential backup: %w", backupErr)
	}
	if os.IsNotExist(err) && (m.enrollmentTokenFile != "" || m.enrollmentToken != "") {
		m.loaded = true
		return nil
	}
	return err
}

func (m *CredentialManager) useCredential(credential persistedCredential) error {
	if err := m.validateCredential(credential); err != nil {
		return err
	}
	if m.agentID == "" {
		m.agentID = credential.AgentID
	}
	m.refresh, m.loaded = credential, true
	return nil
}

func (m *CredentialManager) validateCredential(credential persistedCredential) error {
	if m.agentID != "" && credential.AgentID != m.agentID {
		return fmt.Errorf("credential file agent ID %q does not match configured agent ID %q", credential.AgentID, m.agentID)
	}
	if !credential.RefreshExpiresAt.After(m.now()) {
		return fmt.Errorf("agent refresh credential has expired")
	}
	return nil
}

func (m *CredentialManager) removeStaleCredentialBackup() error {
	backupPath := credentialBackupPath(m.credentialFile)
	if _, err := os.Lstat(backupPath); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect credential backup: %w", err)
	}
	if err := os.Remove(backupPath); err != nil {
		return fmt.Errorf("remove stale credential backup: %w", err)
	}
	if err := syncCredentialDirectoryFn(filepath.Dir(m.credentialFile)); err != nil {
		return fmt.Errorf("remove stale credential backup: %w", err)
	}
	return nil
}

func defaultCredentialJitter() time.Duration {
	value, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(maxCredentialJitter)))
	if err != nil {
		return time.Minute
	}
	return time.Duration(value.Int64()) + time.Nanosecond
}

func (m *CredentialManager) exchangeWithRetry(ctx context.Context, path, credential string) (api.AgentTokenResponse, error) {
	for attempt := 0; attempt < credentialRetryAttempts; attempt++ {
		response, err := m.exchange(ctx, path, credential)
		if err == nil {
			return response, nil
		}
		var requestErr *credentialRequestError
		if !errors.As(err, &requestErr) || !requestErr.retryable || attempt == credentialRetryAttempts-1 {
			return api.AgentTokenResponse{}, err
		}
		delay := requestErr.retryAfter
		if delay <= 0 {
			delay = time.Duration(attempt+1) * 100 * time.Millisecond
		}
		if err := m.sleep(ctx, delay); err != nil {
			return api.AgentTokenResponse{}, err
		}
	}
	return api.AgentTokenResponse{}, fmt.Errorf("credential request failed")
}

func (m *CredentialManager) exchange(ctx context.Context, path, credential string) (api.AgentTokenResponse, error) {
	var result api.AgentTokenResponse
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.server+path, bytes.NewReader(nil))
	if err != nil {
		return result, fmt.Errorf("create credential request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	resp, err := m.http.Do(req)
	if err != nil {
		return result, &credentialRequestError{retryable: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return result, &credentialRequestError{status: resp.StatusCode, retryable: resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError, retryAfter: retryAfter(resp)}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return result, fmt.Errorf("credential response is invalid")
	}
	return result, nil
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func retryAfter(resp *http.Response) time.Duration {
	value := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		if delay := time.Until(when); delay > 0 {
			return delay
		}
	}
	return 0
}

func readSecretFile(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("agent credentials are required")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read enrollment token file: %w", err)
	}
	if secret := strings.TrimSpace(string(b)); secret != "" {
		return secret, nil
	}
	return "", fmt.Errorf("enrollment token file is empty")
}

func readCredentialFile(path string) (persistedCredential, error) {
	var credential persistedCredential
	if err := validateCredentialDirectory(filepath.Dir(path)); err != nil {
		return credential, err
	}
	b, info, err := readProtectedCredentialFile(path)
	if err != nil {
		return credential, err
	}
	if !info.Mode().IsRegular() {
		return credential, fmt.Errorf("credential file is not a regular file")
	}
	if err := validateCredentialFile(path, info); err != nil {
		return credential, err
	}
	if err := json.Unmarshal(b, &credential); err != nil {
		return credential, fmt.Errorf("parse credential file: %w", err)
	}
	if credential.Version != 1 || credential.AgentID == "" || credential.RefreshToken == "" || credential.RefreshExpiresAt.IsZero() {
		return credential, fmt.Errorf("credential file is invalid")
	}
	return credential, nil
}

func writeCredentialFile(path string, credential persistedCredential) error {
	if path == "" {
		return fmt.Errorf("credential file is required")
	}
	if credential.Version != 1 || credential.AgentID == "" || credential.RefreshToken == "" || credential.RefreshExpiresAt.IsZero() {
		return fmt.Errorf("credential is invalid")
	}
	dir := filepath.Dir(path)
	if err := validateCredentialDirectory(dir); err != nil {
		return err
	}
	b, err := json.Marshal(credential)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".credentials-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	backupPath := credentialBackupPath(path)
	if _, err := os.Lstat(backupPath); err == nil {
		return fmt.Errorf("credential backup recovery is required")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := protectCredentialFile(tmpPath, tmp); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	hadPrevious := false
	if _, err := os.Lstat(path); err == nil {
		if err := os.Rename(path, backupPath); err != nil {
			return err
		}
		hadPrevious = true
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		if hadPrevious {
			_ = os.Rename(backupPath, path)
		}
		return err
	}
	if err := syncCredentialDirectoryFn(dir); err != nil {
		if hadPrevious {
			if restoreErr := os.Rename(backupPath, path); restoreErr == nil {
				_ = syncCredentialDirectoryFn(dir)
			}
		}
		return err
	}
	if hadPrevious {
		if err := os.Remove(backupPath); err != nil {
			return err
		}
		if err := syncCredentialDirectoryFn(dir); err != nil {
			return err
		}
	}
	return nil
}

func credentialBackupPath(path string) string { return path + ".previous" }
