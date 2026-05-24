-- Restore preset_slug as nullable TEXT, matching the original column
-- shape from migration 0005. Existing rows get NULL.
ALTER TABLE discovery_snapshot ADD COLUMN preset_slug TEXT;
