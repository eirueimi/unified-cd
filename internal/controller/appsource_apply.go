package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/store"
)

// errStoreWrite wraps a failure from the store layer. The reconciler aborts the
// whole sync on this (infrastructure failure) but skips the single file on any
// other error (parse failure, unknown kind).
var errStoreWrite = errors.New("store write failed")

// probeKind reads only the top-level "kind" field of a YAML document.
func probeKind(doc []byte) string {
	var probe struct {
		Kind string `yaml:"kind"`
	}
	_ = yaml.Unmarshal(doc, &probe)
	return probe.Kind
}

// probeName reads only the "metadata.name" field of a YAML document. Used to
// detect duplicate {kind,name} resources before applyResource writes to the
// store, so the first file (in sorted order) wins.
func probeName(doc []byte) string {
	var probe struct {
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
	}
	_ = yaml.Unmarshal(doc, &probe)
	return probe.Metadata.Name
}

// applyResource parses one synced document by kind and upserts it, returning metadata.name.
// Parse/unknown-kind failures return a bare error (skippable); store failures are
// wrapped with errStoreWrite (abort). Never panics.
func applyResource(ctx context.Context, st store.Store, kind, dir string, doc []byte) (string, error) {
	switch kind {
	case "Job":
		job, err := dsl.Parse(strings.NewReader(string(doc)))
		if err != nil {
			return "", err
		}
		if job.Metadata.Annotations == nil {
			job.Metadata.Annotations = map[string]string{}
		}
		job.Metadata.Annotations["path"] = dir
		specJSON, err := json.Marshal(job.Spec)
		if err != nil {
			return "", err
		}
		name := job.Metadata.QualifiedName()
		if _, err := st.UpsertJob(ctx, name, job.APIVersion, specJSON); err != nil {
			return "", fmt.Errorf("%w: %v", errStoreWrite, err)
		}
		return name, nil
	case "Schedule":
		sc, err := dsl.ParseSchedule(strings.NewReader(string(doc)))
		if err != nil {
			return "", err
		}
		if _, err := st.UpsertSchedule(ctx, sc.Metadata.Name, sc.Spec.Cron, sc.Spec.Job, sc.Spec.Params); err != nil {
			return "", fmt.Errorf("%w: %v", errStoreWrite, err)
		}
		return sc.Metadata.Name, nil
	case "WebhookReceiver":
		wr, err := dsl.ParseWebhookReceiver(strings.NewReader(string(doc)))
		if err != nil {
			return "", err
		}
		specJSON, err := json.Marshal(wr.Spec)
		if err != nil {
			return "", err
		}
		if _, err := st.UpsertWebhookReceiver(ctx, wr.Metadata.Name, specJSON); err != nil {
			return "", fmt.Errorf("%w: %v", errStoreWrite, err)
		}
		return wr.Metadata.Name, nil
	case "GitCredential":
		gc, err := dsl.ParseGitCredential(strings.NewReader(string(doc)))
		if err != nil {
			return "", err
		}
		if err := st.UpsertGitCredential(ctx, gc.Metadata.Name, gc.Spec.Host, gc.Spec.Type, gc.Spec.SecretRef); err != nil {
			return "", fmt.Errorf("%w: %v", errStoreWrite, err)
		}
		return gc.Metadata.Name, nil
	case "AppSource":
		as, err := dsl.ParseAppSource(strings.NewReader(string(doc)))
		if err != nil {
			return "", err
		}
		specJSON, err := json.Marshal(as.Spec)
		if err != nil {
			return "", err
		}
		if _, err := st.UpsertAppSource(ctx, as.Metadata.Name, specJSON); err != nil {
			return "", fmt.Errorf("%w: %v", errStoreWrite, err)
		}
		return as.Metadata.Name, nil
	default:
		return "", fmt.Errorf("unsupported kind %q", kind)
	}
}

// deleteResource removes a previously-managed resource by kind and name.
// For kind "AppSource" this deletes only the app_sources row (non-cascading).
func deleteResource(ctx context.Context, st store.Store, kind, name string) error {
	switch kind {
	case "Job":
		return st.DeleteJob(ctx, name)
	case "Schedule":
		return st.DeleteSchedule(ctx, name)
	case "WebhookReceiver":
		return st.DeleteWebhookReceiver(ctx, name)
	case "GitCredential":
		return st.DeleteGitCredential(ctx, name)
	case "AppSource":
		return st.DeleteAppSource(ctx, name)
	default:
		return fmt.Errorf("unsupported kind %q", kind)
	}
}
