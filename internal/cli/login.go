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
)

// errOIDCNotConfigured is returned by fetchServerOIDCConfig when the server
// has no OIDC provider configured (HTTP 404).
var errOIDCNotConfigured = errors.New("OIDC not configured on this server")

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

			ctx := context.Background()
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
				Scopes:   []string{"openid", "email", "profile"},
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

			// Save the token to the config file
			configPath := DefaultConfigPath()
			if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
				return err
			}
			if err := writeAgentConfig(configPath, AgentConfig{
				Server: serverURL,
				Token:  apiToken,
			}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\nLogged in. Token saved to %s\n", configPath)
			if !token.Expiry.IsZero() {
				fmt.Fprintf(cmd.OutOrStdout(), "Expires: %s\n", token.Expiry.Format(time.RFC3339))
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
}

// fetchServerOIDCConfig fetches OIDC configuration from the master server's OIDC config endpoint.
func fetchServerOIDCConfig(serverURL string) (*serverOIDCConfig, error) {
	resp, err := http.Get(serverURL + "/api/v1/auth/oidc-config") //nolint:gosec
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

	if err := verifyToken(serverURL, token); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return err
	}
	if err := writeAgentConfig(configPath, AgentConfig{
		Server: serverURL,
		Token:  token,
	}); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Logged in. Token saved to %s\n", configPath)
	return nil
}

// verifyToken calls GET /api/v1/tokens with the given Bearer token to confirm it is valid.
func verifyToken(serverURL, token string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
	resp, err := http.Get(issuer + "/.well-known/openid-configuration") //nolint:gosec
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
