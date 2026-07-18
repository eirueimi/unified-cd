package k8sagent

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
)

const (
	kubernetesCredentialRetryAttempts = 3
	kubernetesTokenRefreshLeadTime    = 15 * time.Minute
	maxKubernetesCredentialJitter     = 5 * time.Minute
)

// KubernetesCredentialSource exchanges the current projected ServiceAccount
// token for a short-lived controller access token. It intentionally retains no
// refresh credential and writes no credential material to disk.
type KubernetesCredentialSource struct {
	server                  string
	policy                  string
	serviceAccountTokenFile string
	requestedLabels         []string
	requestedCapabilities   []string
	http                    *http.Client
	now                     func() time.Time
	jitter                  func() time.Duration
	refreshJitter           time.Duration
	sleep                   func(context.Context, time.Duration) error

	mu              sync.Mutex
	accessToken     string
	accessExpiresAt time.Time
	agentID         string
	labels          []string
}

// KubernetesCredentialSourceConfig supplies the non-secret settings for a
// Kubernetes workload identity exchange.
type KubernetesCredentialSourceConfig struct {
	Server                  string
	Policy                  string
	ServiceAccountTokenFile string
	Labels                  []string
	Capabilities            []string
	HTTPClient              *http.Client
	Now                     func() time.Time
	Jitter                  func() time.Duration
}

// NewKubernetesCredentialSource creates a TokenSource backed by a projected
// Kubernetes ServiceAccount token.
func NewKubernetesCredentialSource(cfg KubernetesCredentialSourceConfig) *KubernetesCredentialSource {
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
		jitter = defaultKubernetesCredentialJitter
	}
	return &KubernetesCredentialSource{
		server: cfg.Server, policy: cfg.Policy, serviceAccountTokenFile: cfg.ServiceAccountTokenFile,
		requestedLabels: append([]string(nil), cfg.Labels...), requestedCapabilities: append([]string(nil), cfg.Capabilities...),
		http: httpClient, now: now, jitter: jitter, refreshJitter: jitter(), sleep: sleepKubernetesCredential,
	}
}

// Token returns the active access token while it has more than the refresh
// lead time remaining. Otherwise it exchanges the current projected JWT.
func (s *KubernetesCredentialSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.accessToken != "" && s.now().Add(kubernetesTokenRefreshLeadTime+s.refreshJitter).Before(s.accessExpiresAt) {
		return s.accessToken, nil
	}

	response, err := s.exchangeWithRetry(ctx)
	if err != nil {
		return "", err
	}
	if response.AgentID == "" || response.AccessToken == "" || !response.AccessExpiresAt.After(s.now()) {
		return "", fmt.Errorf("kubernetes enrollment response is invalid")
	}
	s.accessToken = response.AccessToken
	s.accessExpiresAt = response.AccessExpiresAt
	s.agentID = response.AgentID
	s.labels = append(s.labels[:0], response.Labels...)
	return s.accessToken, nil
}

// Invalidate discards the cached access credential after the controller
// rejects it. The next Token call performs a fresh workload enrollment.
func (s *KubernetesCredentialSource) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accessToken = ""
	s.accessExpiresAt = time.Time{}
}

// AgentID returns the canonical ID assigned by the controller after a
// successful exchange.
func (s *KubernetesCredentialSource) AgentID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agentID
}

// Labels returns the labels authorized by the controller after a successful
// exchange.
func (s *KubernetesCredentialSource) Labels() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.labels...)
}

func (s *KubernetesCredentialSource) exchangeWithRetry(ctx context.Context) (api.AgentTokenResponse, error) {
	for attempt := 0; attempt < kubernetesCredentialRetryAttempts; attempt++ {
		token, err := readProjectedServiceAccountToken(s.serviceAccountTokenFile)
		if err != nil {
			return api.AgentTokenResponse{}, err
		}
		response, err := s.exchange(ctx, token)
		if err == nil {
			return response, nil
		}
		if !isRetryableKubernetesExchange(err) || attempt == kubernetesCredentialRetryAttempts-1 {
			return api.AgentTokenResponse{}, err
		}
		if err := s.sleep(ctx, time.Duration(attempt+1)*100*time.Millisecond); err != nil {
			return api.AgentTokenResponse{}, err
		}
	}
	return api.AgentTokenResponse{}, fmt.Errorf("kubernetes enrollment request failed")
}

func defaultKubernetesCredentialJitter() time.Duration {
	value, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(maxKubernetesCredentialJitter)))
	if err != nil {
		return time.Minute
	}
	return time.Duration(value.Int64()) + time.Nanosecond
}

type kubernetesExchangeError struct{ retryable bool }

func (e *kubernetesExchangeError) Error() string { return "kubernetes enrollment request failed" }

func isRetryableKubernetesExchange(err error) bool {
	requestErr, ok := err.(*kubernetesExchangeError)
	return ok && requestErr.retryable
}

func (s *KubernetesCredentialSource) exchange(ctx context.Context, token string) (api.AgentTokenResponse, error) {
	var response api.AgentTokenResponse
	body, err := json.Marshal(api.AgentEnrollRequest{Provider: "kubernetes", Policy: s.policy, Labels: s.requestedLabels, Capabilities: s.requestedCapabilities})
	if err != nil {
		return response, fmt.Errorf("encode kubernetes enrollment request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.server+"/api/v1/agents/enroll", bytes.NewReader(body))
	if err != nil {
		return response, fmt.Errorf("create kubernetes enrollment request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return response, &kubernetesExchangeError{retryable: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return response, &kubernetesExchangeError{retryable: resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&response); err != nil {
		return response, fmt.Errorf("kubernetes enrollment response is invalid")
	}
	return response, nil
}

func readProjectedServiceAccountToken(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("service account token file is required")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read service account token file: %w", err)
	}
	if token := strings.TrimSpace(string(contents)); token != "" {
		return token, nil
	}
	return "", fmt.Errorf("service account token file is empty")
}

func sleepKubernetesCredential(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
