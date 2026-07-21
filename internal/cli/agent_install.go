package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/config"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// AgentConfig holds the runtime configuration for an agent.
type AgentConfig struct {
	Server              string   `yaml:"server"`
	Token               string   `yaml:"token,omitempty"`
	CredentialFile      string   `yaml:"credentialFile,omitempty"`
	EnrollmentTokenFile string   `yaml:"enrollmentTokenFile,omitempty"`
	AgentID             string   `yaml:"agentId"`
	BinPath             string   `yaml:"binPath,omitempty"`
	Labels              []string `yaml:"labels,omitempty"`
}

func newAgentCmd(resolve func() (Config, error)) *cobra.Command {
	return newAgentCmdWithClient(resolve, http.DefaultClient)
}

func newAgentCmdWithClient(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Agent management commands",
	}
	cmd.AddCommand(newAgentInstallCmd())
	cmd.AddCommand(newAgentUninstallCmd())
	cmd.AddCommand(newAgentListCmd(resolve, httpClient))
	cmd.AddCommand(newAgentGetCmd(resolve, httpClient))
	cmd.AddCommand(newAgentRunsCmd(resolve, httpClient))
	cmd.AddCommand(newAgentEnrollmentCmd(resolve, httpClient))
	cmd.AddCommand(newAgentEnrollmentPolicyCmd(resolve, httpClient))
	cmd.AddCommand(newAgentIdentityCmd(resolve, httpClient))
	return cmd
}

func newAgentListCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List registered agents",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
				cfg.Server+"/api/v1/agents", nil)
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
			resp, err := httpClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("server: %s", string(b))
			}
			var list []api.AgentInfo
			if err := json.Unmarshal(b, &list); err != nil {
				return err
			}
			if len(list) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(no agents)")
				return nil
			}
			for _, a := range list {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\t%s\n",
					a.ID, a.Hostname, a.OS, strings.Join(a.Labels, ","), a.LastSeenAt.Format("2006-01-02 15:04"))
			}
			return nil
		},
	}
}

func newAgentGetCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "get <agent-id>",
		Short: "Show details of a registered agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
				cfg.Server+"/api/v1/agents/"+args[0], nil)
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
			resp, err := httpClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("server: %s", string(b))
			}
			var a api.AgentInfo
			if err := json.Unmarshal(b, &a); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "ID:         %s\n", a.ID)
			fmt.Fprintf(out, "Hostname:   %s\n", a.Hostname)
			fmt.Fprintf(out, "OS:         %s\n", a.OS)
			fmt.Fprintf(out, "Labels:     %s\n", strings.Join(a.Labels, ","))
			fmt.Fprintf(out, "Version:    %s\n", a.Version)
			fmt.Fprintf(out, "LastSeenAt: %s\n", a.LastSeenAt.Local().Format("2006-01-02 15:04:05"))
			if len(a.Env) > 0 {
				fmt.Fprintf(out, "Env:\n")
				for k, v := range a.Env {
					fmt.Fprintf(out, "  %s=%s\n", k, v)
				}
			}
			return nil
		},
	}
}

func newAgentRunsCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "runs <agent-id>",
		Short: "List recent runs claimed by an agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
				cfg.Server+"/api/v1/agents/"+args[0]+"/runs", nil)
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
			resp, err := httpClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("server: %s", string(b))
			}
			var list []api.Run
			if err := json.Unmarshal(b, &list); err != nil {
				return err
			}
			if len(list) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(no runs)")
				return nil
			}
			for _, r := range list {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\t%s\n",
					r.ID, r.JobName, r.Status, r.CreatedAt.Format("2006-01-02"), r.TriggeredBy)
			}
			return nil
		},
	}
}

