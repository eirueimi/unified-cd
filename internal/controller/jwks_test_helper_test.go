package controller

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"
)

// testIssuer is a JWKS server plus a signer for tokens it vouches for.
type testIssuer struct {
	t      *testing.T
	Server *httptest.Server
	URL    string // JWKS URL
	Name   string // issuer claim value
	KeyID  string
	key    *rsa.PrivateKey
}

func newTestIssuer(t *testing.T) *testIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	iss := &testIssuer{t: t, Name: "https://issuer.example.com", KeyID: "test-key-1", key: key}
	mux := http.NewServeMux()
	certs := func(w http.ResponseWriter, _ *http.Request) {
		set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: iss.KeyID, Algorithm: string(jose.RS256), Use: "sig"}}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(set)
	}
	mux.HandleFunc("/certs", certs)
	mux.HandleFunc("/{tenant}/discovery/v2.0/keys", certs)
	iss.Server = httptest.NewServer(mux)
	t.Cleanup(iss.Server.Close)
	iss.URL = iss.Server.URL + "/certs"
	return iss
}

// Sign returns a compact RS256 JWT carrying claims. Zero iss/iat/exp are
// filled with valid defaults; pass them explicitly to override.
func (i *testIssuer) Sign(claims map[string]any) string {
	return i.SignWithKeyID(i.KeyID, claims)
}

func (i *testIssuer) SignWithKeyID(kid string, claims map[string]any) string {
	i.t.Helper()
	c := map[string]any{"iss": i.Name, "iat": time.Now().Add(-time.Minute).Unix(), "exp": time.Now().Add(time.Hour).Unix()}
	for k, v := range claims {
		c[k] = v
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: i.key, KeyID: kid}}, (&jose.SignerOptions{}).WithType("JWT"))
	require.NoError(i.t, err)
	token, err := jwt.Signed(signer).Claims(c).Serialize()
	require.NoError(i.t, err)
	return token
}
