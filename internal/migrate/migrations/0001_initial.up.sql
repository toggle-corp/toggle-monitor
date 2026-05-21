-- Issue 2 baseline schema: monitors (current state) and alert_events
-- (event-sourced append-only). All timestamps are TIMESTAMPTZ so the
-- DB stores UTC and the UI applies displayTimezone at render time.

CREATE TABLE monitors (
    slug              TEXT        PRIMARY KEY,
    friendly_name     TEXT        NOT NULL,
    url               TEXT        NOT NULL,
    group_slug        TEXT        NOT NULL,
    source            TEXT        NOT NULL DEFAULT 'static',   -- 'static' | 'kube' (kube lands in Issue 9)
    status            TEXT        NOT NULL DEFAULT 'up',       -- 'up' | 'down' | (later: temporary-paused | kube-paused | kube-invalid | ssl-expiring | ssl-skipped)
    opened_at         TIMESTAMPTZ,                              -- NULL when status='up'
    last_checked_at   TIMESTAMPTZ,
    last_status_code  INTEGER,
    last_error        TEXT,
    archived          BOOLEAN     NOT NULL DEFAULT FALSE,
    archived_at       TIMESTAMPTZ,
    archive_reason    TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX monitors_status_idx ON monitors (status) WHERE archived = FALSE;
CREATE INDEX monitors_group_idx  ON monitors (group_slug) WHERE archived = FALSE;

CREATE TABLE alert_events (
    id               BIGSERIAL    PRIMARY KEY,
    monitor_slug     TEXT         NOT NULL,
    type             TEXT         NOT NULL,                    -- 'open' | 'resolve' (later: 'reminder', 'ssl_open', 'ssl_resolve', 'removed', ...)
    at               TIMESTAMPTZ  NOT NULL,
    status_code      INTEGER,
    error            TEXT,
    downtime_seconds BIGINT,                                   -- set only when type='resolve'
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- Per-monitor history (newest first) and homepage latest-alerts feed.
CREATE INDEX alert_events_monitor_at_idx ON alert_events (monitor_slug, at DESC);
CREATE INDEX alert_events_at_idx         ON alert_events (at DESC);
