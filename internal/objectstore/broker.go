package objectstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// brokerRequestTimeout bounds a single store-credentials exchange with the
// controller. This is a network call, unlike NewFileCredentials' local file
// read, so it needs a real deadline rather than relying on the caller's
// context alone — an unresponsive controller must not hang an artifact
// upload indefinitely.
const brokerRequestTimeout = 30 * time.Second

// BrokerConfig fetches an S3Config from the controller's
// POST /api/v1/store-credentials broker endpoint (see
// docs/superpowers/specs/2026-08-26-sidecar-credential-delivery-design.md
// §5.6): it presents the projected ServiceAccount token at tokenFile and
// gets back the endpoint/bucket to talk to plus a credential to sign
// requests with.
//
// Endpoint/Bucket/Region/UseSSL are resolved SYNCHRONOUSLY, by one fetch
// here, rather than deferred into the returned credentials.Provider the way
// NewFileCredentials defers its own re-reads: NewS3ObjectStore needs the
// endpoint and bucket up front to construct a minio.Client and check the
// bucket exists, before a single request is signed, so there is no later
// point to learn them at. The access key/secret/session token DO refresh
// through the provider seam on every future signing pass whose credential
// has expired — see brokerProvider.IsExpired.
func BrokerConfig(ctx context.Context, brokerURL, tokenFile string) (S3Config, error) {
	p := &brokerProvider{
		brokerURL: strings.TrimRight(brokerURL, "/"),
		tokenFile: tokenFile,
		http:      &http.Client{Timeout: brokerRequestTimeout},
	}
	resp, err := p.fetch(ctx)
	if err != nil {
		return S3Config{}, fmt.Errorf("store credentials broker %s: %w", brokerURL, err)
	}
	p.mu.Lock()
	p.cached = resp
	p.haveCached = true
	p.mu.Unlock()
	return S3Config{
		Endpoint: resp.Endpoint,
		Bucket:   resp.Bucket,
		Region:   resp.Region,
		UseSSL:   resp.UseSSL,
		Creds:    credentials.New(p),
	}, nil
}

// brokerProvider is a credentials.Provider backed by the controller's
// store-credentials broker. It caches the last response and only re-fetches
// when IsExpired reports the cached credential has expired — mirroring
// fileProvider's IsExpired-then-Retrieve split (see credfile.go), except the
// change signal here is the response's own ExpiresAt rather than a content
// hash, since there is no local file to compare against.
type brokerProvider struct {
	brokerURL string
	tokenFile string
	http      *http.Client

	// mu guards the fields below, for the same reason fileProvider's does:
	// minio-go can call a Provider from whichever goroutine is signing a
	// request, and the sidecar can have an upload and a download in flight
	// at once.
	mu         sync.Mutex
	cached     api.StoreCredentialsResponse
	haveCached bool
}

// IsExpired reports true before the first successful fetch, and again once
// the cached response's ExpiresAt has passed. A zero ExpiresAt — today's
// passthrough credential, which never expires on its own (see
// api.StoreCredentialsResponse's doc comment) — is treated as never
// expiring once fetched: there is nothing to refresh toward, so refetching
// on a timer would only add unnecessary controller load and a network
// dependency the credential does not need. A future scoped/short-lived
// credential arrives with a real ExpiresAt and this same check starts
// refreshing it automatically, with no change needed here.
func (p *brokerProvider) IsExpired() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.isExpiredLocked()
}

// isExpiredLocked is IsExpired's logic, callable from retrieve while it
// already holds p.mu — see retrieve's doc comment for why that matters.
func (p *brokerProvider) isExpiredLocked() bool {
	if !p.haveCached {
		return true
	}
	return !p.cached.ExpiresAt.IsZero() && !time.Now().Before(p.cached.ExpiresAt)
}

// Retrieve implements the deprecated single-value Provider method that
// RetrieveWithCredContext delegates to; kept because credentials.Provider
// still requires it (mirrors fileProvider.Retrieve).
func (p *brokerProvider) Retrieve() (credentials.Value, error) {
	return p.retrieve(context.Background())
}

