// Command enrollproxy is the W4 ENROLLMENT INTERPOSER: a transparent reverse
// proxy in front of the controller LB that answers exactly one endpoint itself
// — POST /api/v1/agents/enroll — and forwards everything else unchanged.
//
// WHY IT EXISTS. W4-0 established that Kubernetes agent enrollment is
// unconditionally broken: kubernetesEnrollmentVerifier.Verify reads the
// enrolling ServiceAccount's UID from a TokenReview extra key the API server
// never populates (internal/controller/agent_enrollment_kubernetes.go:84-87),
// so every k8s enrollment is rejected 403 — and PR #75 removed the k8s agent's
// static-token path, leaving it with no working authentication at all
// (scenarios/w4-0-enrollment-spike.md). Phase 1 of this campaign does not fix
// product code, so W4 needs a test-infrastructure bypass to reach the agent
// behaviour it is chartered to measure. This is the same pattern W3 used when
// it put an interposer in front of Garage.
//
// WHAT IT DOES NOT DO. It mints nothing. The credential it hands back is a
// real controller-issued `uca_` access token obtained through the product's
// own supported "enrollment" method (see w4-mint-credential.sh), and it is
// re-minted through the product's own `POST /api/v1/agents/token/refresh`.
// The controller's authorization of every subsequent request is completely
// untouched — verified in Step 2 of the W4 rig build: the agent-auth
// middleware (internal/controller/agent_auth.go:38-116) never consults
// enrollment_method, so a credential whose identity row says
// `enrollment_method = enrollment` authenticates k8s-agent traffic exactly
// like any other.
//
// DISCLOSURE. Every W4 scenario runs against a controller whose ENROLLMENT is
// bypassed by this process. No W4 finding says anything about the enrollment
// path itself beyond what w4-0 already recorded. See README.md
// "The W4 Kubernetes rig".
//
// Usage:
//
//	go run -buildvcs=false ./test/edgecase/tools/w4/enrollproxy \
//	  -listen 127.0.0.1:18099 \
//	  -upstream http://localhost:18080 \
//	  -credentials test/edgecase/tools/w4/w4-agent-credentials.json
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// enrollPath is the ONLY path this proxy answers itself.
const enrollPath = "/api/v1/agents/enroll"

// refreshPath is the product endpoint used to re-mint the access credential
// when the cached one is close to expiry. Rotation invalidates the old refresh
// token, so the rotated pair is persisted back to the credentials file.
const refreshPath = "/api/v1/agents/token/refresh"

// credentials mirrors the fields of api.AgentTokenResponse that the k8s agent's
// KubernetesCredentialSource actually consumes (internal/k8sagent/credentials.go:
// Token() requires AgentID, AccessToken and an AccessExpiresAt in the future).
type credentials struct {
	AgentID          string    `json:"agentId"`
	AccessToken      string    `json:"accessToken"`
	AccessExpiresAt  time.Time `json:"accessExpiresAt"`
	RefreshToken     string    `json:"refreshToken,omitempty"`
	RefreshExpiresAt time.Time `json:"refreshExpiresAt,omitempty"`
	Labels           []string  `json:"labels"`
	Capabilities     []string  `json:"capabilities"`
}

type interposer struct {
	upstream  *url.URL
	proxy     *httputil.ReverseProxy
	credsPath string
	minLeft   time.Duration
	client    *http.Client
	// blockFile is the one-way agent->controller partition switch. It is the
	// W4 analogue of inject.sh's `nginx-block`, and a strictly sharper one:
	// nginx-block denies an agent's SOURCE IP at the LB, which cannot single
	// out a host-run agent sharing the host's address with every curl in the
	// scenario, whereas this proxy carries ONE agent's traffic and nothing
	// else. See w4-k8s-inject.sh `block` / `unblock`.
	blockFile string

	mu    sync.Mutex
	creds credentials

	intercepts atomic.Int64
	refreshes  atomic.Int64
	forwards   atomic.Int64
	blocked    atomic.Int64
	severed    atomic.Int64

	connMu sync.Mutex
	conns  map[net.Conn]struct{}
	armed  bool
}

