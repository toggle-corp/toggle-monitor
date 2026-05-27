-- 0011: SMTP monitor support. `kind` discriminates the probe protocol
-- ('http' default | 'smtp'); host/port/tls_mode carry the SMTP probe
-- target. HTTP monitors leave host/port/tls_mode NULL. `url` stays
-- NOT NULL for every kind — SMTP monitors store a synthesized
-- smtp://host:port so URL-keyed features (rendering, /status hostRegex)
-- keep working. See the SMTP monitoring design.

ALTER TABLE monitors ADD COLUMN kind     TEXT    NOT NULL DEFAULT 'http';
ALTER TABLE monitors ADD COLUMN host     TEXT;
ALTER TABLE monitors ADD COLUMN port     INTEGER;
ALTER TABLE monitors ADD COLUMN tls_mode TEXT;

CREATE INDEX monitors_kind_idx ON monitors (kind) WHERE archived = FALSE;
