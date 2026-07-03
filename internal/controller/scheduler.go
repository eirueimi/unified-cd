package controller

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/cache"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/gittemplate"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/secrets"
	"github.com/eirueimi/unified-cd/internal/store"
)

// RunScheduler transitions Pending runs to Queued.
// Only one replica acts as leader at a time via a Postgres advisory lock held on a dedicated connection.
// The lock is acquired once and kept for the goroutine's lifetime; it is released on ctx cancel or error.
func RunScheduler(ctx context.Context, st store.Store, tick time.Duration) {
	if tick == 0 {
		tick = 200 * time.Millisecond
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	var release func()          // non-nil when this instance is the leader
	var lastScheduleCheck time.Time // time of the last schedule check

	defer func() {
		if release != nil {
			release()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			// Try to acquire the lock if not yet the leader.
			if release == nil {
				var err error
				release, err = st.AcquireSchedulerLock(ctx)
				if err != nil {
					slog.Error("scheduler advisory lock error", "error", err)
					continue
				}
				if release == nil {
					continue // Another replica is leader.
				}
				slog.Info("scheduler became leader")
			}

			n, err := st.TransitionPendingToQueued(ctx, 50)
			if err != nil {
				slog.Error("scheduler transition error", "error", err)
				// Release leadership so another replica can take over.
				release()
				release = nil
				continue
			}
			if n > 0 {
				slog.Info("scheduler enqueued", "count", n)
			}

			// Run the schedule check once per minute.
			if t.Sub(lastScheduleCheck) >= time.Minute {
				checkAndFireSchedules(ctx, st, t)
				lastScheduleCheck = t
			}
		}
	}
}

// checkAndFireSchedules checks all schedules and creates Runs for those that are due.
// Called every minute while RunScheduler holds the leader lock.
//   - Fires a Run when next ∈ [now-1h, now].
//   - Advances last_fired_at without creating a Run when next < now-1h (missed while down).
//   - Does not update last_fired_at on CreateRun failure (allows retry on the next tick).
func checkAndFireSchedules(ctx context.Context, st store.Store, now time.Time) {
	schedules, err := st.ListSchedules(ctx)
	if err != nil {
		slog.Warn("checkAndFireSchedules: failed to list schedules", "error", err)
		return
	}
	windowStart := now.Add(-time.Hour)
	for _, sc := range schedules {
		base := windowStart
		if sc.LastFiredAt != nil {
			base = *sc.LastFiredAt
		}
		next, err := dsl.NextCronTime(sc.Cron, base)
		if err != nil {
			slog.Warn("checkAndFireSchedules: invalid cron expression", "schedule", sc.Name, "cron", sc.Cron, "error", err)
			continue
		}
		switch {
		case next.After(now):
			// Not yet due — do nothing.
		case !next.Before(windowStart):
			// next ∈ [windowStart, now] → fire
			params := sc.Params
			if job, jerr := st.GetJob(ctx, sc.JobName); jerr == nil {
				inputs := inputsFromSpecJSON(job.Spec)
				resolved, perr := resolveParams(inputs, sc.Params)
				if perr != nil {
					slog.Warn("checkAndFireSchedules: param validation failed", "schedule", sc.Name, "error", perr)
					continue // Do not update last_fired_at — allow retry on the next tick.
				}
				params = resolved
			} else {
				slog.Warn("checkAndFireSchedules: failed to load job for param validation", "schedule", sc.Name, "job", sc.JobName, "error", jerr)
			}
			_, err := st.CreateRun(ctx, sc.JobName, params, []byte(`{}`), nil, "schedule:"+sc.Name)
			if err != nil {
				slog.Warn("checkAndFireSchedules: failed to create Run", "schedule", sc.Name, "error", err)
				continue // Do not update last_fired_at — allow retry on the next tick.
			}
			if err := st.UpdateScheduleLastFiredAt(ctx, sc.Name, next); err != nil {
				slog.Warn("checkAndFireSchedules: failed to update last_fired_at", "schedule", sc.Name, "error", err)
			}
		default:
			// next < windowStart → too old to fire; advance last_fired_at
			if err := st.UpdateScheduleLastFiredAt(ctx, sc.Name, next); err != nil {
				slog.Warn("checkAndFireSchedules: failed to update last_fired_at", "schedule", sc.Name, "error", err)
			}
		}
	}
}

const cacheCleanupLockKey = int64(0x63616368) // 'cach'

// RunCacheCleanup deletes expired cache entries every 24 hours.
// Returns immediately if st or obj is nil.
// Even when multiple replicas are running, only one performs the cleanup due to the advisory lock.
func RunCacheCleanup(ctx context.Context, st store.Store, obj objectstore.ObjectStore) {
	if st == nil || obj == nil {
		return
	}
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		runCacheCleanupAsLeader(ctx, st, obj)
	}
}

