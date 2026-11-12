// Package gittokentest provides a fake GitHub API for tests of GitHub App
// installation token issuing.
package gittokentest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// DefaultInstallationID is the installation ID a new FakeGitHub serves.
const DefaultInstallationID int64 = 7890123

// GenerateKeyPEM returns a fresh 2048-bit RSA key and its PEM encoding:
// PKCS#8 ("PRIVATE KEY") when pkcs8 is true, otherwise PKCS#1
// ("RSA PRIVATE KEY").
func GenerateKeyPEM(t testing.TB, pkcs8 bool) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	if !pkcs8 {
		return key, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal PKCS#8 key: %v", err)
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// AccessTokenRequest is one recorded POST /app/installations/{id}/access_tokens.
type AccessTokenRequest struct {
	InstallationID     int64
	Repositories       []string
	Permissions        map[string]string
	HasRepositoriesKey bool
}

// FakeGitHub is a TLS test server implementing the GitHub App endpoints the
// token source uses. Configure its exported fields before the first request.
type FakeGitHub struct {
	URL            string
	InstallationID int64            // installation of every owner and repository
	Repos          []string         // repositories of the installation, in listing order
	TokenTTL       time.Duration    // lifetime of issued tokens
	Now            func() time.Time // clock for expires_at and JWT expiry checks
	// IssueStatus, when non-zero, makes every POST access_tokens fail with
	// this status and IssueMessage as GitHub's message.
	IssueStatus  int
	IssueMessage string
	// UserAccount makes the owner a user: /orgs/{owner}/installation
	// returns 404 and /users/{owner}/installation answers. Otherwise the
	// owner is an organization and /users/{owner}/installation returns 404.
	UserAccount bool

	server *httptest.Server
	pub    *rsa.PublicKey

	mu       sync.Mutex
	n        int
	tokens   map[string]bool // issued token values
	requests []AccessTokenRequest
	paths    []string
	lastJWT  string
}

// NewFakeGitHub starts a fake GitHub that accepts App JWTs signed by the
// private half of pub. The server is closed when the test ends.
func NewFakeGitHub(t testing.TB, pub *rsa.PublicKey) *FakeGitHub {
	t.Helper()
	f := &FakeGitHub{
		InstallationID: DefaultInstallationID,
		TokenTTL:       time.Hour,
		Now:            time.Now,
		pub:            pub,
		tokens:         map[string]bool{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", f.accessTokens)
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", f.installation(func() bool { return true }))
	mux.HandleFunc("GET /orgs/{owner}/installation", f.installation(func() bool { return !f.UserAccount }))
	mux.HandleFunc("GET /users/{owner}/installation", f.installation(func() bool { return f.UserAccount }))
	mux.HandleFunc("GET /installation/repositories", f.listRepositories)
	f.server = httptest.NewTLSServer(f.record(mux))
	f.URL = f.server.URL
	t.Cleanup(f.server.Close)
	return f
}

// SetInstallationID changes the installation ID after requests have been
// served, as a reinstalled App would. It is safe to call between requests.
func (f *FakeGitHub) SetInstallationID(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.InstallationID = id
}

func (f *FakeGitHub) installationID() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.InstallationID
}

// Client returns an HTTP client that trusts the fake's TLS certificate.
func (f *FakeGitHub) Client() *http.Client { return f.server.Client() }

// AccessTokenRequests returns every POST access_tokens received so far.
func (f *FakeGitHub) AccessTokenRequests() []AccessTokenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]AccessTokenRequest(nil), f.requests...)
}

// Paths returns the URL path of every request received so far, in order.
func (f *FakeGitHub) Paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

// LastJWT returns the most recent App JWT presented as a Bearer token.
func (f *FakeGitHub) LastJWT() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastJWT
}

func (f *FakeGitHub) record(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.mu.Unlock()
		if r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
			writeError(w, http.StatusBadRequest, "missing GitHub API headers")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// checkJWT verifies the Bearer App JWT and reports whether it is valid.
func (f *FakeGitHub) checkJWT(w http.ResponseWriter, r *http.Request) bool {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		writeError(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
		return false
	}
	f.mu.Lock()
	f.lastJWT = raw
	f.mu.Unlock()
	tok, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		writeError(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
		return false
	}
	var claims jwt.Claims
	if err := tok.Claims(f.pub, &claims); err != nil {
		writeError(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
		return false
	}
	if err := claims.ValidateWithLeeway(jwt.Expected{Time: f.Now()}, 0); err != nil || claims.Issuer == "" {
		writeError(w, http.StatusUnauthorized, "'Expiration time' claim ('exp') is too far in the future")
		return false
	}
	return true
}

func (f *FakeGitHub) installation(exists func() bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !f.checkJWT(w, r) {
			return
		}
		if !exists() {
			writeError(w, http.StatusNotFound, "Not Found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": f.installationID()})
	}
}

func (f *FakeGitHub) accessTokens(w http.ResponseWriter, r *http.Request) {
	if !f.checkJWT(w, r) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	req := AccessTokenRequest{InstallationID: id}
	if raw, ok := body["repositories"]; ok {
		req.HasRepositoriesKey = true
		_ = json.Unmarshal(raw, &req.Repositories)
	}
	if raw, ok := body["permissions"]; ok {
		_ = json.Unmarshal(raw, &req.Permissions)
	}
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	if f.IssueStatus != 0 {
		writeError(w, f.IssueStatus, f.IssueMessage)
		return
	}
	if id != f.installationID() {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	f.mu.Lock()
	f.n++
	value := fmt.Sprintf("ghs_%d", f.n)
	f.tokens[value] = true
	f.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":      value,
		"expires_at": f.Now().Add(f.TokenTTL).UTC().Format(time.RFC3339),
	})
}

func (f *FakeGitHub) listRepositories(w http.ResponseWriter, r *http.Request) {
	value, ok := strings.CutPrefix(r.Header.Get("Authorization"), "token ")
	f.mu.Lock()
	known := ok && f.tokens[value]
	f.mu.Unlock()
	if !known {
		writeError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if perPage <= 0 || perPage > 100 {
		perPage = 30
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page <= 0 {
		page = 1
	}
	start := min((page-1)*perPage, len(f.Repos))
	end := min(start+perPage, len(f.Repos))
	repos := make([]map[string]any, 0, end-start)
	for _, name := range f.Repos[start:end] {
		repos = append(repos, map[string]any{"name": name, "full_name": "acme/" + name})
	}
	if end < len(f.Repos) {
		w.Header().Set("Link", fmt.Sprintf(`<%s/installation/repositories?per_page=%d&page=%d>; rel="next"`, f.URL, perPage, page+1))
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(f.Repos), "repositories": repos})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"message": message, "documentation_url": "https://docs.github.com/rest"})
}
