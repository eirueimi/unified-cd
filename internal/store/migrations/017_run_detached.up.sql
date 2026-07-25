ALTER TABLE runs ADD COLUMN detached BOOLEAN NOT NULL DEFAULT false;

-- Partial index so the claim query's detached filter stays cheap on the hot
-- Queued path (ClaimNextRun selects WHERE status = 'Queued' AND detached = $3).
CREATE INDEX idx_runs_queued_detached ON runs (detached) WHERE status = 'Queued';
