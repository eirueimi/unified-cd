package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// reservedExportDirs are the top-level directories export uses for non-Job
// kinds. A Job whose qualified path starts with one of these would change
// meaning on re-import through an AppSource, so export fails instead.
var reservedExportDirs = map[string]bool{
	"schedules": true, "webhookreceivers": true, "gitcredentials": true, "appsources": true,
}

func newExportCmd(resolve func() (Config, error)) *cobra.Command {
	return newExportCmdWithClient(resolve, http.DefaultClient)
}

func newExportCmdWithClient(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	var outDir string
	var unmanagedOnly, force bool
	cmd := &cobra.Command{
		Use:   "export",
		Short: "export all resources as a YAML tree consumable by an AppSource",
		Long: `Export Jobs, Schedules, WebhookReceivers, GitCredentials and AppSources as
one YAML file per resource. Jobs are placed at their qualified path so the
output directory can be committed to Git and used directly as an AppSource
path. Secret values cannot be exported.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			return runExport(cmd.OutOrStdout(), cfg, httpClient, outDir, unmanagedOnly, force)
		},
	}
	cmd.Flags().StringVarP(&outDir, "output", "o", "", "output directory (required)")
	cmd.Flags().BoolVar(&unmanagedOnly, "unmanaged-only", false, "export only resources not managed by any AppSource")
	cmd.Flags().BoolVar(&force, "force", false, "write into a non-empty directory")
	_ = cmd.MarkFlagRequired("output")
	return cmd
}

// exportDoc is one exported resource document. Field order is fixed by the
// struct so every file starts with apiVersion/kind/metadata/spec.
type exportDoc struct {
	APIVersion string         `yaml:"apiVersion"`
	Kind       string         `yaml:"kind"`
	Metadata   exportMetadata `yaml:"metadata"`
	Spec       any            `yaml:"spec"`
}

type exportMetadata struct {
	Name string `yaml:"name"`
}

func runExport(out io.Writer, cfg Config, httpClient *http.Client, outDir string, unmanagedOnly, force bool) error {
	if err := ensureExportDir(outDir, force); err != nil {
		return err
	}

	var appsources []api.AppSourceMeta
	if err := getJSON(cfg, httpClient, "/api/v1/appsources", &appsources); err != nil {
		return fmt.Errorf("list appsources: %w", err)
	}
	managed := map[string]bool{}
	for _, a := range appsources {
		for _, ref := range a.ManagedResources {
			managed[ref.Kind+"\x00"+ref.Name] = true
		}
	}
	skip := func(kind, name string) bool {
		return unmanagedOnly && managed[kind+"\x00"+name]
	}
	exported, skipped := 0, 0

	// Jobs: placed at their qualified path so an AppSource pointing at outDir
	// reproduces the same qualified names (metadata.name is the leaf).
	var jobs []api.Job
	if err := getJSON(cfg, httpClient, "/api/v1/jobs", &jobs); err != nil {
		return fmt.Errorf("list jobs: %w", err)
	}
	for _, j := range jobs {
		if skip("Job", j.Name) {
			skipped++
			continue
		}
		if first := strings.SplitN(j.Path, "/", 2)[0]; j.Path != "" && reservedExportDirs[first] {
			return fmt.Errorf("job %q: path segment %q collides with a reserved export directory (%s); rename the job path", j.Name, first, first)
		}
		spec, err := jobSpecForExport(j.Spec)
		if err != nil {
			return fmt.Errorf("job %q: parse spec: %w", j.Name, err)
		}
		apiVersion := j.APIVersion
		if apiVersion == "" {
			apiVersion = "unified-cd/v1"
		}
		doc := exportDoc{APIVersion: apiVersion, Kind: "Job", Metadata: exportMetadata{Name: j.Leaf}, Spec: spec}
		if err := writeExportDoc(filepath.Join(outDir, filepath.FromSlash(j.Path), j.Leaf+".yaml"), doc); err != nil {
			return err
		}
		exported++
	}

	var schedules []api.ScheduleMeta
	if err := getJSON(cfg, httpClient, "/api/v1/schedules", &schedules); err != nil {
		return fmt.Errorf("list schedules: %w", err)
	}
	for _, sc := range schedules {
		if skip("Schedule", sc.Name) {
			skipped++
			continue
		}
		spec := map[string]any{"cron": sc.Cron, "job": sc.JobName}
		if len(sc.Params) > 0 {
			spec["params"] = sc.Params
		}
		doc := exportDoc{APIVersion: "unified-cd/v1", Kind: "Schedule", Metadata: exportMetadata{Name: sc.Name}, Spec: spec}
		if err := writeExportDoc(filepath.Join(outDir, "schedules", sc.Name+".yaml"), doc); err != nil {
			return err
		}
		exported++
	}

	var webhooks []api.WebhookReceiverMeta
	if err := getJSON(cfg, httpClient, "/api/v1/webhooks", &webhooks); err != nil {
		return fmt.Errorf("list webhooks: %w", err)
	}
	for _, wr := range webhooks {
		if skip("WebhookReceiver", wr.Name) {
			skipped++
			continue
		}
		spec, err := webhookSpecForExport(wr.Spec)
		if err != nil {
			return fmt.Errorf("webhookreceiver %q: parse spec: %w", wr.Name, err)
		}
		doc := exportDoc{APIVersion: "unified-cd/v1", Kind: "WebhookReceiver", Metadata: exportMetadata{Name: wr.Name}, Spec: spec}
		if err := writeExportDoc(filepath.Join(outDir, "webhookreceivers", wr.Name+".yaml"), doc); err != nil {
			return err
		}
		exported++
	}

	var creds []api.GitCredentialMeta
	if err := getJSON(cfg, httpClient, "/api/v1/gitcredentials", &creds); err != nil {
		return fmt.Errorf("list gitcredentials: %w", err)
	}
	for _, gc := range creds {
		if skip("GitCredential", gc.Name) {
			skipped++
			continue
		}
		spec := map[string]any{"host": gc.Host, "type": gc.CredType, "secretRef": gc.SecretRef}
		doc := exportDoc{APIVersion: "unified-cd/v1", Kind: "GitCredential", Metadata: exportMetadata{Name: gc.Name}, Spec: spec}
		if err := writeExportDoc(filepath.Join(outDir, "gitcredentials", gc.Name+".yaml"), doc); err != nil {
			return err
		}
		exported++
	}

	for _, a := range appsources {
		if skip("AppSource", a.Name) {
			skipped++
			continue
		}
		spec := map[string]any{"repoURL": a.RepoURL, "targetRevision": a.TargetRevision, "path": a.Path}
		if a.SyncPolicy != nil {
			sp := map[string]any{}
			if a.SyncPolicy.Interval != "" {
				sp["interval"] = a.SyncPolicy.Interval
			}
			if a.SyncPolicy.Prune {
				sp["prune"] = true
			}
			if a.SyncPolicy.AllowManualOverride {
				sp["allowManualOverride"] = true
			}
			if len(sp) > 0 {
				spec["syncPolicy"] = sp
			}
		}
		doc := exportDoc{APIVersion: "unified-cd/v1", Kind: "AppSource", Metadata: exportMetadata{Name: a.Name}, Spec: spec}
		if err := writeExportDoc(filepath.Join(outDir, "appsources", a.Name+".yaml"), doc); err != nil {
			return err
		}
		exported++
	}

	fmt.Fprintf(out, "exported %d resources (%d skipped as managed); secrets are not exported\n", exported, skipped)
	return nil
}

// ensureExportDir creates the output directory, refusing a non-empty one
// unless force is set.
func ensureExportDir(dir string, force bool) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return os.MkdirAll(dir, 0o755)
	}
	if err != nil {
		return err
	}
	if len(entries) > 0 && !force {
		return fmt.Errorf("output directory %s is not empty (use --force to overwrite)", dir)
	}
	return nil
}

// jobSpecForExport parses stored Job spec JSON into a typed dsl.Spec so
// yaml.Marshal renders canonical lowercase yaml-tag keys, regardless of
// whether the stored JSON itself has capitalized Go field names (the store
// holds json.Marshal(job.Spec) on a yaml-tag-only struct, which produces
// "Steps", "Name", etc.) or lowercase keys. json.Unmarshal is case-insensitive
// on field names, so both forms decode correctly here.
func jobSpecForExport(spec []byte) (dsl.Spec, error) {
	var s dsl.Spec
	if len(spec) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(spec, &s); err != nil {
		return dsl.Spec{}, err
	}
	return s, nil
}

// webhookSpecForExport is the WebhookReceiver analog of jobSpecForExport.
func webhookSpecForExport(spec []byte) (dsl.WebhookReceiverSpec, error) {
	var s dsl.WebhookReceiverSpec
	if len(spec) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(spec, &s); err != nil {
		return dsl.WebhookReceiverSpec{}, err
	}
	return s, nil
}

func writeExportDoc(path string, doc exportDoc) error {
	b, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// getJSON performs an authenticated GET against the controller and decodes the
// JSON response into v.
func getJSON(cfg Config, httpClient *http.Client, path string, v any) error {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, cfg.Server+path, nil)
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
	return json.Unmarshal(b, v)
}
