-- ADR-0003: drop the per-monitor group_slug column. Group is no
-- longer a first-class concept; status pages are the only collection
-- entity and they derive membership from tags + host predicates.
DROP INDEX IF EXISTS monitors_group_idx;
ALTER TABLE monitors DROP COLUMN IF EXISTS group_slug;
