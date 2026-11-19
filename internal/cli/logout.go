package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/eirueimi/unified-cd/internal/api"
)

func newLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Revoke the stored refresh token and clear the saved login",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := configPathFor(cmd)
			unlock, err := lockFile(path + ".lock")
			if err != nil {
				return fmt.Errorf("could not lock the config file: %w", err)
			}
			defer unlock()

			c, err := LoadConfig(path)
			if err != nil {
				return fmt.Errorf("read config: %w", err)
			}
			if c.RefreshToken != "" && c.Server != "" {
				if err := revokeStoredToken(cmdCtx(cmd), c); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not revoke the refresh token: %s\n", err.Error())
				}
			}
			c.Token = ""
			c.RefreshToken = ""
			c.TokenExpiry = time.Time{}
			c.LoginClient = ""
			c.LoginRelay = false
			if err := SaveConfig(path, c); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Logged out.")
			return nil
		},
	}
}

// revokeStoredToken revokes c.RefreshToken at the identity provider when the
// server advertises a revocation endpoint (otherwise it does nothing). Browser
// and relayed device logins go through the controller, which holds the client
// secret; direct device logins revoke at the IdP as a public client. The whole
// call is bounded by refreshTimeout because the config lock is held meanwhile.
// The returned error is safe to print: it never carries a token or a body.
func revokeStoredToken(ctx context.Context, c Config) error {
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	cfg, err := fetchServerOIDCConfigCtx(ctx, c.Server)
	if err != nil {
		return &refreshError{Reason: "could not fetch the OIDC config from the server"}
	}
	if cfg.RevocationEndpoint == "" {
		return nil
	}
	if c.LoginClient == "browser" || c.LoginRelay {
		client := "device"
		if c.LoginClient == "browser" {
			client = "browser"
		}
		body, _ := json.Marshal(api.CLIRefreshRequest{RefreshToken: c.RefreshToken, Client: client})
		return postRelay(ctx, c.Server, "/api/v1/auth/cli/revoke", body, nil)
	}
	clientID := cfg.DeviceClientID
	if clientID == "" {
		clientID = cfg.ClientID
	}
	return directRevoke(ctx, cfg.RevocationEndpoint, clientID, c.RefreshToken)
}

// directRevoke performs an RFC 7009 revocation at the IdP as a public client.
func directRevoke(ctx context.Context, endpoint, clientID, refreshToken string) error {
	form := url.Values{
		"client_id":       {clientID},
		"token":           {refreshToken},
		"token_type_hint": {"refresh_token"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return &refreshError{Reason: "invalid revocation endpoint"}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &refreshError{Reason: "could not reach the identity provider"}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	var oe struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &oe) == nil && oe.Error != "" {
		return &refreshError{Reason: oauthCode(oe.Error)}
	}
	return &refreshError{Reason: fmt.Sprintf("identity provider returned %d", resp.StatusCode)}
}
