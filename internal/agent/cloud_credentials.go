package agent

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
)

const (
	cloudCredentialRetryAttempts = 3
	cloudTokenRefreshLeadTime    = 15 * time.Minute
	maxCloudCredentialJitter     = 5 * time.Minute
)

// CloudCredentialSourceConfig supplies the non-secret settings for a cloud
// instance-identity exchange.
type CloudCredentialSourceConfig struct {
	Server, Provider, Policy, Audience string
	Labels, Capabilities               []string
	HTTPClient                         *http.Client
	MetadataBaseURL                    string // "" = provider default; tests point it at httptest
	STSHostOverride                    string // tests only: replaces the STS host the agent signs for
	Now                                func() time.Time
	Jitter                             func() time.Duration
}

// CloudCredentialSource exchanges a fresh cloud instance-identity proof (GCP,
// Azure or AWS) for a short-lived controller access token. It retains no
// refresh credential and writes no credential material to disk.
type CloudCredentialSource struct {
	server, provider, policy, audience string
	metadataBaseURL, stsHostOverride   string
	requestedLabels                    []string
	requestedCapabilities              []string
	http                               *http.Client
	metadata                           *http.Client // direct, no proxy, no redirects; used only for metadata fetches
	now                                func() time.Time
	refreshJitter                      time.Duration
	sleep                              func(context.Context, time.Duration) error

	mu              sync.Mutex
	accessToken     string
	accessExpiresAt time.Time
	agentID         string
	labels          []string
}

// NewCloudCredentialSource creates a TokenSource backed by a cloud identity.
func NewCloudCredentialSource(cfg CloudCredentialSourceConfig) (*CloudCredentialSource, error) {
	switch cfg.Provider {
	case "gcp", "azure", "aws":
	default:
		return nil, fmt.Errorf("unsupported cloud provider %q", cfg.Provider)
	}
	if cfg.Policy == "" || cfg.Audience == "" {
		return nil, fmt.Errorf("cloud enrollment requires a policy and an audience")
	}
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
		jitter = defaultCloudCredentialJitter
	}
	return &CloudCredentialSource{
		server: cfg.Server, provider: cfg.Provider, policy: cfg.Policy, audience: cfg.Audience,
		metadataBaseURL: cfg.MetadataBaseURL, stsHostOverride: cfg.STSHostOverride,
		requestedLabels: append([]string(nil), cfg.Labels...), requestedCapabilities: append([]string(nil), cfg.Capabilities...),
		http: httpClient, metadata: newMetadataClient(), now: now, refreshJitter: jitter(), sleep: sleepCloudCredential,
	}, nil
}

// Token returns the active access token while it has more than the refresh
// lead time remaining. Otherwise it fetches a fresh proof and exchanges it.
func (s *CloudCredentialSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.accessToken != "" && s.now().Add(cloudTokenRefreshLeadTime+s.refreshJitter).Before(s.accessExpiresAt) {
		return s.accessToken, nil
	}
	response, err := s.exchangeWithRetry(ctx)
	if err != nil {
		return "", err
	}
	if response.AgentID == "" || response.AccessToken == "" || !response.AccessExpiresAt.After(s.now()) {
		return "", fmt.Errorf("%s enrollment response is invalid", s.provider)
	}
	s.accessToken = response.AccessToken
	s.accessExpiresAt = response.AccessExpiresAt
	s.agentID = response.AgentID
	s.labels = append(s.labels[:0], response.Labels...)
	return s.accessToken, nil
}

// Invalidate discards the cached access credential after the controller
// rejects it. The next Token call performs a fresh enrollment.
func (s *CloudCredentialSource) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accessToken = ""
	s.accessExpiresAt = time.Time{}
}

// AgentID returns the canonical ID assigned by the controller.
func (s *CloudCredentialSource) AgentID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agentID
}

// Labels returns the labels authorized by the controller.
func (s *CloudCredentialSource) Labels() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.labels...)
}

func (s *CloudCredentialSource) exchangeWithRetry(ctx context.Context) (api.AgentTokenResponse, error) {
	for attempt := 0; attempt < cloudCredentialRetryAttempts; attempt++ {
		proof, err := s.fetchProof(ctx) // fresh for every attempt
		if err != nil {
			return api.AgentTokenResponse{}, err
		}
		response, err := s.exchange(ctx, proof)
		if err == nil {
			return response, nil
		}
		if !isRetryableCloudExchange(err) || attempt == cloudCredentialRetryAttempts-1 {
			return api.AgentTokenResponse{}, err
		}
		if err := s.sleep(ctx, time.Duration(attempt+1)*100*time.Millisecond); err != nil {
			return api.AgentTokenResponse{}, err
		}
	}
	return api.AgentTokenResponse{}, fmt.Errorf("%s enrollment request failed", s.provider)
}

func defaultCloudCredentialJitter() time.Duration {
	value, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(maxCloudCredentialJitter)))
	if err != nil {
		return time.Minute
	}
	return time.Duration(value.Int64()) + time.Nanosecond
}

type cloudExchangeError struct {
	provider  string
	retryable bool
}

func (e *cloudExchangeError) Error() string { return e.provider + " enrollment request failed" }

func isRetryableCloudExchange(err error) bool {
	requestErr, ok := err.(*cloudExchangeError)
	return ok && requestErr.retryable
}

func (s *CloudCredentialSource) exchange(ctx context.Context, proof cloudProof) (api.AgentTokenResponse, error) {
	var response api.AgentTokenResponse
	body, err := json.Marshal(api.AgentEnrollRequest{Provider: s.provider, Policy: s.policy, Labels: s.requestedLabels, Capabilities: s.requestedCapabilities, AWS: proof.aws})
	if err != nil {
		return response, fmt.Errorf("encode %s enrollment request: %w", s.provider, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.server+"/api/v1/agents/enroll", bytes.NewReader(body))
	if err != nil {
		return response, fmt.Errorf("create %s enrollment request: %w", s.provider, err)
	}
	if proof.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+proof.bearer)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return response, &cloudExchangeError{provider: s.provider, retryable: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return response, &cloudExchangeError{provider: s.provider, retryable: resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&response); err != nil {
		return response, fmt.Errorf("%s enrollment response is invalid", s.provider)
	}
	return response, nil
}

func sleepCloudCredential(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var _ TokenSource = (*CloudCredentialSource)(nil)
