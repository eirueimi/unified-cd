package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/unified-cd/unified-cd/internal/dsl"
	"github.com/unified-cd/unified-cd/internal/gittemplate"
	"github.com/unified-cd/unified-cd/internal/secrets"
	"github.com/unified-cd/unified-cd/internal/store"
)

// appSourceReconcilerLockKey is the advisory lock key used by the AppSource Reconciler.
const appSourceReconcilerLockKey = int64(0x61707073)

// AppSourceFetcher is the interface that abstracts Git operations used by the Reconciler.
type AppSourceFetcher interface {
	ResolveCommitSHA(ctx context.Context, repoURL, ref, token, sshKey string) (string, error)
	FetchDir(ctx context.Context, repoURL, ref, path, token, sshKey string) (map[string][]byte, error)
}

// RunAppSourceReconciler periodically syncs AppSource definitions from Git.
// Uses a default interval of 30 seconds when tick is 0.
func RunAppSourceReconciler(ctx context.Context, st store.Store, fetcher AppSourceFetcher, km secrets.KeyManager, tick time.Duration) {
	if tick == 0 {
		tick = 30 * time.Second
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcileAppSources(ctx, st, fetcher, km)
		}
	}
}

// reconcileAppSources checks all AppSources and syncs those that need it from Git.
// Uses an advisory lock to prevent multiple replicas from running concurrently.
func reconcileAppSources(ctx context.Context, st store.Store, fetcher AppSourceFetcher, km secrets.KeyManager) {
	release, err := st.AcquireAdvisoryLock(ctx, appSourceReconcilerLockKey)
	if err != nil {
		slog.Warn("appsource reconciler: failed to acquire advisory lock", "error", err)
		return
	}
	if release == nil {
		// Another replica holds the lock.
		return
	}
	defer release()

	sources, err := st.ListAppSources(ctx)
	if err != nil {
		slog.Warn("appsource reconciler: failed to list AppSources", "error", err)
		return
	}

	now := time.Now()
	for _, src := range sources {
		var spec dsl.AppSourceSpec
		if err := json.Unmarshal(src.Spec, &spec); err != nil {
			slog.Warn("appsource reconciler: failed to parse spec", "name", src.Name, "error", err)
			continue
		}
		if !shouldSync(src, spec, now) {
			continue
		}
		if err := syncAppSource(ctx, st, fetcher, km, src, spec); err != nil {
			slog.Warn("appsource reconciler: sync failed", "name", src.Name, "error", err)
		}
	}
}

// shouldSync determines whether an AppSource needs to be synced.
// Returns true when it has never been synced, last_commit is empty, or the sync interval has elapsed.
func shouldSync(src store.AppSource, spec dsl.AppSourceSpec, now time.Time) bool {
	if src.LastSyncedAt == nil {
		return true
	}
	if src.LastCommit == "" {
		return true
	}
	return now.Sub(*src.LastSyncedAt) >= spec.IntervalDuration()
}

// syncAppSource syncs a single AppSource from Git.
// Skips when the SHA is unchanged from last time. Deletes stale Jobs when prune is enabled.
func syncAppSource(ctx context.Context, st store.Store, fetcher AppSourceFetcher, km secrets.KeyManager, src store.AppSource, spec dsl.AppSourceSpec) error {
	cred, err := resolveCredential(ctx, st, km, spec.RepoURL, spec.GitCredentialRef)
	if err != nil {
		return fmt.Errorf("failed to resolve credential: %w", err)
	}

	headSHA, err := fetcher.ResolveCommitSHA(ctx, spec.RepoURL, spec.TargetRevision, cred.Token, cred.SSHKey)
	if err != nil {
		return fmt.Errorf("failed to resolve commit SHA: %w", err)
	}
	// Skip when the SHA has not changed (force sync when last_commit is empty).
	if headSHA == src.LastCommit && src.LastCommit != "" {
		return nil
	}

	files, err := fetcher.FetchDir(ctx, spec.RepoURL, spec.TargetRevision, spec.Path, cred.Token, cred.SSHKey)
	if err != nil {
		return fmt.Errorf("failed to fetch directory: %w", err)
	}

	currentJobNames := map[string]bool{}
	for filePath, content := range files {
		job, err := dsl.Parse(strings.NewReader(string(content)))
		if err != nil {
			slog.Warn("appsource reconciler: failed to parse YAML", "name", src.Name, "file", filePath, "error", err)
			continue
		}
		specJSON, err := json.Marshal(job.Spec)
		if err != nil {
			slog.Warn("appsource reconciler: failed to convert spec to JSON", "name", src.Name, "file", filePath, "error", err)
			continue
		}
		if _, err := st.UpsertJob(ctx, job.Metadata.Name, job.APIVersion, specJSON); err != nil {
			return fmt.Errorf("failed to upsert Job %q (%s): %w", job.Metadata.Name, filePath, err)
		}
		currentJobNames[job.Metadata.Name] = true
	}

	// Handle Jobs that were managed previously but are no longer present in the current file list.
	for _, prev := range src.ManagedJobs {
		if currentJobNames[prev] {
			continue
		}
		if spec.SyncPolicy.Prune {
			if err := st.DeleteJob(ctx, prev); err != nil {
				slog.Warn("appsource reconciler: failed to delete Job", "appsource", src.Name, "job", prev, "error", err)
			} else {
				slog.Info("appsource reconciler: deleted Job (prune)", "appsource", src.Name, "job", prev)
			}
		} else {
			slog.Warn("appsource reconciler: Job removed from Git is still present (set syncPolicy.prune: true to delete it)", "appsource", src.Name, "job", prev)
		}
	}

	managedJobs := make([]string, 0, len(currentJobNames))
	for name := range currentJobNames {
		managedJobs = append(managedJobs, name)
	}
	return st.UpdateAppSourceSyncState(ctx, src.Name, headSHA, time.Now(), managedJobs)
}

// resolveCredential resolves Git credentials based on the AppSource's repoURL and gitCredentialRef.
// Returns an empty Credential when st or km is nil (e.g. in test environments).
func resolveCredential(ctx context.Context, st store.Store, km secrets.KeyManager, repoURL, _ string) (gittemplate.Credential, error) {
	if st == nil || km == nil {
		return gittemplate.Credential{}, nil
	}
	u, err := url.Parse(repoURL)
	if err != nil {
		return gittemplate.Credential{}, nil
	}
	host := u.Hostname()
	if host == "" {
		return gittemplate.Credential{}, nil
	}

	gc, err := st.GetGitCredentialByHost(ctx, host)
	if err != nil || gc == nil {
		return gittemplate.Credential{}, nil
	}

	stored, err := st.GetSecret(ctx, gc.SecretRef, "global", "")
	if err != nil {
		return gittemplate.Credential{}, fmt.Errorf("failed to get secret %q: %w", gc.SecretRef, err)
	}
	plaintext, err := secrets.Decrypt(ctx, km, stored.EncryptedDEK, stored.Ciphertext)
	if err != nil {
		return gittemplate.Credential{}, fmt.Errorf("failed to decrypt secret for host %q: %w", host, err)
	}
	switch gc.CredType {
	case "token":
		return gittemplate.Credential{Token: string(plaintext)}, nil
	case "sshKey":
		return gittemplate.Credential{SSHKey: string(plaintext)}, nil
	default:
		return gittemplate.Credential{}, nil
	}
}
