ALTER TABLE app_sources ADD COLUMN managed_resources jsonb NOT NULL DEFAULT '[]'::jsonb;
-- Backfill verbatim: managed_jobs holds only the stored job key. For jobs applied
-- before commit 51ce318 that key is the BARE metadata.name (e.g. "build"), with no
-- record of the source subdirectory, so the qualified name ("team-a/build") cannot be
-- reconstructed here. We intentionally preserve the bare name and let the AppSource
-- reconciler recognize the re-keyed job on its next sync (see bug #25: the prune loop's
-- legacy leaf-match guard in appsource_reconciler.go) instead of deleting the live job.
UPDATE app_sources SET managed_resources = COALESCE(
  (SELECT jsonb_agg(jsonb_build_object('kind', 'Job', 'name', j)) FROM unnest(managed_jobs) AS j),
  '[]'::jsonb);
ALTER TABLE app_sources DROP COLUMN managed_jobs;
