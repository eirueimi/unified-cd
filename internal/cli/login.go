package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/oauth2"
	"golang.org/x/term"

	"github.com/eirueimi/unified-cd/internal/api"
)

// noRefreshTokenWarning is printed when the IdP issued no refresh token.
const noRefreshTokenWarning = "the identity provider issued no refresh token; you will need to log in again when the token expires"

// errOIDCNotConfigured is returned by fetchServerOIDCConfig when the server
// has no OIDC provider configured (HTTP 404).
var errOIDCNotConfigured = errors.New("OIDC not configured on this server")

// cmdCtx returns the command's context, falling back to context.Background()
// when it is nil (e.g. a command invoked in tests without ExecuteContext).
// Production runs go through main.go's ExecuteContext, so the returned context
// carries the SIGINT/SIGTERM cancellation that lets Ctrl-C abort login.
func cmdCtx(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

func newLoginCmd() *cobra.Command {
	var oidcIssuer, clientID, serverURL string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in via OIDC device flow and save the token",
		RunE: func(cmd *cobra.Command, args []string) error {
			if serverURL == "" {
				serverURL = os.Getenv("UNIFIED_SERVER")
			}
			if serverURL == "" {
				return fmt.Errorf("--server is required (or set UNIFIED_SERVER)")
			}
			// If --issuer / --client-id are not specified, fetch OIDC config from the server
			var serverCfg *serverOIDCConfig
			if oidcIssuer == "" || clientID == "" {
				var err error
				serverCfg, err = fetchServerOIDCConfig(serverURL)
				if errors.Is(err, errOIDCNotConfigured) {
					return tokenPromptLogin(cmd, serverURL, DefaultConfigPath())
				}
				if err != nil {
					return fmt.Errorf("failed to fetch OIDC config from server: %w\nSpecify --issuer and --client-id manually", err)
				}
				oidcIssuer = serverCfg.Issuer
				// Device flow uses a public client (deviceClientId).
				// Falls back to the browser clientId if not provided.
				clientID = serverCfg.DeviceClientID
				if clientID == "" {
					clientID = serverCfg.ClientID
				}
			}

			// Use cmd.Context() (not context.Background()) so a Ctrl-C / SIGINT —
			// which main.go wires to this context via signal.NotifyContext —
			// cancels the OIDC device-flow polling (DeviceAuth/DeviceAccessToken)
			// promptly instead of leaving `login` hung waiting for browser approval.
			ctx := cmdCtx(cmd)
			// A controller that holds the device client secret relays the device
			// flow; the CLI must not talk to the IdP directly then.
			if serverCfg != nil && serverCfg.DeviceRelay {
				return relayDeviceLogin(ctx, cmd, serverURL)
			}
			// Use the endpoints provided by the server if it has already performed discovery
			// (the Dex device authorization endpoint is implementation-specific, e.g. /device/code,
			//  so trust the discovery result rather than hardcoding it).
			// Only perform well-known discovery on the CLI side if the server does not provide them.
			var endpoint oauth2.Endpoint
			if serverCfg != nil && serverCfg.DeviceAuthEndpoint != "" && serverCfg.TokenEndpoint != "" {
				endpoint = oauth2.Endpoint{
					DeviceAuthURL: serverCfg.DeviceAuthEndpoint,
					TokenURL:      serverCfg.TokenEndpoint,
				}
			} else {
				var err error
				endpoint, err = discoverOIDCEndpoint(oidcIssuer)
				if err != nil {
					return fmt.Errorf("OIDC provider discovery error: %w", err)
				}
			}

			oauth2cfg := &oauth2.Config{
				ClientID: clientID,
				Endpoint: endpoint,
				Scopes:   deviceScopes(serverCfg),
			}

			// The Dex device authorization endpoint does not use redirect_uri
			// (the post-approval callback is hardcoded to /device/callback inside Dex).
			deviceResp, err := oauth2cfg.DeviceAuth(ctx)
			if err != nil {
				return fmt.Errorf("device authentication error: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\nOpen the following URL in your browser:\n  %s\n\nWaiting (user code: %s)...\n",
				deviceResp.VerificationURIComplete, deviceResp.UserCode)

			token, err := oauth2cfg.DeviceAccessToken(ctx, deviceResp)
			if err != nil {
				return fmt.Errorf("device access token error: %w", err)
			}

			// Save the verifiable id_token (JWT) for server API authentication.
			// access_token cannot be used for verification because its aud differs depending on the Dex implementation.
			apiToken, _ := token.Extra("id_token").(string)
			if apiToken == "" {
				apiToken = token.AccessToken
			}

			// Expiry is the id_token's exp, never the access token's.
			expiry := token.Expiry
			if exp, err := idTokenExpiry(apiToken); err == nil {
				expiry = exp
			}
			refresh := token.RefreshToken
			configPath := DefaultConfigPath()
			if err := saveLogin(configPath, Config{Server: serverURL}, api.CLITokenResponse{
				IDToken:      apiToken,
				RefreshToken: refresh,
				Expiry:       expiry,
			}, "device", false); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\nLogged in. Token saved to %s\n", configPath)
			if !expiry.IsZero() {
				fmt.Fprintf(cmd.OutOrStdout(), "Expires: %s\n", expiry.Format(time.RFC3339))
			}
			if refresh == "" {
				fmt.Fprintln(cmd.ErrOrStderr(), noRefreshTokenWarning)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&oidcIssuer, "issuer", "", "OIDC issuer URL")
	cmd.Flags().StringVar(&clientID, "client-id", "", "OIDC client ID")
	cmd.Flags().StringVar(&serverURL, "server", "", "master server URL (required)")
	return cmd
}

// serverOIDCConfig holds OIDC configuration fetched from the server.
// When the server operates with IssuerInternal (Docker proxy configuration),
// DeviceAuthEndpoint and TokenEndpoint are also returned, allowing discovery to be skipped on the CLI side.
type serverOIDCConfig struct {
	Issuer             string `json:"issuer"`
	ClientID           string `json:"clientId"`
	DeviceClientID     string `json:"deviceClientId"`
	DeviceAuthEndpoint string `json:"deviceAuthEndpoint"`
	TokenEndpoint      string `json:"tokenEndpoint"`
	// DeviceRelay is true when the controller relays the device flow and
	// refresh (it holds the device client secret). Absent on older servers.
	DeviceRelay        bool   `json:"deviceRelay"`
	RevocationEndpoint string `json:"revocationEndpoint"`
	// Scopes are the scopes the server wants the device flow to request
	// (including the roles claim). Absent on older servers.
	Scopes []string `json:"scopes"`
}

// deviceScopes returns the scopes for the device flow: the server's when it
// sent any, else openid/email/profile (older servers, or manual --issuer).
func deviceScopes(cfg *serverOIDCConfig) []string {
	if cfg != nil && len(cfg.Scopes) > 0 {
		return cfg.Scopes
	}
	return []string{"openid", "email", "profile"}
}

// fetchServerOIDCConfig fetches OIDC configuration from the master server's OIDC config endpoint.
func fetchServerOIDCConfig(serverURL string) (*serverOIDCConfig, error) {
	return fetchServerOIDCConfigCtx(context.Background(), serverURL)
}

// fetchServerOIDCConfigCtx is fetchServerOIDCConfig bound to ctx.
func fetchServerOIDCConfigCtx(ctx context.Context, serverURL string) (*serverOIDCConfig, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, serverURL+"/api/v1/auth/oidc-config", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errOIDCNotConfigured
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server returned %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	var cfg serverOIDCConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// tokenPromptLogin handles login when the server has no SSO configured.
// It prompts for an existing PAT, verifies it, and saves it to configPath.
func tokenPromptLogin(cmd *cobra.Command, serverURL, configPath string) error {
	fmt.Fprintln(cmd.OutOrStdout(), "SSO is not configured on this server.")
	fmt.Fprint(cmd.OutOrStdout(), "Enter your personal access token: ")

	var token string
	// term.IsTerminal and term.ReadPassword require a real OS file descriptor;
	// cmd.InOrStdin() may be a bytes.Buffer substitute (e.g. in tests) with no fd.
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		if err != nil {
			return fmt.Errorf("read token: %w", err)
		}
		fmt.Fprintln(cmd.OutOrStdout()) // newline after masked input
		token = strings.TrimSpace(string(b))
	} else {
		reader := bufio.NewReader(cmd.InOrStdin())
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read token: %w", err)
		}
		token = strings.TrimSpace(line)
	}

	if token == "" {
		return fmt.Errorf("token cannot be empty")
	}

	if err := verifyToken(cmdCtx(cmd), serverURL, token); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return err
	}
	if err := SaveConfig(configPath, Config{
		Server: serverURL,
		Token:  token,
	}); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Logged in. Token saved to %s\n", configPath)
	return nil
}

// verifyToken calls GET /api/v1/tokens with the given Bearer token to confirm it is valid.
func verifyToken(ctx context.Context, serverURL, token string) error {
	// Derive the timeout from the caller's context (cmd.Context()) so a Ctrl-C
	// cancels verification immediately rather than waiting out the 10s bound.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, serverURL+"/api/v1/tokens", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("token verification failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("invalid token: authentication failed")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token verification failed: server returned %d", resp.StatusCode)
	}
	return nil
}

