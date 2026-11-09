package controller

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func realIPFor(t *testing.T, trusted []string, peer string, headers map[string]string) string {
	t.Helper()
	var prefixes []netip.Prefix
	for _, c := range trusted {
		prefixes = append(prefixes, netip.MustParsePrefix(c))
	}
	var got string
	h := trustedProxyRealIP(prefixes)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.RemoteAddr
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = peer
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	h.ServeHTTP(httptest.NewRecorder(), req)
	return got
}

func TestTrustedProxyRealIP(t *testing.T) {
	trusted := []string{"10.0.0.0/8", "2001:db8::/32"}
	cases := []struct {
		name    string
		trusted []string
		peer    string
		headers map[string]string
		want    string
	}{
		{"no trusted proxies ignores spoofed XFF", nil, "203.0.113.9:1234", map[string]string{"X-Forwarded-For": "1.2.3.4"}, "203.0.113.9:1234"},
		{"untrusted peer spoofing XFF is ignored", trusted, "203.0.113.9:1234", map[string]string{"X-Forwarded-For": "1.2.3.4"}, "203.0.113.9:1234"},
		{"untrusted peer spoofing X-Real-IP is ignored", trusted, "203.0.113.9:1234", map[string]string{"X-Real-IP": "1.2.3.4"}, "203.0.113.9:1234"},
		{"trusted peer, single XFF", trusted, "10.1.2.3:80", map[string]string{"X-Forwarded-For": "198.51.100.7"}, "198.51.100.7"},
		{"trusted peer, client-prepended XFF uses rightmost untrusted", trusted, "10.1.2.3:80", map[string]string{"X-Forwarded-For": "1.2.3.4, 198.51.100.7, 10.9.9.9"}, "198.51.100.7"},
		{"trusted peer, only trusted hops falls back to X-Real-IP", trusted, "10.1.2.3:80", map[string]string{"X-Forwarded-For": "10.9.9.9", "X-Real-IP": "198.51.100.8"}, "198.51.100.8"},
		{"trusted peer, X-Real-IP only", trusted, "10.1.2.3:80", map[string]string{"X-Real-IP": "198.51.100.8"}, "198.51.100.8"},
		{"trusted peer, no headers keeps peer", trusted, "10.1.2.3:80", nil, "10.1.2.3:80"},
		{"trusted peer, malformed XFF hop is not skipped", trusted, "10.1.2.3:80", map[string]string{"X-Forwarded-For": "1.2.3.4, garbage"}, "10.1.2.3:80"},
		{"XFF hop with a port", trusted, "10.1.2.3:80", map[string]string{"X-Forwarded-For": "198.51.100.7:5555"}, "198.51.100.7"},
		{"XFF hop with bracketed v6 and port", trusted, "10.1.2.3:80", map[string]string{"X-Forwarded-For": "[2001:4860::1]:443"}, "2001:4860::1"},
		{"XFF hop with bare bracketed v6", trusted, "10.1.2.3:80", map[string]string{"X-Forwarded-For": "[2001:4860::1]"}, "2001:4860::1"},
		{"trusted IPv6 peer", trusted, "[2001:db8::1]:443", map[string]string{"X-Forwarded-For": "198.51.100.7"}, "198.51.100.7"},
		{"IPv4-mapped peer matches IPv4 prefix", trusted, "[::ffff:10.1.2.3]:80", map[string]string{"X-Forwarded-For": "198.51.100.7"}, "198.51.100.7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, realIPFor(t, tc.trusted, tc.peer, tc.headers))
		})
	}
}

// A spoofed X-Forwarded-For from an untrusted peer must not move the client
// into another bucket of the enrollment rate limiter (the original bypass).
func TestTrustedProxyRealIP_SpoofDoesNotBypassEnrollmentLimiter(t *testing.T) {
	prefixes := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	lim := newEnrollmentLimiter(nil)
	var allowed int
	h := trustedProxyRealIP(prefixes)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if lim.allow(r, "aws", "p") {
			allowed++
		}
	}))
	for i := 0; i < 20; i++ {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.RemoteAddr = "203.0.113.9:1234"
		req.Header.Set("X-Forwarded-For", "198.51.100."+string(rune('0'+i%10)))
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	require.Equal(t, 5, allowed, "burst is 5 per real client address, regardless of forged headers")
}
