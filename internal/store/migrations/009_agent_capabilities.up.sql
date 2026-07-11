ALTER TABLE agents ADD COLUMN capabilities text[];          -- NULL = legacy agent (skip cap check)
ALTER TABLE runs   ADD COLUMN required_caps text[] DEFAULT '{}'::text[] NOT NULL;