func runCacheCleanupAsLeader(ctx context.Context, st store.Store, obj objectstore.ObjectStore) {
	release, err := st.AcquireAdvisoryLock(ctx, cacheCleanupLockKey)
	if err != nil {
		slog.Warn("cache cleanup lock", "error", err)
		return
	}
	if release == nil {
		return // Another replica is leader.
	}
	defer release()
	n, err := cache.DeleteExpired(ctx, obj, time.Now())
	if err != nil {
		slog.Warn("cache cleanup error", "error", err)
	} else if n > 0 {
		slog.Info("cache cleanup", "deleted", n)
	}
}

// RunGitResolver periodically resolves git:// URIs in Pending runs.
// For each Pending run with unresolved git:// uses.job URIs, it fetches the
// referenced YAML and inlines its steps directly into the run's spec.
// After UpdateRunSpec, the next RunScheduler tick will queue the run normally.
// Returns immediately if resolver is nil.
func RunGitResolver(ctx context.Context, st store.Store, resolver *gittemplate.Resolver, km secrets.KeyManager, tick time.Duration) {
	if resolver == nil {
		return
	}
	if tick == 0 {
		tick = 200 * time.Millisecond
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			resolveGitPendingRuns(ctx, st, resolver, km)
		}
	}
}

func resolveGitPendingRuns(ctx context.Context, st store.Store, resolver *gittemplate.Resolver, km secrets.KeyManager) {
	runs, err := st.ListPendingRuns(ctx, 50)
	if err != nil {
		slog.Warn("git resolver: list pending runs", "error", err)
		return
	}
	for _, r := range runs {
		if !gittemplate.HasGitURIs(r.Spec) {
			continue
		}
		credFn := func(ctx context.Context, host string) (gittemplate.Credential, error) {
			gc, err := st.GetGitCredentialByHost(ctx, host)
			if err != nil {
				return gittemplate.Credential{}, fmt.Errorf("get git credential: %w", err)
			}
			if gc == nil {
				return gittemplate.Credential{}, nil // public repo
			}
			stored, err := st.GetSecret(ctx, gc.SecretRef, "global", "")
			if err != nil {
				return gittemplate.Credential{}, fmt.Errorf("get secret %q for host %q: %w", gc.SecretRef, host, err)
			}
			plaintext, err := secrets.Decrypt(ctx, km, stored.EncryptedDEK, stored.Ciphertext)
			if err != nil {
				return gittemplate.Credential{}, fmt.Errorf("decrypt secret for host %q: %w", host, err)
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
		resolved, err := resolver.ResolveSpec(ctx, r.Spec, credFn)
		if err != nil {
			if gittemplate.IsResolveError(err) {
				slog.Error("git resolver: deterministic resolution failure, failing run", "runID", r.ID, "error", err)
				if ferr := st.MarkRunFinished(ctx, r.ID, api.RunFailed); ferr != nil {
					slog.Warn("git resolver: mark run failed failed", "runID", r.ID, "error", ferr)
				}
				continue
			}
			slog.Warn("git resolver: resolve spec failed", "runID", r.ID, "error", err)
			continue
		}
		if err := st.UpdateRunSpec(ctx, r.ID, resolved); err != nil {
			slog.Warn("git resolver: update spec failed", "runID", r.ID, "error", err)
		}
	}
}
