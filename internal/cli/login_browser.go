package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/eirueimi/unified-cd/internal/api"
)

const (
	errBrowserNotConfigured = "browser login is not configured on this server"
	errBrowserUpgrade       = "this server does not support browser login for the CLI; upgrade the controller or use the device flow"
	browserDonePage         = "Login complete. You can close this tab."
)

// browserLoginTimeout is how long login --browser waits for the browser to
// come back. A variable so tests can shorten it.
var browserLoginTimeout = 5 * time.Minute

// openBrowser opens a URL with the OS default handler. A variable so tests can
// drive the flow without a real browser.
var openBrowser = func(u string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
	case "darwin":
		return exec.Command("open", u).Start()
	default:
		return exec.Command("xdg-open", u).Start()
	}
}

// browserLogin runs the loopback + PKCE login against the controller: the
// browser goes to the controller start URL, the IdP sends it back to the
// controller, and the controller redirects it to this process's loopback
// listener with a one-time code that is exchanged together with the verifier.
func browserLogin(ctx context.Context, cmd *cobra.Command, serverURL, configPath string) error {
	serverURL = strings.TrimRight(serverURL, "/")
	cfg, err := fetchServerOIDCConfigCtx(ctx, serverURL)
	if errors.Is(err, errOIDCNotConfigured) {
		return errors.New(errBrowserNotConfigured)
	}
	if err != nil {
		return fmt.Errorf("failed to fetch OIDC config from server: %w", err)
	}
	if !cfg.BrowserSSOEnabled {
		return errors.New(errBrowserNotConfigured)
	}

	verifier, err := randomBase64URL(32)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("could not start the loopback listener: %w", err)
	}
	handler, codes := callbackHandler()
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	redirect := "http://" + ln.Addr().String() + "/callback"
	startURL := serverURL + "/api/v1/auth/cli/browser/start?" + url.Values{
		"redirect":  {redirect},
		"challenge": {challenge},
	}.Encode()

	if err := preflightBrowserStart(ctx, startURL); err != nil {
		return err
	}

	fmt.Fprintf(cmd.OutOrStdout(), "\nOpen the following URL in your browser:\n  %s\n\nWaiting for the browser login to finish...\n", startURL)
	if err := openBrowser(startURL); err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "could not open the browser automatically; open the URL above manually")
	}

	timer := time.NewTimer(browserLoginTimeout)
	defer timer.Stop()
	var code string
	select {
	case code = <-codes:
	case <-timer.C:
		return fmt.Errorf("timed out after %s waiting for the browser login", browserLoginTimeout)
	case <-ctx.Done():
		return fmt.Errorf("browser login cancelled: %w", ctx.Err())
	}

	body, _ := json.Marshal(api.CLIBrowserExchangeRequest{Code: code, CodeVerifier: verifier})
	var tok api.CLITokenResponse
	if err := postRelay(ctx, serverURL, "/api/v1/auth/cli/browser/exchange", body, &tok); err != nil {
		return fmt.Errorf("browser login failed: %w", err)
	}
	if tok.IDToken == "" {
		return errors.New("browser login failed: server returned no id_token")
	}
	if err := persistLogin(configPath, serverURL, tok, "browser", false); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "\nLogged in. Token saved to %s\n", configPath)
	if !tok.Expiry.IsZero() {
		fmt.Fprintf(cmd.OutOrStdout(), "Expires: %s\n", tok.Expiry.Format(time.RFC3339))
	}
	if tok.RefreshToken == "" {
		fmt.Fprintln(cmd.ErrOrStderr(), noRefreshTokenWarning)
	}
	return nil
}

// preflightBrowserStart asks the start endpoint once, without following its
// redirect to the IdP, so an old controller (404) or a misconfigured one (4xx,
// 5xx) is reported here instead of as a blank browser tab and a timeout.
func preflightBrowserStart(ctx context.Context, startURL string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, startURL, nil)
	if err != nil {
		return err
	}
	// Share the default client transport so the extra request headers apply.
	client := &http.Client{
		Transport:     http.DefaultClient.Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach the server: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return errors.New(errBrowserUpgrade)
	case resp.StatusCode >= 400:
		var oe struct {
			Error string `json:"error"`
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if json.Unmarshal(b, &oe) == nil && oe.Error != "" && len(oe.Error) <= 200 {
			return fmt.Errorf("browser login failed: server returned %d: %s", resp.StatusCode, oe.Error)
		}
		return fmt.Errorf("browser login failed: server returned %d", resp.StatusCode)
	}
	return nil
}

// callbackHandler serves the loopback /callback. The first request that carries
// a code delivers it on the returned channel; the code is never logged.
func callbackHandler() (http.Handler, <-chan string) {
	codes := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if r.Method != http.MethodGet || code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			return
		}
		select {
		case codes <- code:
		default: // a code was already delivered
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, browserDonePage)
	})
	return mux, codes
}

func randomBase64URL(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
