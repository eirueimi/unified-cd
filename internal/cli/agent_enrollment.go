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

func newAgentEnrollmentPolicyCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	cmd := &cobra.Command{Use: "enrollment-policy", Short: "Manage Kubernetes agent enrollment policies"}
	cmd.AddCommand(newAgentEnrollmentPolicyWriteCmd(resolve, httpClient, "create"))
	cmd.AddCommand(newAgentEnrollmentPolicyWriteCmd(resolve, httpClient, "update"))
	cmd.AddCommand(newAgentEnrollmentPolicyGetCmd(resolve, httpClient))
	cmd.AddCommand(newAgentEnrollmentPolicyListCmd(resolve, httpClient))
	cmd.AddCommand(newAgentEnrollmentPolicyDeleteCmd(resolve, httpClient))
	return cmd
}
func newAgentEnrollmentPolicyWriteCmd(resolve func() (Config, error), client *http.Client, action string) *cobra.Command {
	var cluster, template, ttl string
	var namespaces, serviceAccounts, allowed, required, capabilities []string
	var enabled bool
	cmd := &cobra.Command{Use: action + " <name>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := resolve()
		if err != nil {
			return err
		}
		body, err := json.Marshal(api.AgentEnrollmentPolicyRequest{Name: args[0], Provider: "kubernetes", Cluster: cluster, Namespaces: namespaces, ServiceAccounts: serviceAccounts, AgentIDTemplate: template, AllowedLabels: allowed, RequiredLabels: required, Capabilities: capabilities, AccessTokenTTL: ttl, Enabled: enabled})
		if err != nil {
			return err
		}
		method, path := http.MethodPost, "/api/v1/agent-enrollment-policies"
		if action == "update" {
			method = http.MethodPut
			path += "/" + url.PathEscape(args[0])
		}
		resp, _, err := agentLifecycleRequest(context.Background(), client, cfg, method, path, bytes.NewReader(body))
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			return agentLifecycleStatusError(resp)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "enrollment policy %q %sd\n", args[0], action)
		return nil
	}}
	cmd.Flags().StringVar(&cluster, "cluster", "", "configured Kubernetes cluster name")
	cmd.Flags().StringArrayVar(&namespaces, "namespace", nil, "allowed namespace (repeatable)")
	cmd.Flags().StringArrayVar(&serviceAccounts, "service-account", nil, "allowed service account (repeatable)")
	cmd.Flags().StringVar(&template, "agent-id-template", "k8s:{cluster}:{namespace}:{podUID}", "canonical agent ID template")
	cmd.Flags().StringArrayVar(&allowed, "allowed-label", nil, "allowed agent label (repeatable)")
	cmd.Flags().StringArrayVar(&required, "required-label", nil, "required agent label (repeatable)")
	cmd.Flags().StringArrayVar(&capabilities, "capability", nil, "authorized capability (repeatable)")
	cmd.Flags().StringVar(&ttl, "access-token-ttl", "1h", "access token lifetime (5m to 4h)")
	cmd.Flags().BoolVar(&enabled, "enabled", true, "enable policy")
	_ = cmd.MarkFlagRequired("cluster")
	_ = cmd.MarkFlagRequired("namespace")
	_ = cmd.MarkFlagRequired("service-account")
	return cmd
}
func newAgentEnrollmentPolicyGetCmd(resolve func() (Config, error), client *http.Client) *cobra.Command {
	return &cobra.Command{Use: "get <name>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := resolve()
		if err != nil {
			return err
		}
		resp, body, err := agentLifecycleRequest(context.Background(), client, cfg, http.MethodGet, "/api/v1/agent-enrollment-policies/"+url.PathEscape(args[0]), nil)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return agentLifecycleStatusError(resp)
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(body))
		return nil
	}}
}
func newAgentEnrollmentPolicyListCmd(resolve func() (Config, error), client *http.Client) *cobra.Command {
	return &cobra.Command{Use: "list", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := resolve()
		if err != nil {
			return err
		}
		resp, body, err := agentLifecycleRequest(context.Background(), client, cfg, http.MethodGet, "/api/v1/agent-enrollment-policies", nil)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return agentLifecycleStatusError(resp)
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(body))
		return nil
	}}
}
func newAgentEnrollmentPolicyDeleteCmd(resolve func() (Config, error), client *http.Client) *cobra.Command {
	return &cobra.Command{Use: "delete <name>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := resolve()
		if err != nil {
			return err
		}
		resp, _, err := agentLifecycleRequest(context.Background(), client, cfg, http.MethodDelete, "/api/v1/agent-enrollment-policies/"+url.PathEscape(args[0]), nil)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusNoContent {
			return agentLifecycleStatusError(resp)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "enrollment policy %q deleted\n", args[0])
		return nil
	}}
}

func newAgentEnrollmentCreateCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	var agentID, expiresIn, outputFile string
	var labels, capabilities []string
	var quiet bool
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
			if quiet {
				fmt.Fprintln(cmd.OutOrStdout(), result.Token)
				return nil
			}
			if outputFile != "" {
				if err := writeNewEnrollmentTokenFile(outputFile, result.Token); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Enrollment token written to %s (shown only once).\n", outputFile)
				fmt.Fprint(cmd.OutOrStdout(), nextAgentCommands(cfg.Server, agentID, outputFile, ""))
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Enrollment token created (shown only once):\n\n%s\n", result.Token)
			fmt.Fprint(cmd.OutOrStdout(), nextAgentCommands(cfg.Server, agentID, "", result.Token))
			return nil
		},
	}
	cmd.Flags().StringVar(&agentID, "agent-id", "", "fixed canonical agent ID (required)")
	cmd.Flags().StringVar(&expiresIn, "expires-in", "10m", "positive enrollment-token lifetime")
	cmd.Flags().StringArrayVar(&labels, "label", nil, "authorized agent label (repeatable)")
	cmd.Flags().StringArrayVar(&capabilities, "capability", nil, "authorized agent capability (repeatable)")
	cmd.Flags().StringVar(&outputFile, "output-file", "", "create a new owner-only file containing the token")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "print only the token (for piping to unified-cd-agent --enrollment-token -)")
	cmd.MarkFlagsMutuallyExclusive("quiet", "output-file")
	_ = cmd.MarkFlagRequired("agent-id")
	return cmd
}

// nextAgentCommands renders the command an operator can run on the agent host
// after creating an enrollment token: a direct `unified-cd-agent` invocation.
// When tokenFile is set the token was written to that file, so the command
// references it via --enrollment-token-file. When tokenValue is set instead
// (no --output-file, no --quiet) the token was printed to stdout, so it is
// embedded inline via --enrollment-token with a shell-history/ps warning.
// Otherwise (neither set) a placeholder path and a save-the-token hint are
// shown. The credential file is intentionally omitted: the agent defaults it
// to $HOME/.unified-cd/<id>/credential.json.
func nextAgentCommands(server, agentID, tokenFile, tokenValue string) string {
	var b strings.Builder
	b.WriteString("\n")
	switch {
	case tokenFile != "":
		b.WriteString("Next, on the agent host, run the agent:\n")
		fmt.Fprintf(&b, "  unified-cd-agent \\\n    --server %s \\\n    --id %s \\\n    --enrollment-token-file %s\n",
			server, agentID, tokenFile)
	case tokenValue != "":
		b.WriteString("Next, on the agent host, run the agent (the token is visible in shell history/ps — prefer --output-file for shared hosts):\n")
		fmt.Fprintf(&b, "  unified-cd-agent \\\n    --server %s \\\n    --id %s \\\n    --enrollment-token %s\n",
			server, agentID, tokenValue)
	default:
		b.WriteString("Save this token to a private file on the agent host, then run the agent:\n")
		fmt.Fprintf(&b, "  unified-cd-agent \\\n    --server %s \\\n    --id %s \\\n    --enrollment-token-file <path-to-token-file>\n",
			server, agentID)
	}
	fmt.Fprintf(&b, "\nThe credential file defaults to $HOME/.unified-cd/%s/credential.json.\n", agentID)
	return b.String()
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
