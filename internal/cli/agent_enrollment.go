package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/spf13/cobra"
)

func newAgentEnrollmentCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	cmd := &cobra.Command{Use: "enrollment", Short: "Manage one-time agent enrollment tokens"}
	cmd.AddCommand(newAgentEnrollmentCreateCmd(resolve, httpClient))
	cmd.AddCommand(newAgentEnrollmentListCmd(resolve, httpClient))
	cmd.AddCommand(newAgentEnrollmentRevokeCmd(resolve, httpClient))
	return cmd
}

func newAgentEnrollmentCreateCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	var agentID, expiresIn, outputFile string
	var labels, capabilities []string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a one-time agent enrollment token",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			body, err := json.Marshal(api.CreateAgentEnrollmentRequest{
				AgentID: agentID, ExpiresIn: expiresIn, Labels: labels, Capabilities: capabilities,
			})
			if err != nil {
				return err
			}
			resp, responseBody, err := agentLifecycleRequest(context.Background(), httpClient, cfg, http.MethodPost, "/api/v1/agent-enrollments", bytes.NewReader(body))
			if err != nil {
				return err
			}
			if resp.StatusCode != http.StatusCreated {
				return agentLifecycleStatusError(resp)
			}
			var result api.CreateAgentEnrollmentResponse
			if err := json.Unmarshal(responseBody, &result); err != nil {
				return err
			}
			if outputFile != "" {
				if err := writeNewEnrollmentTokenFile(outputFile, result.Token); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Enrollment token written to %s (shown only once).\n", outputFile)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Enrollment token created (shown only once):\n\n%s\n", result.Token)
			return nil
		},
	}
	cmd.Flags().StringVar(&agentID, "agent-id", "", "fixed canonical agent ID (required)")
	cmd.Flags().StringVar(&expiresIn, "expires-in", "10m", "positive enrollment-token lifetime")
	cmd.Flags().StringArrayVar(&labels, "label", nil, "authorized agent label (repeatable)")
	cmd.Flags().StringArrayVar(&capabilities, "capability", nil, "authorized agent capability (repeatable)")
	cmd.Flags().StringVar(&outputFile, "output-file", "", "create a new owner-only file containing the token")
	_ = cmd.MarkFlagRequired("agent-id")
	return cmd
}

func newAgentEnrollmentListCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List enrollment-token metadata",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			resp, body, err := agentLifecycleRequest(context.Background(), httpClient, cfg, http.MethodGet, "/api/v1/agent-enrollments", nil)
			if err != nil {
				return err
			}
			if resp.StatusCode != http.StatusOK {
				return agentLifecycleStatusError(resp)
			}
			var items []api.AgentEnrollmentMeta
			if err := json.Unmarshal(body, &items); err != nil {
				return err
			}
			if len(items) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(no enrollment tokens)")
				return nil
			}
			for _, item := range items {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", item.ID, item.AgentID, item.ExpiresAt.Format("2006-01-02 15:04"))
			}
			return nil
		},
	}
}

func newAgentEnrollmentRevokeCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <enrollment-id>",
		Short: "Revoke a one-time agent enrollment token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			resp, _, err := agentLifecycleRequest(context.Background(), httpClient, cfg, http.MethodDelete, "/api/v1/agent-enrollments/"+url.PathEscape(args[0]), nil)
			if err != nil {
				return err
			}
			if resp.StatusCode != http.StatusNoContent {
				return agentLifecycleStatusError(resp)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "enrollment token %q revoked\n", args[0])
			return nil
		},
	}
}

func newAgentIdentityCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	cmd := &cobra.Command{Use: "identity", Short: "Manage persistent agent identities"}
	cmd.AddCommand(newAgentIdentityGetCmd(resolve, httpClient))
	cmd.AddCommand(newAgentIdentityActionCmd(resolve, httpClient, "enable", "Enable an agent identity"))
	cmd.AddCommand(newAgentIdentityActionCmd(resolve, httpClient, "disable", "Disable an agent identity"))
	cmd.AddCommand(newAgentIdentityActionCmd(resolve, httpClient, "revoke-credentials", "Revoke all agent credentials"))
	return cmd
}

func newAgentIdentityGetCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "get <agent-id>",
		Short: "Show agent identity metadata",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			resp, body, err := agentLifecycleRequest(context.Background(), httpClient, cfg, http.MethodGet, "/api/v1/agent-identities/"+url.PathEscape(args[0]), nil)
			if err != nil {
				return err
			}
			if resp.StatusCode != http.StatusOK {
				return agentLifecycleStatusError(resp)
			}
			var identity api.AgentIdentityMeta
			if err := json.Unmarshal(body, &identity); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "ID: %s\nAgentID: %s\nStatus: %s\nEnrollmentMethod: %s\nLabels: %s\nCapabilities: %s\n",
				identity.ID, identity.AgentID, identity.Status, identity.EnrollmentMethod,
				strings.Join(identity.AuthorizedLabels, ","), strings.Join(identity.AuthorizedCapabilities, ","))
			return nil
		},
	}
}

func newAgentIdentityActionCmd(resolve func() (Config, error), httpClient *http.Client, action, short string) *cobra.Command {
	return &cobra.Command{
		Use:   action + " <agent-id>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			path := "/api/v1/agent-identities/" + url.PathEscape(args[0])
			if action == "revoke-credentials" {
				path += "/credentials/revoke"
			} else {
				path += "/" + action
			}
			resp, _, err := agentLifecycleRequest(context.Background(), httpClient, cfg, http.MethodPost, path, nil)
			if err != nil {
				return err
			}
			if resp.StatusCode != http.StatusNoContent {
				return agentLifecycleStatusError(resp)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "agent identity %q %s\n", args[0], action)
			return nil
		},
	}
}

func agentLifecycleRequest(ctx context.Context, client *http.Client, cfg Config, method, path string, body io.Reader) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(cfg.Server, "/")+path, body)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	return resp, responseBody, nil
}

type enrollmentTokenFile interface {
	io.Writer
	Close() error
}

type enrollmentTokenFileOpener func(name string, flag int, perm os.FileMode) (enrollmentTokenFile, error)

func writeNewEnrollmentTokenFile(path, token string) error {
	return writeNewEnrollmentTokenFileWith(path, token, func(name string, flag int, perm os.FileMode) (enrollmentTokenFile, error) {
		return os.OpenFile(name, flag, perm)
	})
}

func writeNewEnrollmentTokenFileWith(path, token string, open enrollmentTokenFileOpener) error {
	f, err := open(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create enrollment token output file: %w", err)
	}
	if _, err := fmt.Fprintln(f, token); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("write enrollment token output file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("close enrollment token output file: %w", err)
	}
	return nil
}

func agentLifecycleStatusError(resp *http.Response) error {
	return fmt.Errorf("server returned status %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
}