// trackConn/untrackConn maintain the live connection set so arming the
// partition can SEVER what is already in flight.
//
// MEASURED, AND THIS IS WHY IT EXISTS: without severing, `block` armed at
// 15:26:27 still let the agent claim and run edge-w4-tick at 15:26:44 — its
// claim long-poll (`?timeout=30s`) had entered the handler BEFORE the arm and
// was already proxying, so the per-request check never saw it, and the
// controller handed it a run 17 s into a nominally-armed window
// (step5-block-verb.txt, EFFECT 3, first measurement). That is the same class
// as the README's nginx-reload trap ("an already-connected agent may keep
// being served by an old worker"), and exactly the class W3-1's `s3-slow`
// lesson is about: state inspection said armed, the effect said otherwise.
func (ip *interposer) trackConn(c net.Conn) {
	ip.connMu.Lock()
	defer ip.connMu.Unlock()
	if ip.conns == nil {
		ip.conns = map[net.Conn]struct{}{}
	}
	ip.conns[c] = struct{}{}
}

func (ip *interposer) untrackConn(c net.Conn) {
	ip.connMu.Lock()
	defer ip.connMu.Unlock()
	delete(ip.conns, c)
}

// watchArm polls the block file and closes every live connection on the
// transition into an armed state, so the arm takes effect within one poll
// interval rather than at the end of the longest in-flight long poll.
func (ip *interposer) watchArm() {
	for {
		time.Sleep(200 * time.Millisecond)
		armed := ip.blockMode() != ""
		ip.connMu.Lock()
		became := armed && !ip.armed
		ip.armed = armed
		var doomed []net.Conn
		if became {
			for c := range ip.conns {
				doomed = append(doomed, c)
			}
		}
		ip.connMu.Unlock()
		if became {
			for _, c := range doomed {
				_ = c.Close()
			}
			ip.severed.Add(int64(len(doomed)))
			log.Printf("BLOCK-ARM severed %d in-flight connection(s)", len(doomed))
		}
	}
}

// blockMode reads the partition switch. Returning "" means "not armed".
// The file's CONTENT selects the failure shape:
//
//	(empty) or "reset" — hijack and close the connection, so the agent's HTTP
//	                     client sees a transport error. This is the closest
//	                     analogue to a partition: no status code is produced,
//	                     so the agent cannot distinguish it from the
//	                     controller being gone.
//	"hang"             — accept and never answer, until the client gives up.
//	                     Exercises the agent's own timeouts instead.
//	"<3-digit code>"   — answer with that status (e.g. 503), for arms that
//	                     need a controller-shaped rejection rather than a
//	                     network failure.
//
// A file switch (not a signal, not an HTTP control port) is deliberate: it is
// the same idiom as inject.sh's $S3FAULT_DIR arm files, it survives a proxy
// restart, and `cat` on it is the arm's own state readout.
func (ip *interposer) blockMode() string {
	if ip.blockFile == "" {
		return ""
	}
	data, err := os.ReadFile(ip.blockFile)
	if err != nil {
		return ""
	}
	mode := strings.TrimSpace(string(data))
	if mode == "" {
		mode = "reset"
	}
	return mode
}

