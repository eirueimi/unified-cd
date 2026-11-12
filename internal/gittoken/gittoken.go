// Package gittoken issues short-lived git access tokens. Its GitHub App
// source signs an App JWT, finds the installation, and creates installation
// access tokens narrowed to the requested repositories and permissions,
// caching them in memory per replica.
//
// The private key, the App JWT and installation tokens never appear in
// errors: errors from GitHub carry the HTTP status and GitHub's message only.
package gittoken

import (
	"context"
	"time"
)

const (
	// DefaultAPIURL is the GitHub REST API base URL used when GitHubApp.APIURL is empty.
	DefaultAPIURL = "https://api.github.com"
	// DefaultHTTPTimeout is the timeout of the HTTP client NewIssuer creates.
	DefaultHTTPTimeout = 30 * time.Second
	// JobMinRemaining is the lifetime a cached token handed to a job must
	// still have; a cached token with less is re-issued.
	JobMinRemaining = 50 * time.Minute
	// ControllerMinRemaining is the lifetime a cached token used by the
	// controller itself must still have.
	ControllerMinRemaining = 5 * time.Minute
	// InstallationCacheTTL is how long a looked-up installation ID is reused.
	InstallationCacheTTL = time.Hour
)

// maxRepositories is the most repositories GitHub accepts in one token request.
const maxRepositories = 500

// TokenRequest describes the access a token must grant.
type TokenRequest struct {
	Owner        string            // repository owner; selects the installation
	Repositories []string          // names within Owner; trailing-"*" prefix patterns allowed; nil or ["*"] = the installation's
	Permissions  map[string]string // name -> read|write|admin; empty = installation's
}

// Token is an issued access token.
type Token struct {
	Value     string
	ExpiresAt time.Time
}

// TokenSource issues tokens for requests.
type TokenSource interface {
	Token(ctx context.Context, req TokenRequest) (Token, error)
}

// GitHubApp is the configuration of one GitHub App git credential.
type GitHubApp struct {
	Name           string // credential name: cache key prefix and error context
	AppID          int64
	InstallationID int64  // 0 = look up
	APIURL         string // "" = DefaultAPIURL
	KeyRef         string // StoredSecret name; part of the cache fingerprint
	// PrivateKey returns the App's PEM private key (PKCS#1 or PKCS#8). It is
	// called only on a token cache miss.
	PrivateKey func(ctx context.Context) ([]byte, error)
}