// discoverOIDCEndpoint fetches endpoint information from the OIDC provider's well-known endpoint.
func discoverOIDCEndpoint(issuer string) (oauth2.Endpoint, error) {
	return discoverOIDCEndpointCtx(context.Background(), issuer)
}

// discoverOIDCEndpointCtx is discoverOIDCEndpoint bound to ctx.
func discoverOIDCEndpointCtx(ctx context.Context, issuer string) (oauth2.Endpoint, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return oauth2.Endpoint{}, err
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec
	if err != nil {
		return oauth2.Endpoint{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		body := string(b)
		if len(body) > 200 {
			body = body[:200] + "..."
		}
		return oauth2.Endpoint{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}
	var discovery struct {
		DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
		TokenEndpoint               string `json:"token_endpoint"`
	}
	if err := json.Unmarshal(b, &discovery); err != nil {
		return oauth2.Endpoint{}, err
	}
	return oauth2.Endpoint{
		DeviceAuthURL: discovery.DeviceAuthorizationEndpoint,
		TokenURL:      discovery.TokenEndpoint,
	}, nil
}

// relayDeviceLogin runs the device flow through the controller's relay
// endpoints, using the shared default HTTP client so extra headers apply.
func relayDeviceLogin(ctx context.Context, cmd *cobra.Command, serverURL string) error {
	var start api.CLIDeviceStartResponse
	if err := postRelay(ctx, serverURL, "/api/v1/auth/cli/device/start", nil, &start); err != nil {
		return fmt.Errorf("device authentication error: %w", err)
	}
	link := start.VerificationURIComplete
	if link == "" {
		link = start.VerificationURI
	}
	fmt.Fprintf(cmd.OutOrStdout(), "\nOpen the following URL in your browser:\n  %s\n\nWaiting (user code: %s)...\n", link, start.UserCode)

	interval := time.Duration(start.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	var deadline time.Time
	if start.ExpiresIn > 0 {
		deadline = time.Now().Add(time.Duration(start.ExpiresIn) * time.Second)
	}
	body, _ := json.Marshal(api.CLIDeviceTokenRequest{DeviceCode: start.DeviceCode})
	for {
		if !deadline.IsZero() && time.Now().After(deadline) {
			return fmt.Errorf("device access token error: expired_token")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("device access token error: %w", ctx.Err())
		case <-time.After(interval):
		}
		var tok api.CLITokenResponse
		err := postRelay(ctx, serverURL, "/api/v1/auth/cli/device/token", body, &tok)
		if err == nil {
			return finishRelayLogin(cmd, serverURL, tok)
		}
		switch err.Error() {
		case "authorization_pending":
		case "slow_down", "server returned 429":
			interval += 5 * time.Second
		default:
			return fmt.Errorf("device access token error: %w", err)
		}
	}
}

func finishRelayLogin(cmd *cobra.Command, serverURL string, tok api.CLITokenResponse) error {
	configPath := DefaultConfigPath()
	if err := saveLogin(configPath, Config{Server: serverURL}, tok, "device", true); err != nil {
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
