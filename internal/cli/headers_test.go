package cli

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseHeaders(t *testing.T) {
	kvs, err := parseHeaders([]string{
		"Proxy-Authorization: Bearer abc.def:ghi", // value keeps colons
		"X-Empty:",                                // empty value ok
		"  ",                                      // blank skipped
		"X-No-Space:v",                            // no space after colon
	})
	require.NoError(t, err)
	require.Len(t, kvs, 3)
	assert.Equal(t, headerKV{"Proxy-Authorization", "Bearer abc.def:ghi"}, kvs[0])
	assert.Equal(t, headerKV{"X-Empty", ""}, kvs[1])
	assert.Equal(t, headerKV{"X-No-Space", "v"}, kvs[2])

	_, err = parseHeaders([]string{"no-colon"})
	assert.Error(t, err)
	_, err = parseHeaders([]string{": novalue-key"})
	assert.Error(t, err)
}

// recordRT records the last request it saw and returns 204.
type recordRT struct{ last *http.Request }

func (r *recordRT) RoundTrip(req *http.Request) (*http.Response, error) {
	r.last = req
	return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Header: http.Header{}}, nil
}

func TestHeaderRoundTripper_ScopedToHost(t *testing.T) {
	rec := &recordRT{}
	rt := &headerRoundTripper{
		base:    rec,
		host:    "controller.example:8080",
		headers: []headerKV{{"Proxy-Authorization", "Bearer iap"}},
	}

	// Matching host → header injected.
	req, _ := http.NewRequest(http.MethodGet, "https://controller.example:8080/api/v1/jobs", nil)
	_, err := rt.RoundTrip(req)
	require.NoError(t, err)
	assert.Equal(t, "Bearer iap", rec.last.Header.Get("Proxy-Authorization"))
	// Original request not mutated (clone was used).
	assert.Empty(t, req.Header.Get("Proxy-Authorization"))

	// Different host (e.g. the OIDC issuer) → NOT injected.
	rec.last = nil
	req2, _ := http.NewRequest(http.MethodGet, "https://issuer.example/token", nil)
	_, err = rt.RoundTrip(req2)
	require.NoError(t, err)
	assert.Empty(t, rec.last.Header.Get("Proxy-Authorization"))
}

func TestInstallHeaderTransport(t *testing.T) {
	orig := http.DefaultClient.Transport
	t.Cleanup(func() { http.DefaultClient.Transport = orig })

	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Iap")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	require.NoError(t, installHeaderTransport(Config{Server: srv.URL, Headers: []string{"X-Iap: tok"}}))
	resp, err := http.DefaultClient.Get(srv.URL + "/api/v1/jobs")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, "tok", gotHeader)

	// No headers configured → transport untouched (no-op).
	http.DefaultClient.Transport = orig
	require.NoError(t, installHeaderTransport(Config{Server: srv.URL}))
	assert.Equal(t, orig, http.DefaultClient.Transport)
}

// End-to-end through the root command: --header reaches the server, and --token
// still populates Authorization (the two do not collide).
func TestRoot_HeaderFlagReachesServer(t *testing.T) {
	orig := http.DefaultClient.Transport
	t.Cleanup(func() { http.DefaultClient.Transport = orig })

	var auth, iap string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		iap = r.Header.Get("Proxy-Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	root := NewRoot()
	root.SetArgs([]string{"jobs", "list", "--server", srv.URL, "--token", "apptok",
		"--header", "Proxy-Authorization: Bearer iaptok"})
	require.NoError(t, root.Execute())

	assert.Equal(t, "Bearer apptok", auth)     // app token untouched in Authorization
	assert.Equal(t, "Bearer iaptok", iap)      // IAP token in the extra header
}
