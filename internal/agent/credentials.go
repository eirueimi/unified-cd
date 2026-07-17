package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
)

const tokenRefreshLeadTime = 15 * time.Minute

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
	credentialFile      string
	http                *http.Client
	now                 func() time.Time
	jitter              func() time.Duration

	mu            sync.Mutex
	loaded        bool
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
		jitter = func() time.Duration { return 0 }
	}
	return &CredentialManager{
		server: cfg.Server, agentID: cfg.AgentID, enrollmentTokenFile: cfg.EnrollmentTokenFile,
		credentialFile: cfg.CredentialFile, http: httpClient, now: now, jitter: jitter,
		persist: writeCredentialFile,
	}
}

// Token returns a cached access token while it has more than the refresh lead
// time remaining. Otherwise it atomically refreshes or performs first-time
// enrollment and writes the replacement refresh credential before use.
func (m *CredentialManager) Token(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.accessToken != "" && m.now().Add(tokenRefreshLeadTime+m.jitter()).Before(m.accessExpires) {
		return m.accessToken, nil
	}
	if err := m.loadRefreshCredential(); err != nil {
		return "", err
	}

	var response api.AgentTokenResponse
	var err error
	if m.refresh.RefreshToken != "" {
		response, err = m.exchange(ctx, "/api/v1/agents/token/refresh", m.refresh.RefreshToken)
	} else {
		enrollment, readErr := readSecretFile(m.enrollmentTokenFile)
		if readErr != nil {
			return "", readErr
		}
		response, err = m.exchange(ctx, "/api/v1/agents/enroll", enrollment)
	}
	if err != nil {
		return "", err
	}
	if response.AgentID != m.agentID || response.AccessToken == "" || response.RefreshToken == "" || response.RefreshExpiresAt == nil {
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

func (m *CredentialManager) loadRefreshCredential() error {
	if m.loaded {
		return nil
	}
	m.loaded = true
	if m.credentialFile == "" {
		return nil
	}
	credential, err := readCredentialFile(m.credentialFile)
	if err == nil {
		if credential.AgentID != m.agentID {
			return fmt.Errorf("credential file agent ID does not match configured agent ID")
		}
		if !credential.RefreshExpiresAt.After(m.now()) {
			return fmt.Errorf("agent refresh credential has expired")
		}
		m.refresh = credential
		return nil
	}
	if os.IsNotExist(err) && m.enrollmentTokenFile != "" {
		return nil
	}
	return err
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
		return result, fmt.Errorf("credential request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return result, fmt.Errorf("credential request returned http %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return result, fmt.Errorf("credential response is invalid")
	}
	return result, nil
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
	info, err := os.Lstat(path)
	if err != nil {
		return credential, err
	}
	if !info.Mode().IsRegular() {
		return credential, fmt.Errorf("credential file is not a regular file")
	}
	if err := validateCredentialFile(path, info); err != nil {
		return credential, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
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
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	if err := syncCredentialDirectory(dir); err != nil {
		return err
	}
	return nil
}