func main() {
	listen := flag.String("listen", "127.0.0.1:18099", "address to listen on")
	upstream := flag.String("upstream", "http://localhost:18080", "controller (or LB) base URL to forward to")
	credsPath := flag.String("credentials", "", "path to the JSON credentials file written by w4-mint-credential.sh (required)")
	minLeft := flag.Duration("min-remaining", 5*time.Minute, "re-mint the access credential when less than this remains on it")
	blockFile := flag.String("block-file", "", "when this file EXISTS, every request is failed (one-way agent->controller partition; content selects reset|hang|<status>)")
	flag.Parse()

	if *credsPath == "" {
		log.Fatal("enrollproxy: -credentials is required")
	}
	target, err := url.Parse(*upstream)
	if err != nil || target.Scheme == "" || target.Host == "" {
		log.Fatalf("enrollproxy: -upstream %q is not an absolute URL", *upstream)
	}

	ip := &interposer{
		upstream:  target,
		credsPath: *credsPath,
		minLeft:   *minLeft,
		blockFile: *blockFile,
		// No client-level timeout: the agent's claim is a 60 s long poll and the
		// artifact paths carry large bodies. The refresh call rides the same
		// client and is bounded by the request context instead.
		client: &http.Client{},
	}
	if err := ip.load(); err != nil {
		log.Fatalf("enrollproxy: %v", err)
	}
	ip.proxy = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Host = r.In.Host
			r.SetXForwarded()
		},
		// -1 flushes immediately, so SSE and any other streamed response is not
		// buffered here. The agent's claim long-poll does not stream, but the
		// campaign's rule is that the interposer must be transparent, and a
		// future scenario reading /api/v1/runs/{id}/events through this port
		// must not be silently broken by buffering.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("FORWARD-ERROR %s %s: %v", r.Method, r.URL.Path, err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}

	log.Printf("enrollproxy: listening on %s -> %s", *listen, target)
	log.Printf("enrollproxy: intercepting POST %s for agentId=%s (credential %s)",
		enrollPath, ip.creds.AgentID, shortToken(ip.creds.AccessToken))
	srv := &http.Server{
		Addr:    *listen,
		Handler: ip,
		// No ReadHeaderTimeout: this proxy carries a 30-60 s claim long poll and
		// must not impose a deadline the controller does not.
		ConnState: func(c net.Conn, state http.ConnState) {
			switch state {
			case http.StateNew:
				ip.trackConn(c)
			case http.StateHijacked, http.StateClosed:
				ip.untrackConn(c)
			}
		},
	}
	go ip.watchArm()
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("enrollproxy: %v", err)
	}
}

func (ip *interposer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The partition is checked FIRST, ahead of the enrollment interception, so
	// an armed block also severs re-enrollment. A partition that let the agent
	// keep renewing its credential would not be a partition.
	if mode := ip.blockMode(); mode != "" {
		n := ip.blocked.Add(1)
		log.Printf("BLOCK #%d %s %s mode=%s", n, r.Method, r.URL.Path, mode)
		switch {
		case mode == "hang":
			<-r.Context().Done()
		case len(mode) == 3 && mode[0] >= '1' && mode[0] <= '5':
			code := int(mode[0]-'0')*100 + int(mode[1]-'0')*10 + int(mode[2]-'0')
			http.Error(w, "w4-inject: agent->controller partition armed", code)
		default: // "reset"
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
					return
				}
			}
			// Hijack unavailable (HTTP/2). Fall back to a panic, which makes
			// net/http drop the connection without a response — same observable
			// shape at the client.
			panic(http.ErrAbortHandler)
		}
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == enrollPath {
		ip.handleEnroll(w, r)
		return
	}
	ip.forwards.Add(1)
	ip.proxy.ServeHTTP(w, r)
}

