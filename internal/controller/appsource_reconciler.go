package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/gittemplate"
	"github.com/eirueimi/unified-cd/internal/secrets"
	"github.com/eirueimi/unified-cd/internal/store"
)

// appSourceReconcilerLockKey is the advisory lock key used by the AppSource Reconciler.
const appSourceReconcilerLockKey = int64(0x61707073)

// urlUserinfoRe matches the userinfo portion of a URL embedded in a larger string:
// scheme://[user[:pass]@]host. We only replace the credential (userinfo) part so the
// rest of the message (scheme, host, path, surrounding text) is preserved for
// debugging. The host is required to be non-"@" so we don't over-match on stray "@".
var urlUserinfoRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)([^/@\s]+)@`)

// redactURLCredentials strips userinfo (user and optional password) from any URL
// substrings in s, replacing it with "***" (or "***:***" when a password is present).
// This prevents credentials embedded in spec.repoURL (e.g.
// https://user:token@host/repo) from leaking into persisted last_error strings.
func redactURLCredentials(s string) string {
	return urlUserinfoRe.ReplaceAllStringFunc(s, func(match string) string {
		sub := urlUserinfoRe.FindStringSubmatch(match)
		if sub == nil {
			return match
		}
		scheme, userinfo := sub[1], sub[2]
		if strings.Contains(userinfo, ":") {
			return scheme + "***:***@"
		}
		return scheme + "***@"
	})
}

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
			// Redact any credentials embedded in a repoURL before the error string is
			// logged or persisted (bug #33), so tokens/passwords never leak.
			redacted := redactURLCredentials(err.Error())
			slog.Warn("appsource reconciler: sync failed", "name", src.Name, "error", redacted)
			if serr := st.SetAppSourceSyncStatus(ctx, src.Name, "Failed", redacted); serr != nil {
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
	skipped := 0
	for _, fp := range paths {
		kind := probeKind(files[fp])
		dir := relDir(spec.Path, fp)
		refName := probeName(files[fp])
		if kind == "Job" {
			refName = dsl.QualifyName(dir, refName)
		}
		// Skip duplicates BEFORE writing to the store, so the first file (sorted)
		// wins. Dedup on the qualified name so team-a/build and team-b/build are
		// distinct, not collapsed.
		if ref := (store.ResourceRef{Kind: kind, Name: refName}); seen[ref] {
			slog.Warn("appsource reconciler: duplicate resource, keeping first", "name", src.Name, "kind", kind, "resource", ref.Name, "file", fp)
			continue
		}
		name, err := applyResource(ctx, st, kind, dir, files[fp])
		if err != nil {
			// Store-write failures abort the whole sync; parse/unknown-kind skip one file.
			if errors.Is(err, errStoreWrite) {
				return fmt.Errorf("apply %s (%s): %w", kind, fp, err)
			}
			slog.Warn("appsource reconciler: failed to apply resource, skipping",
				"appsource", src.Name, "file", fp, "kind", kind, "resource", refName, "error", err)
			skipped++
			continue
		}
		ref := store.ResourceRef{Kind: kind, Name: name}
		seen[ref] = true
		current = append(current, ref)
	}
	if skipped > 0 {
		slog.Warn("appsource reconciler: some resources failed to apply and were skipped",
			"appsource", src.Name, "skipped", skipped, "applied", len(current))
	}

	// Prune resources managed previously but absent now.
	//
	// Legacy re-keying guard (bug #25): before commit 51ce318, a Job in a
	// subdirectory was stored under its BARE metadata.name and recorded in
	// managed_resources as {Job,"build"}. It is now keyed by the QUALIFIED name
	// {Job,"team-a/build"}. On the first sync after upgrade the prev entry is bare
	// but the seen entry is qualified, so an exact-set comparison would treat the
	// live job as removed and delete it (data loss). We recognize the re-key and
	// skip the delete, and rewrite managed_resources to the qualified form so
	// subsequent syncs are clean.
	//
	// Collision handling: we prefer an EXACT match (seen[prev]) first, so a bare
	// prev that still corresponds to a truly bare seen entry (flat layout) is
	// handled normally. Only when there is no exact match do we fall back to a
	// leaf match, and only for bare Job names. If two directories each contain a
	// "build" (seen = {team-a/build, team-b/build}), a single legacy bare
	// {Job,"build"} maps to whichever qualified entry shares the leaf — either is a
	// safe non-deletion because the live row was re-keyed into exactly one of them
	// by applyResource; we never delete, and we record the matched qualified name.
	legacyLeaf := map[string]string{} // bare Job leaf -> a qualified seen name sharing that leaf
	for ref := range seen {
		if ref.Kind != "Job" {
			continue
		}
		if _, leaf := dsl.SplitQualifiedName(ref.Name); leaf != ref.Name {
			// ref.Name is qualified (contains a "/"); index its leaf.
			if _, exists := legacyLeaf[leaf]; !exists {
				legacyLeaf[leaf] = ref.Name
			}
		}
	}

	for _, prev := range src.ManagedResources {
		if seen[prev] {
			continue
		}
		// Legacy-upgrade fallback: a bare Job name re-keyed to a qualified one.
		if prev.Kind == "Job" && !strings.Contains(prev.Name, "/") {
			if qualified, ok := legacyLeaf[prev.Name]; ok {
				// current already contains {Job, qualified} from applyResource, so
				// managed_resources will be rewritten to the qualified form. Complete
				// the in-place rename at the store level (bug #25 follow-up): applyResource
				// has ALREADY UpsertJob'd the qualified row before this prune loop runs, so
				// the bare row is now an orphan. RenameJob repoints run history from the
				// bare name to the qualified name and deletes the bare orphan, leaving
				// exactly one row under the qualified name with history intact. It never
				// deletes a live job: it only removes the bare row after run history has
				// been moved onto the (already-present) qualified row.
				if err := st.RenameJob(ctx, prev.Name, qualified); err != nil {
					// Non-fatal: the qualified row already exists and is managed, so the
					// job is not lost. Worst case the bare orphan lingers and we retry
					// next sync. Log and keep the historic no-delete guarantee.
					slog.Warn("appsource reconciler: legacy job re-key rename failed (bare orphan left for retry)",
						"appsource", src.Name, "old", prev.Name, "new", qualified, "error", err)
				} else {
					slog.Info("appsource reconciler: legacy job name re-keyed in place (orphan removed, history repointed)",
						"appsource", src.Name, "old", prev.Name, "new", qualified)
				}
				continue
			}
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
