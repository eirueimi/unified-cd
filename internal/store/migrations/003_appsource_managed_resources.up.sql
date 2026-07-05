ALTER TABLE app_sources ADD COLUMN managed_resources jsonb NOT NULL DEFAULT '[]'::jsonb;
UPDATE app_sources SET managed_resources = COALESCE(
  (SELECT jsonb_agg(jsonb_build_object('kind', 'Job', 'name', j)) FROM unnest(managed_jobs) AS j),
  '[]'::jsonb);
ALTER TABLE app_sources DROP COLUMN managed_jobs;
