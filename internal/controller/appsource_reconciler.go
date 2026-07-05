package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"time"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/gittemplate"
	"github.com/eirueimi/unified-cd/internal/secrets"
	"github.com/eirueimi/unified-cd/internal/store"
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
			if serr := st.SetAppSourceSyncStatus(ctx, src.Name, "Failed", err.Error()); serr != nil {
				slog.Warn("appsource reconciler: failed to record sync status", "name", src.Name, "error", serr)
			}
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
// Skips when the SHA is unchanged from last time. Prunes resources removed from Git when syncPolicy.prune is enabled.
func syncAppSource(ctx context.Context, st store.Store, fetcher AppSourceFetcher, km secrets.KeyManager, src store.AppSource, spec dsl.AppSourceSpec) error {
	cred, err := resolveCredential(ctx, st, km, spec.RepoURL)
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

	// Deterministic order: sort file paths so duplicate {kind,name} resolution is stable.
	paths := make([]string, 0, len(files))
	for fp := range files {
		paths = append(paths, fp)
	}
	sort.Strings(paths)

	current := make([]store.ResourceRef, 0, len(paths))
	seen := map[store.ResourceRef]bool{}
	for _, fp := range paths {
		kind := probeKind(files[fp])
		// Skip duplicates BEFORE writing to the store, so the first file (sorted)
		// wins the stored spec — not just the ManagedResources bookkeeping.
		if ref := (store.ResourceRef{Kind: kind, Name: probeName(files[fp])}); seen[ref] {
			slog.Warn("appsource reconciler: duplicate resource, keeping first", "name", src.Name, "kind", kind, "resource", ref.Name, "file", fp)
			continue
		}
		name, err := applyResource(ctx, st, kind, files[fp])
		if err != nil {
			// Store-write failures abort the whole sync; parse/unknown-kind skip one file.
			if errors.Is(err, errStoreWrite) {
				return fmt.Errorf("apply %s (%s): %w", kind, fp, err)
			}
			slog.Warn("appsource reconciler: skipping file", "name", src.Name, "file", fp, "kind", kind, "error", err)
			continue
		}
		ref := store.ResourceRef{Kind: kind, Name: name}
		seen[ref] = true
		current = append(current, ref)
	}

	// Prune resources managed previously but absent now.
	for _, prev := range src.ManagedResources {
		if seen[prev] {
			continue
		}
		if spec.SyncPolicy.Prune {
			if err := deleteResource(ctx, st, prev.Kind, prev.Name); err != nil {
				slog.Warn("appsource reconciler: failed to delete resource", "appsource", src.Name, "kind", prev.Kind, "resource", prev.Name, "error", err)
			} else {
				slog.Info("appsource reconciler: deleted resource (prune)", "appsource", src.Name, "kind", prev.Kind, "resource", prev.Name)
			}
		} else {
			slog.Warn("appsource reconciler: resource removed from Git is still present (set syncPolicy.prune: true to delete it)", "appsource", src.Name, "kind", prev.Kind, "resource", prev.Name)
		}
	}

	return st.UpdateAppSourceSyncState(ctx, src.Name, headSHA, time.Now(), current)
}

// resolveCredential resolves Git credentials by matching the AppSource's repoURL host
// against a stored GitCredential. Returns an empty Credential when st or km is nil
// (e.g. in test environments).
func resolveCredential(ctx context.Context, st store.Store, km secrets.KeyManager, repoURL string) (gittemplate.Credential, error) {
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
