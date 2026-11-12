package gittoken

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Issuer creates GitHub App token sources that share one in-memory token
// cache and one installation-ID cache. It is safe for concurrent use.
type Issuer struct {
	// OnRequest, when set, is called once per POST access_tokens with the
	// credential name and the request's error (nil on success). Set it
	// before the first Token call.
	OnRequest func(credential string, err error)

	client *http.Client
	now    func() time.Time

	mu            sync.Mutex
	tokens        map[string]*tokenEntry
	installations map[string]installationEntry
}

// tokenEntry holds one cached token. sem (capacity 1) serializes misses for
// its key; tok and ok are read and written only while holding sem.
type tokenEntry struct {
	sem chan struct{}
	tok Token
	ok  bool
}

type installationEntry struct {
	id      int64
	expires time.Time
}

// NewIssuer returns an Issuer. A nil client means
// &http.Client{Timeout: DefaultHTTPTimeout} (default transport, so proxy
// settings and the system CA pool apply); a nil now means time.Now.
func NewIssuer(client *http.Client, now func() time.Time) *Issuer {
	if client == nil {
		client = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	if now == nil {
		now = time.Now
	}
	return &Issuer{
		client:        client,
		now:           now,
		tokens:        map[string]*tokenEntry{},
		installations: map[string]installationEntry{},
	}
}

// Source returns a TokenSource for app. A cached token is reused while more
// than minRemaining of its lifetime is left.
func (i *Issuer) Source(app GitHubApp, minRemaining time.Duration) TokenSource {
	return &appSource{issuer: i, app: app, minRemaining: minRemaining}
}

type appSource struct {
	issuer       *Issuer
	app          GitHubApp
	minRemaining time.Duration
}

// normalized is a request in canonical form. repositories is nil for the
// whole installation; otherwise it is sorted.
type normalized struct {
	owner        string
	repositories []string
	permissions  map[string]string
}

func normalize(req TokenRequest) normalized {
	n := normalized{owner: strings.ToLower(req.Owner)}
	if len(req.Repositories) > 0 && !slices.Contains(req.Repositories, "*") {
		n.repositories = slices.Clone(req.Repositories)
		slices.Sort(n.repositories)
		n.repositories = slices.Compact(n.repositories)
	}
	if len(req.Permissions) > 0 {
		n.permissions = req.Permissions
	}
	return n
}

// cacheKey is Name + SHA-256 of a canonical JSON of the credential config
// and the normalized request. encoding/json sorts map keys.
func cacheKey(app GitHubApp, apiURL string, n normalized) string {
	repos := n.repositories
	if repos == nil {
		repos = []string{"*"}
	}
	b, _ := json.Marshal(struct {
		AppID          int64             `json:"appId"`
		InstallationID int64             `json:"installationId"`
		APIURL         string            `json:"apiUrl"`
		KeyRef         string            `json:"keyRef"`
		Owner          string            `json:"owner"`
		Repositories   []string          `json:"repositories"`
		Permissions    map[string]string `json:"permissions"`
	}{app.AppID, app.InstallationID, apiURL, app.KeyRef, n.owner, repos, n.permissions})
	sum := sha256.Sum256(b)
	return app.Name + "|" + hex.EncodeToString(sum[:])
}

func (s *appSource) Token(ctx context.Context, req TokenRequest) (Token, error) {
	app := s.app
	if app.AppID <= 0 {
		return Token{}, fmt.Errorf("github app %q: app ID must be positive", app.Name)
	}
	if app.PrivateKey == nil {
		return Token{}, fmt.Errorf("github app %q: no private key loader", app.Name)
	}
	if req.Owner == "" {
		return Token{}, fmt.Errorf("github app %q: token request has no repository owner", app.Name)
	}
	api, err := newGitHubAPI(s.issuer.client, app)
	if err != nil {
		return Token{}, err
	}
	n := normalize(req)
	if len(n.repositories) > maxRepositories {
		return Token{}, fmt.Errorf("github app %q: %d repositories requested; GitHub accepts at most %d repositories per token", app.Name, len(n.repositories), maxRepositories)
	}

	entry := s.issuer.entry(cacheKey(app, api.base.String(), n))
	select {
	case entry.sem <- struct{}{}:
	case <-ctx.Done():
		return Token{}, ctx.Err()
	}
	defer func() { <-entry.sem }()

	if entry.ok && entry.tok.ExpiresAt.Sub(s.issuer.now()) > s.minRemaining {
		return entry.tok, nil
	}
	tok, err := s.issue(ctx, api, n)
	if err != nil {
		return Token{}, err
	}
	entry.tok, entry.ok = tok, true
	return tok, nil
}

func (i *Issuer) entry(key string) *tokenEntry {
	i.mu.Lock()
	defer i.mu.Unlock()
	e := i.tokens[key]
	if e == nil {
		e = &tokenEntry{sem: make(chan struct{}, 1)}
		i.tokens[key] = e
	}
	return e
}

// issue performs a cache miss: load the key, sign a JWT, resolve the
// installation, expand patterns and create the token.
func (s *appSource) issue(ctx context.Context, api *githubAPI, n normalized) (Token, error) {
	app := s.app
	pemBytes, err := app.PrivateKey(ctx)
	if err != nil {
		return Token{}, fmt.Errorf("github app %q: load private key: %w", app.Name, err)
	}
	key, err := parsePrivateKey(pemBytes)
	if err != nil {
		return Token{}, fmt.Errorf("github app %q: %w", app.Name, err)
	}
	appJWT, err := signAppJWT(key, app.AppID, s.issuer.now())
	if err != nil {
		return Token{}, fmt.Errorf("github app %q: %w", app.Name, err)
	}

	var concrete []string
	hasPattern := false
	for _, r := range n.repositories {
		if isPrefixPattern(r) {
			hasPattern = true
		} else {
			concrete = append(concrete, r)
		}
	}

	installationID := app.InstallationID
	if installationID == 0 {
		if installationID, err = s.issuer.installation(ctx, api, app, appJWT, n.owner, concrete); err != nil {
			return Token{}, err
		}
	}

	repositories := n.repositories
	if hasPattern {
		// The listing token is used once here and never cached or returned.
		listing, err := s.createToken(ctx, api, appJWT, installationID, nil, map[string]string{"metadata": "read"})
		if err != nil {
			return Token{}, err
		}
		names, err := api.listRepositories(ctx, listing.Value)
		if err != nil {
			return Token{}, err
		}
		if repositories, err = expandRepositories(app.Name, n.repositories, names); err != nil {
			return Token{}, err
		}
		if len(repositories) > maxRepositories {
			return Token{}, fmt.Errorf("github app %q: %d repositories match; GitHub accepts at most %d repositories per token", app.Name, len(repositories), maxRepositories)
		}
	}
	return s.createToken(ctx, api, appJWT, installationID, repositories, n.permissions)
}

func (s *appSource) createToken(ctx context.Context, api *githubAPI, appJWT string, installationID int64, repositories []string, permissions map[string]string) (Token, error) {
	tok, err := api.createToken(ctx, appJWT, installationID, repositories, permissions)
	if hook := s.issuer.OnRequest; hook != nil {
		hook(s.app.Name, err)
	}
	return tok, err
}

// installation returns the cached installation ID for owner or looks it up.
// The cache key includes the App ID because two Apps installed on the same
// owner have different installations.
func (i *Issuer) installation(ctx context.Context, api *githubAPI, app GitHubApp, appJWT, owner string, concrete []string) (int64, error) {
	key := api.base.String() + "|" + strconv.FormatInt(app.AppID, 10) + "|" + owner
	i.mu.Lock()
	cached, ok := i.installations[key]
	i.mu.Unlock()
	now := i.now()
	if ok && now.Before(cached.expires) {
		return cached.id, nil
	}
	id, err := api.lookupInstallation(ctx, appJWT, owner, concrete)
	if err != nil {
		return 0, err
	}
	i.mu.Lock()
	i.installations[key] = installationEntry{id: id, expires: now.Add(InstallationCacheTTL)}
	i.mu.Unlock()
	return id, nil
}