// RetrieveWithCredContext ignores cc: the broker fetch builds its own
// short-lived http.Client and does not need the STS/IAM-oriented fields
// CredContext carries (mirrors fileProvider.RetrieveWithCredContext).
func (p *brokerProvider) RetrieveWithCredContext(_ *credentials.CredContext) (credentials.Value, error) {
	return p.retrieve(context.Background())
}

// retrieve serves the cached response when it is still fresh, and only
// calls the controller when it is not.
//
// This matters because minio-go's credentials.New sets forceRefresh: true
// unconditionally, so the FIRST Credentials.Get() a caller ever makes always
// calls Provider.Retrieve(), regardless of what IsExpired() would have said
// (see the vendored pkg/credentials/credentials.go's Credentials doc
// comment). BrokerConfig already paid for one fetch to learn
// Endpoint/Bucket before a minio.Client could even be constructed — without
// this check, that response would be thrown away and immediately re-fetched
// by minio-go's forced call, doubling the controller round trips every
// sidecar container pays on cold start for no benefit.
func (p *brokerProvider) retrieve(ctx context.Context) (credentials.Value, error) {
	p.mu.Lock()
	if !p.isExpiredLocked() {
		resp := p.cached
		p.mu.Unlock()
		return credentials.Value{
			AccessKeyID: resp.AccessKey, SecretAccessKey: resp.SecretKey,
			SessionToken: resp.Token, SignerType: credentials.SignatureV4,
		}, nil
	}
	p.mu.Unlock()

	resp, err := p.fetch(ctx)
	if err != nil {
		return credentials.Value{}, err
	}
	p.mu.Lock()
	p.cached = resp
	p.haveCached = true
	p.mu.Unlock()
	return credentials.Value{
		AccessKeyID:     resp.AccessKey,
		SecretAccessKey: resp.SecretKey,
		SessionToken:    resp.Token,
		SignerType:      credentials.SignatureV4,
	}, nil
}

// fetch reads the current projected token from tokenFile (re-read every
// call, not cached — the kubelet refreshes the file in place, and a stale
// in-memory copy would eventually be a token the API server has already
// rotated past) and exchanges it for store credentials.
func (p *brokerProvider) fetch(ctx context.Context) (api.StoreCredentialsResponse, error) {
	tokenBytes, err := os.ReadFile(p.tokenFile)
	if err != nil {
		return api.StoreCredentialsResponse{}, fmt.Errorf("read broker token file %s: %w", p.tokenFile, err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		return api.StoreCredentialsResponse{}, fmt.Errorf("broker token file %s is empty", p.tokenFile)
	}

	reqCtx, cancel := context.WithTimeout(ctx, brokerRequestTimeout)
	defer cancel()
	body, err := json.Marshal(api.StoreCredentialsRequest{Token: token})
	if err != nil {
		return api.StoreCredentialsResponse{}, fmt.Errorf("encode store credentials request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, p.brokerURL+"/api/v1/store-credentials", bytes.NewReader(body))
	if err != nil {
		return api.StoreCredentialsResponse{}, fmt.Errorf("create store credentials request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpResp, err := p.http.Do(httpReq)
	if err != nil {
		return api.StoreCredentialsResponse{}, fmt.Errorf("request store credentials: %w", err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode != http.StatusOK {
		limited := io.LimitReader(httpResp.Body, 4096)
		msg, _ := io.ReadAll(limited)
		return api.StoreCredentialsResponse{}, fmt.Errorf("store credentials request failed: %s: %s", httpResp.Status, strings.TrimSpace(string(msg)))
	}
	var resp api.StoreCredentialsResponse
	if err := json.NewDecoder(io.LimitReader(httpResp.Body, 1<<20)).Decode(&resp); err != nil {
		return api.StoreCredentialsResponse{}, fmt.Errorf("decode store credentials response: %w", err)
	}
	return resp, nil
}
