-- 0007: Tags on monitors. Free-form labels used by the public /status
-- page's match rules; never displayed on operator-facing UIs yet, but
-- the column is generally-useful so we keep the type a Postgres TEXT[]
-- rather than a JSON blob. NULL/empty array both mean "no tags".

ALTER TABLE monitors ADD COLUMN tags TEXT[] NOT NULL DEFAULT '{}';
