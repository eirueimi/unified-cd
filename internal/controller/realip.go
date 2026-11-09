package controller

import (
	"net/http"
	"net/netip"
	"strings"
)

// trustedProxyRealIP replaces r.RemoteAddr with the client address reported by
// a reverse proxy, but only when the TCP peer is itself inside one of the
// trusted prefixes. A request from any other peer keeps its socket address and
// its X-Forwarded-For / X-Real-IP headers are ignored, so a client cannot pick
// the address that the rate limiters and access log attribute to it.
//
// With no trusted prefixes (the default) the middleware is a pass-through.
//
// X-Forwarded-For is read right to left: each hop appends the address it saw,
// so the entries a trusted proxy added are at the right and everything left of
// the first untrusted entry is client-supplied. The rightmost entry that is
// not itself a trusted proxy is the client. X-Real-IP is the fallback when
// there is no usable X-Forwarded-For.
func trustedProxyRealIP(trusted []netip.Prefix) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if len(trusted) == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if peer, ok := parseAddr(normalizedRemoteIP(r.RemoteAddr)); ok && inPrefixes(trusted, peer) {
				if client, ok := forwardedClient(r, trusted); ok {
					r.RemoteAddr = client.String()
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func forwardedClient(r *http.Request, trusted []netip.Prefix) (netip.Addr, bool) {
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		addr, ok := parseAddr(strings.TrimSpace(hops[i]))
		if !ok {
			// A malformed hop cannot be attributed; stop rather than skip to
			// an entry further left that a client may have supplied.
			return netip.Addr{}, false
		}
		if !inPrefixes(trusted, addr) {
			return addr, true
		}
	}
	if addr, ok := parseAddr(strings.TrimSpace(r.Header.Get("X-Real-IP"))); ok {
		return addr, true
	}
	return netip.Addr{}, false
}

func parseAddr(s string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap().WithZone(""), true
}

func inPrefixes(prefixes []netip.Prefix, addr netip.Addr) bool {
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
