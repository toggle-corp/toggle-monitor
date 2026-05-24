-- Restore the annotations column. The default of '{}'::jsonb preserves
-- the original NOT NULL contract; existing rows get an empty map.
ALTER TABLE discovery_snapshot ADD COLUMN annotations JSONB NOT NULL DEFAULT '{}'::jsonb;
