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

	"github.com/spf13/cobra"
	"github.com/eirueimi/unified-cd/internal/api"
	"gopkg.in/yaml.v3"
)

// AgentConfig holds the runtime configuration for an agent.
type AgentConfig struct {
	Server  string   `yaml:"server"`
	Token   string   `yaml:"token"`
	AgentID string   `yaml:"agentId"`
	BinPath string   `yaml:"binPath,omitempty"`
	Labels  []string `yaml:"labels,omitempty"`
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
	cmd.AddCommand(newAgentListCmd(resolve, httpClient))
	cmd.AddCommand(newAgentGetCmd(resolve, httpClient))
	cmd.AddCommand(newAgentRunsCmd(resolve, httpClient))
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
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
				cfg.Server+"/api/v1/agents", nil)
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
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
				cfg.Server+"/api/v1/agents/"+args[0], nil)
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
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
				cfg.Server+"/api/v1/agents/"+args[0]+"/runs", nil)
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
	var server, token, agentID string
	var labels []string
	var installDir string

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the agent as a system service (systemd/launchd)",
		RunE: func(cmd *cobra.Command, args []string) error {
			binPath, err := os.Executable()
			if err != nil {
				binPath = "unified-cd"
			}

			cfg := AgentConfig{
				Server:  server,
				Token:   token,
				AgentID: agentID,
				BinPath: binPath,
				Labels:  labels,
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
				fmt.Fprintf(cmd.OutOrStdout(), "  %s agent --server=%s --token=%s --id=%s\n",
					binPath, server, token, agentID)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&server, "server", "", "master server URL (required)")
	cmd.Flags().StringVar(&token, "token", "", "agent token (required)")
	cmd.Flags().StringVar(&agentID, "id", "", "agent ID (required)")
	cmd.Flags().StringArrayVar(&labels, "label", nil, "agent label (repeatable, e.g. --label kind:linux)")
	cmd.Flags().StringVar(&installDir, "dir", "", "install directory (default: ~/.unified-cd)")
	_ = cmd.MarkFlagRequired("server")
	_ = cmd.MarkFlagRequired("token")
	_ = cmd.MarkFlagRequired("id")
	return cmd
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
	return fmt.Sprintf(`[Unit]
Description=unified-cd Agent (%s)
After=network.target

[Service]
Type=simple
ExecStart=%s agent --server=%s --token=%s --id=%s%s
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, cfg.AgentID, cfg.BinPath, cfg.Server, cfg.Token, cfg.AgentID, labelsFlag)
}

// generateLaunchdPlist generates the contents of a launchd property list.
func generateLaunchdPlist(cfg AgentConfig) string {
	labelsArg := ""
	if len(cfg.Labels) > 0 {
		labelsArg = fmt.Sprintf("\t\t<string>--labels=%s</string>\n", strings.Join(cfg.Labels, ","))
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
    <string>--token=%s</string>
    <string>--id=%s</string>
%s  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
</dict>
</plist>
`, cfg.BinPath, cfg.Server, cfg.Token, cfg.AgentID, labelsArg)
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