func newAgentInstallCmd() *cobra.Command {
	var server, credentialFile, enrollmentTokenFile, agentID string
	var labels []string
	var installDir string

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the agent as a system service (systemd/launchd)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if credentialFile == "" {
				// Match the agent runtime default so the generated service uses
				// the same path the agent would pick on its own.
				defaultPath, err := config.DefaultAgentCredentialFile(agentID)
				if err != nil {
					return err
				}
				credentialFile = defaultPath
			}
			if enrollmentTokenFile == "" {
				if _, err := os.Stat(credentialFile); os.IsNotExist(err) {
					return fmt.Errorf("enrollment token file is required when credential file does not exist")
				} else if err != nil {
					return fmt.Errorf("credential file: %w", err)
				}
			}
			binPath, err := os.Executable()
			if err != nil {
				binPath = "unified-cd"
			}

			cfg := AgentConfig{
				Server:              server,
				CredentialFile:      credentialFile,
				EnrollmentTokenFile: enrollmentTokenFile,
				AgentID:             agentID,
				BinPath:             binPath,
				Labels:              labels,
			}

			if installDir == "" {
				home, _ := os.UserHomeDir()
				installDir = filepath.Join(home, ".unified-cd")
			}
			if err := os.MkdirAll(installDir, 0o750); err != nil {
				return fmt.Errorf("failed to create install directory: %w", err)
			}

			configPath := filepath.Join(installDir, "agent.yaml")
			if err := writeAgentConfig(configPath, cfg); err != nil {
				return fmt.Errorf("failed to write agent config file: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Agent config written to %s\n", configPath)

			switch runtime.GOOS {
			case "linux":
				return installLinux(cmd, cfg, installDir)
			case "darwin":
				return installDarwin(cmd, cfg, installDir)
			default:
				fmt.Fprintf(cmd.OutOrStdout(), "\nWindows: run the agent manually or use Task Scheduler:\n")
				fmt.Fprintf(cmd.OutOrStdout(), "  %s agent --server=%s --id=%s%s\n",
					binPath, server, agentID, agentCredentialArgs(cfg, " "))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&server, "server", "", "master server URL (required)")
	cmd.Flags().StringVar(&credentialFile, "credential-file", "", "path for persistent VM refresh credentials")
	cmd.Flags().StringVar(&enrollmentTokenFile, "enrollment-token-file", "", "path for one-time enrollment token")
	cmd.Flags().StringVar(&agentID, "id", "", "agent ID (required)")
	cmd.Flags().StringArrayVar(&labels, "label", nil, "agent label (repeatable, e.g. --label kind:linux)")
	cmd.Flags().StringVar(&installDir, "dir", "", "install directory (default: ~/.unified-cd)")
	_ = cmd.MarkFlagRequired("server")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

func newAgentUninstallCmd() *cobra.Command {
	var installDir, agentID string
	var purgeCredentials bool

	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove agent service files generated by 'agent install'",
		RunE: func(cmd *cobra.Command, args []string) error {
			if purgeCredentials && agentID == "" {
				return fmt.Errorf("--id is required with --purge-credentials")
			}
			if installDir == "" {
				home, _ := os.UserHomeDir()
				installDir = filepath.Join(home, ".unified-cd")
			}
			out := cmd.OutOrStdout()

			removed := reportRemove(out, filepath.Join(installDir, "agent.yaml"))

			switch runtime.GOOS {
			case "linux":
				removed += reportRemove(out, filepath.Join(installDir, "systemd", "unified-cd-agent.service"))
				fmt.Fprintf(out, "\nIf you enabled the service, also run:\n")
				fmt.Fprintf(out, "  systemctl --user disable --now unified-cd-agent\n")
				fmt.Fprintf(out, "  rm -f ~/.config/systemd/user/unified-cd-agent.service\n")
				fmt.Fprintf(out, "  systemctl --user daemon-reload\n")
			case "darwin":
				removed += reportRemove(out, filepath.Join(installDir, "launchd", "dev.unified-cd.agent.plist"))
				fmt.Fprintf(out, "\nIf you loaded the agent, also run:\n")
				fmt.Fprintf(out, "  launchctl unload ~/Library/LaunchAgents/dev.unified-cd.agent.plist\n")
				fmt.Fprintf(out, "  rm -f ~/Library/LaunchAgents/dev.unified-cd.agent.plist\n")
			default:
				fmt.Fprintf(out, "\nWindows: stop the agent process or remove its Task Scheduler entry manually.\n")
			}

			if removed == 0 {
				fmt.Fprintf(out, "\nNo generated service files were found under %s.\n", installDir)
			}

			if purgeCredentials {
				credPath, err := config.DefaultAgentCredentialFile(agentID)
				if err != nil {
					return err
				}
				credDir := filepath.Dir(credPath)
				if err := os.RemoveAll(credDir); err != nil {
					return fmt.Errorf("remove credential directory: %w", err)
				}
				fmt.Fprintf(out, "\nRemoved credential directory %s\n", credDir)
			} else {
				fmt.Fprintf(out, "\nAgent credentials were left in place. Re-run with --purge-credentials --id <id> to delete the default $HOME/.unified-cd/<id>/ credential directory.\n")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&installDir, "dir", "", "install directory (default: ~/.unified-cd)")
	cmd.Flags().StringVar(&agentID, "id", "", "agent ID (required with --purge-credentials)")
	cmd.Flags().BoolVar(&purgeCredentials, "purge-credentials", false, "also delete the default $HOME/.unified-cd/<id>/ credential directory")
	return cmd
}

// reportRemove deletes path if it exists, printing the outcome to out. It
// returns 1 when a file was removed and 0 otherwise (missing file, or a removal
// error, which is reported but not fatal so the rest of uninstall proceeds).
func reportRemove(out io.Writer, path string) int {
	if _, err := os.Stat(path); err != nil {
		return 0
	}
	if err := os.Remove(path); err != nil {
		fmt.Fprintf(out, "Failed to remove %s: %v\n", path, err)
		return 0
	}
	fmt.Fprintf(out, "Removed %s\n", path)
	return 1
}

// writeAgentConfig writes the agent configuration to a YAML file.
func writeAgentConfig(path string, cfg AgentConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// generateSystemdUnit generates the contents of a systemd unit file.
func generateSystemdUnit(cfg AgentConfig) string {
	labelsFlag := ""
	if len(cfg.Labels) > 0 {
		labelsFlag = " --labels=" + strings.Join(cfg.Labels, ",")
	}
	credentialArgs := agentCredentialArgs(cfg, " ")
	return fmt.Sprintf(`[Unit]
Description=unified-cd Agent (%s)
After=network.target

[Service]
Type=simple
ExecStart=%s agent --server=%s --id=%s%s%s
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, cfg.AgentID, cfg.BinPath, cfg.Server, cfg.AgentID, credentialArgs, labelsFlag)
}

// generateLaunchdPlist generates the contents of a launchd property list.
func generateLaunchdPlist(cfg AgentConfig) string {
	labelsArg := ""
	if len(cfg.Labels) > 0 {
		labelsArg = fmt.Sprintf("\t\t<string>--labels=%s</string>\n", strings.Join(cfg.Labels, ","))
	}
	credentialArgs := ""
	if cfg.CredentialFile != "" {
		credentialArgs += fmt.Sprintf("\t\t<string>--credential-file=%s</string>\n", cfg.CredentialFile)
	}
	if cfg.EnrollmentTokenFile != "" {
		credentialArgs += fmt.Sprintf("\t\t<string>--enrollment-token-file=%s</string>\n", cfg.EnrollmentTokenFile)
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>dev.unified-cd.agent</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>agent</string>
    <string>--server=%s</string>
    <string>--id=%s</string>
%s%s  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
</dict>
</plist>
`, cfg.BinPath, cfg.Server, cfg.AgentID, credentialArgs, labelsArg)
}

func agentCredentialArgs(cfg AgentConfig, separator string) string {
	args := ""
	if cfg.CredentialFile != "" {
		args += separator + "--credential-file=" + cfg.CredentialFile
	}
	if cfg.EnrollmentTokenFile != "" {
		args += separator + "--enrollment-token-file=" + cfg.EnrollmentTokenFile
	}
	return args
}

// installLinux writes the systemd unit file to the install directory.
func installLinux(cmd *cobra.Command, cfg AgentConfig, installDir string) error {
	unit := generateSystemdUnit(cfg)
	unitDir := filepath.Join(installDir, "systemd")
	_ = os.MkdirAll(unitDir, 0o750)
	unitPath := filepath.Join(unitDir, "unified-cd-agent.service")
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("failed to write systemd unit file: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Systemd unit written to %s\n", unitPath)
	fmt.Fprintf(cmd.OutOrStdout(), "\nTo enable:\n")
	fmt.Fprintf(cmd.OutOrStdout(), "  cp %s ~/.config/systemd/user/\n", unitPath)
	fmt.Fprintf(cmd.OutOrStdout(), "  systemctl --user daemon-reload\n")
	fmt.Fprintf(cmd.OutOrStdout(), "  systemctl --user enable --now unified-cd-agent\n")
	return nil
}

// installDarwin writes the launchd property list to the install directory.
func installDarwin(cmd *cobra.Command, cfg AgentConfig, installDir string) error {
	plist := generateLaunchdPlist(cfg)
	plistDir := filepath.Join(installDir, "launchd")
	_ = os.MkdirAll(plistDir, 0o750)
	plistPath := filepath.Join(plistDir, "dev.unified-cd.agent.plist")
	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("failed to write launchd property list: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "launchd plist written to %s\n", plistPath)
	fmt.Fprintf(cmd.OutOrStdout(), "\nTo enable:\n")
	fmt.Fprintf(cmd.OutOrStdout(), "  cp %s ~/Library/LaunchAgents/\n", plistPath)
	fmt.Fprintf(cmd.OutOrStdout(), "  launchctl load ~/Library/LaunchAgents/dev.unified-cd.agent.plist\n")
	return nil
}
