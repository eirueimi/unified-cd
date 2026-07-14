package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// newApplyCmd creates a command that registers or updates a job YAML on the master server.
// Supports multi-document YAML separated by --- and applies each document in order.
func newApplyCmd(resolve func() (Config, error)) *cobra.Command {
	return newApplyCmdWithClient(resolve, http.DefaultClient)
}

func newApplyCmdWithClient(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	var file string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "apply (register / update) a job from a YAML file",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			if file == "" {
				return fmt.Errorf("--file is required")
			}
			f, err := os.Open(file)
			if err != nil {
				return err
			}
			defer f.Close()

			dec := yaml.NewDecoder(f)
			docIndex := 0
			for {
				// Read each document as a yaml.Node first, then reconstruct the raw YAML bytes
				var node yaml.Node
				if err := dec.Decode(&node); err != nil {
					if err == io.EOF {
						break
					}
					return fmt.Errorf("document %d: parse error: %w", docIndex+1, err)
				}
				// Skip empty documents (e.g. lines with only ---)
				if node.Kind == 0 {
					docIndex++
					continue
				}
				docBytes, err := yaml.Marshal(node.Content[0])
				if err != nil {
					return fmt.Errorf("document %d: marshal error: %w", docIndex+1, err)
				}

				if err := applyOneDocument(cmd, cfg, httpClient, docBytes, docIndex, dryRun); err != nil {
					return err
				}
				docIndex++
			}
			if docIndex == 0 {
				return fmt.Errorf("no YAML documents found in %s", file)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "path to job YAML")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "validate the YAML locally without applying it to the server")
	return cmd
}

// applyOneDocument parses a single YAML document and applies it to the server.
// When dryRun is true, the document is parsed and validated locally with the
// matching internal/dsl parser and reported; no HTTP request is made.
func applyOneDocument(cmd *cobra.Command, cfg Config, httpClient *http.Client, b []byte, docIndex int, dryRun bool) error {
	var kindProbe struct {
		Kind string `yaml:"kind"`
	}
	_ = yaml.Unmarshal(b, &kindProbe)

	if dryRun {
		return applyOneDocumentDryRun(cmd, kindProbe.Kind, b, docIndex)
	}

	var endpoint string
	var bodyBytes []byte
	switch kindProbe.Kind {
	case "GitCredential":
		var gc struct {
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Spec struct {
				Host      string `yaml:"host"`
				Type      string `yaml:"type"`
				SecretRef string `yaml:"secretRef"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal(b, &gc); err != nil {
			return fmt.Errorf("document %d: parse gitcredential yaml: %w", docIndex+1, err)
		}
		endpoint = cfg.Server + "/api/v1/gitcredentials"
		bodyBytes, _ = json.Marshal(api.UpsertGitCredentialRequest{
			Name:      gc.Metadata.Name,
			Host:      gc.Spec.Host,
			CredType:  gc.Spec.Type,
			SecretRef: gc.Spec.SecretRef,
		})
	case "WebhookReceiver":
		endpoint = cfg.Server + "/api/v1/webhooks/"
		bodyBytes, _ = json.Marshal(api.ApplyWebhookRequest{YAML: string(b)})
	case "AppSource":
		endpoint = cfg.Server + "/api/v1/appsources"
		bodyBytes, _ = json.Marshal(api.ApplyAppSourceRequest{YAML: string(b)})
	case "Schedule":
		endpoint = cfg.Server + "/api/v1/schedules/"
		bodyBytes, _ = json.Marshal(api.ApplyScheduleRequest{YAML: string(b)})
	default:
		endpoint = cfg.Server + "/api/v1/jobs"
		bodyBytes, _ = json.Marshal(api.ApplyJobRequest{YAML: string(b)})
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("document %d: server: %s", docIndex+1, string(respBody))
	}

	switch kindProbe.Kind {
	case "GitCredential":
		var kindMeta struct {
			Metadata struct{ Name string `yaml:"name"` } `yaml:"metadata"`
		}
		_ = yaml.Unmarshal(b, &kindMeta)
		fmt.Fprintf(cmd.OutOrStdout(), "gitcredential applied: %s\n", kindMeta.Metadata.Name)
	case "WebhookReceiver":
		var meta api.WebhookReceiverMeta
		if err := json.Unmarshal(respBody, &meta); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "webhook receiver applied: %s\n", meta.Name)
	case "AppSource":
		var meta api.AppSourceMeta
		if err := json.Unmarshal(respBody, &meta); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "appsource applied: %s\n", meta.Name)
	case "Schedule":
		var meta api.ScheduleMeta
		if err := json.Unmarshal(respBody, &meta); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "schedule applied: %s (cron=%s job=%s)\n", meta.Name, meta.Cron, meta.JobName)
	default:
		var job api.Job
		if err := json.Unmarshal(respBody, &job); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "job applied: %s\n", job.Name)
	}
	return nil
}

// applyOneDocumentDryRun parses and validates a single YAML document locally
// using the internal/dsl parser matching kind (no HTTP request). On success it
// reports "document N: <kind> "<name>" valid"; on failure it returns a
// wrapped parse/validation error.
func applyOneDocumentDryRun(cmd *cobra.Command, kind string, b []byte, docIndex int) error {
	kindLabel := kind
	if kindLabel == "" {
		kindLabel = "Job"
	}

	var name string
	switch kind {
	case "Schedule":
		s, err := dsl.ParseSchedule(bytes.NewReader(b))
		if err != nil {
			return fmt.Errorf("document %d: %w", docIndex+1, err)
		}
		name = s.Metadata.Name
	case "WebhookReceiver":
		w, err := dsl.ParseWebhookReceiver(bytes.NewReader(b))
		if err != nil {
			return fmt.Errorf("document %d: %w", docIndex+1, err)
		}
		name = w.Metadata.Name
	case "AppSource":
		a, err := dsl.ParseAppSource(bytes.NewReader(b))
		if err != nil {
			return fmt.Errorf("document %d: %w", docIndex+1, err)
		}
		name = a.Metadata.Name
	case "GitCredential":
		g, err := dsl.ParseGitCredential(bytes.NewReader(b))
		if err != nil {
			return fmt.Errorf("document %d: %w", docIndex+1, err)
		}
		name = g.Metadata.Name
	default:
		j, err := dsl.Parse(bytes.NewReader(b))
		if err != nil {
			return fmt.Errorf("document %d: %w", docIndex+1, err)
		}
		name = j.Metadata.Name
	}

	fmt.Fprintf(cmd.OutOrStdout(), "document %d: %s %q valid\n", docIndex+1, kindLabel, name)
	return nil
}
