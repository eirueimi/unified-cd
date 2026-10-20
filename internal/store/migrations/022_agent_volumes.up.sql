-- Agent-owned named volumes (see
-- docs/superpowers/specs/2026-10-20-agent-named-volumes-design.md).
--
-- agents.volumes is the set of volume names an agent advertises at
-- registration. Unlike agents.capabilities it is NOT NULL: an agent that
-- declares no volumes is simply an agent with an empty set, and there is no
-- legacy-agent case to preserve.
--
-- runs.required_volumes holds one array of candidate volume names per volume
-- the job mounts, e.g. [["lib-a"],["cache-1","cache-2"]]. A run needs one free
-- volume from every inner array. It is written once, at queue time, after the
-- candidate names have been expanded against the run's params.
ALTER TABLE agents ADD COLUMN volumes text[] NOT NULL DEFAULT '{}'::text[];
ALTER TABLE runs   ADD COLUMN required_volumes jsonb NOT NULL DEFAULT '[]'::jsonb;
