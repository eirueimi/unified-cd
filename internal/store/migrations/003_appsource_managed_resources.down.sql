ALTER TABLE app_sources ADD COLUMN managed_jobs text[] NOT NULL DEFAULT '{}'::text[];
UPDATE app_sources SET managed_jobs = COALESCE(
  (SELECT array_agg(elem->>'name') FROM jsonb_array_elements(managed_resources) AS elem
   WHERE elem->>'kind' = 'Job'),
  '{}'::text[]);
ALTER TABLE app_sources DROP COLUMN managed_resources;
