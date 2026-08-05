package cli

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// headerKV is one parsed "Key: Value" extra header.
type headerKV struct {
	key, value string
}

// parseHeaders turns "Key: Value" strings into key/value pairs. The value keeps
// everything after the FIRST colon (so tokens containing ':' survive), with one
// optional leading space trimmed. Blank entries are skipped; a line with no
// colon, or an empty key, is an error.
func parseHeaders(raw []string) ([]headerKV, error) {
	var out []headerKV
	for _, h := range raw {
		if strings.TrimSpace(h) == "" {
			continue
		}
		i := strings.IndexByte(h, ':')
		if i < 0 {
			return nil, fmt.Errorf("invalid header %q: want \"Key: Value\"", h)
		}
		key := strings.TrimSpace(h[:i])
		if key == "" {
			return nil, fmt.Errorf("invalid header %q: empty key", h)
		}
		val := strings.TrimPrefix(h[i+1:], " ")
		out = append(out, headerKV{key: key, value: val})
	}
	return out, nil
}

// headerRoundTripper injects extra headers into requests bound for a specific
// host (the configured Server), leaving requests to any other host — e.g. the
// OIDC issuer during `login` — untouched. Later entries win for a repeated key.
type headerRoundTripper struct {
	base    http.RoundTripper
	host    string // Server host:port (or host) the headers apply to; "" => all hosts
	headers []headerKV
}

func (h *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	base := h.base
	if base == nil {
		base = http.DefaultTransport
	}
	if len(h.headers) > 0 && (h.host == "" || sameHost(req.URL, h.host)) {
		// Clone so we never mutate the caller's request (net/http contract).
		req = req.Clone(req.Context())
		for _, kv := range h.headers {
			req.Header.Set(kv.key, kv.value)
		}
	}
	return base.RoundTrip(req)
}

// sameHost reports whether u targets the given "host" (host or host:port). A
// bare host in either side matches regardless of the scheme's default port.
func sameHost(u *url.URL, host string) bool {
	if u.Host == host {
		return true
	}
	return u.Hostname() == hostOnly(host)
}

func hostOnly(host string) string {
	if i := strings.LastIndexByte(host, ':'); i >= 0 && !strings.Contains(host, "]") {
		return host[:i]
	}
	return host
}

// installHeaderTransport wraps http.DefaultClient's transport so every request
// to cfg.Server carries cfg.Headers. A no-op when there are no headers, so
// default behaviour (and code paths that don't opt in) is unchanged. Called
// once at startup from the root command's PersistentPreRunE.
func installHeaderTransport(cfg Config) error {
	kvs, err := parseHeaders(cfg.Headers)
	if err != nil {
		return err
	}
	if len(kvs) == 0 {
		return nil
	}
	host := ""
	if cfg.Server != "" {
		if u, err := url.Parse(cfg.Server); err == nil {
			host = u.Host
		}
	}
	base := http.DefaultClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	http.DefaultClient.Transport = &headerRoundTripper{base: base, host: host, headers: kvs}
	return nil
}
