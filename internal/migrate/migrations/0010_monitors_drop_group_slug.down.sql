-- Down: re-create the group_slug column (without backfill — data is
-- lost on roll-forward). Restores the column to its 0001 shape so a
-- subsequent up-migration can repopulate it from config.
ALTER TABLE monitors ADD COLUMN group_slug TEXT NOT NULL DEFAULT '';
ALTER TABLE monitors ALTER COLUMN group_slug DROP DEFAULT;
CREATE INDEX monitors_group_idx ON monitors (group_slug) WHERE archived = FALSE;