// handleEnroll answers the intercepted enrollment exchange. It MUST be
// answerable repeatedly: KubernetesCredentialSource.Token() re-enters the
// exchange whenever the cached access token is inside its refresh lead time
// (15 min + up to 5 min of jitter, credentials.go:83), and Invalidate() drops
// the cache on a rejected GET (internal/agent/client.go:96-100).
func (ip *interposer) handleEnroll(w http.ResponseWriter, r *http.Request) {
	n := ip.intercepts.Add(1)
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	var req struct {
		Provider     string   `json:"provider"`
		Policy       string   `json:"policy"`
		Labels       []string `json:"labels"`
		Capabilities []string `json:"capabilities"`
	}
	_ = json.Unmarshal(body, &req)

	creds, refreshed, err := ip.currentCredential(r)
	if err != nil {
		log.Printf("INTERCEPT #%d POST %s from=%s provider=%q policy=%q -> 503 (%v)",
			n, enrollPath, r.RemoteAddr, req.Provider, req.Policy, err)
		http.Error(w, "enrollproxy: could not produce an access credential", http.StatusServiceUnavailable)
		return
	}
	log.Printf("INTERCEPT #%d POST %s from=%s provider=%q policy=%q labels=%v caps=%v -> 200 agentId=%s cred=%s expires=%s remounted=%t",
		n, enrollPath, r.RemoteAddr, req.Provider, req.Policy, req.Labels, req.Capabilities,
		creds.AgentID, shortToken(creds.AccessToken), creds.AccessExpiresAt.UTC().Format(time.RFC3339), refreshed)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-W4-Enroll-Interposed", fmt.Sprintf("%d", n))
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"agentId":         creds.AgentID,
		"accessToken":     creds.AccessToken,
		"accessExpiresAt": creds.AccessExpiresAt,
		"labels":          creds.Labels,
		"capabilities":    creds.Capabilities,
	})
}

// currentCredential returns a usable access credential, rotating through the
// controller's real refresh endpoint when the cached one is nearly spent.
func (ip *interposer) currentCredential(r *http.Request) (credentials, bool, error) {
	ip.mu.Lock()
	defer ip.mu.Unlock()
	if ip.creds.AccessToken != "" && time.Until(ip.creds.AccessExpiresAt) > ip.minLeft {
		return ip.creds, false, nil
	}
	if ip.creds.RefreshToken == "" {
		return ip.creds, false, fmt.Errorf("access credential is spent and no refresh token is available; re-run w4-mint-credential.sh")
	}
	fresh, err := ip.refresh(r)
	if err != nil {
		return ip.creds, false, err
	}
	ip.creds = fresh
	if err := ip.save(); err != nil {
		log.Printf("enrollproxy: WARNING could not persist rotated credential: %v", err)
	}
	ip.refreshes.Add(1)
	return ip.creds, true, nil
}

func (ip *interposer) refresh(r *http.Request) (credentials, error) {
	var out credentials
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		strings.TrimSuffix(ip.upstream.String(), "/")+refreshPath, bytes.NewReader(nil))
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+ip.creds.RefreshToken)
	resp, err := ip.client.Do(req)
	if err != nil {
		return out, fmt.Errorf("refresh: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("refresh: controller answered %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return out, fmt.Errorf("refresh: decode: %w", err)
	}
	if out.AccessToken == "" || out.AgentID == "" {
		return out, fmt.Errorf("refresh: controller returned an empty credential")
	}
	return out, nil
}

func (ip *interposer) load() error {
	data, err := os.ReadFile(ip.credsPath)
	if err != nil {
		return fmt.Errorf("read credentials %s: %w", ip.credsPath, err)
	}
	if err := json.Unmarshal(data, &ip.creds); err != nil {
		return fmt.Errorf("parse credentials %s: %w", ip.credsPath, err)
	}
	if ip.creds.AgentID == "" || ip.creds.AccessToken == "" {
		return fmt.Errorf("credentials %s carry no agentId/accessToken", ip.credsPath)
	}
	return nil
}

// save rewrites the credentials file atomically, 0600, after a rotation.
func (ip *interposer) save() error {
	data, err := json.MarshalIndent(ip.creds, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(ip.credsPath), ".w4creds.tmp")
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, ip.credsPath)
}

// shortToken renders a token as its kind plus the first 8 characters of its
// credential UUID (agentauth format `<kind>_<uuid>_<secret>`). The secret is
// never logged. This is deliberately the same shape the campaign's evidence
// scrubbing leaves behind.
func shortToken(tok string) string {
	parts := strings.SplitN(tok, "_", 3)
	if len(parts) < 2 {
		return "(none)"
	}
	id := parts[1]
	if len(id) > 8 {
		id = id[:8]
	}
	return parts[0] + "_" + id + "..."
}
