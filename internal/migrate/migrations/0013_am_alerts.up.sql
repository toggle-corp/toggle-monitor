-- 0013: Alertmanager webhook receiver (ADR-0005). One row per
-- (fingerprint, incident); a fingerprint that fires-resolves-refires
-- gets a fresh row each time, while history is preserved. The partial
-- unique index on open rows is the idempotency anchor: it lets
-- InsertOpenAMIncident do an INSERT … ON CONFLICT DO NOTHING and
-- guarantees "at most one Slack parent per live fingerprint" at the
-- database level, even under concurrent webhook redeliveries.

CREATE TABLE am_alerts (
    id              BIGSERIAL   PRIMARY KEY,
    fingerprint     TEXT        NOT NULL,
    alertname       TEXT        NOT NULL,
    labels          JSONB       NOT NULL,
    annotations     JSONB       NOT NULL,
    started_at      TIMESTAMPTZ NOT NULL,
    ended_at        TIMESTAMPTZ,
    channel_slug    TEXT        NOT NULL,
    slack_channel   TEXT,
    slack_ts        TEXT,
    rule_chain      TEXT        NOT NULL,
    resolved_notify JSONB       NOT NULL,
    external_url    TEXT        NOT NULL DEFAULT '',
    receiver        TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- At most one OPEN incident per fingerprint — the partial uniqueness
-- the handler relies on for "DB-INSERT before Slack-post" idempotency.
CREATE UNIQUE INDEX am_alerts_open_by_fingerprint
    ON am_alerts (fingerprint) WHERE ended_at IS NULL;

-- Listing default sort: newest first.
CREATE INDEX am_alerts_started_at_desc
    ON am_alerts (started_at DESC);

-- am_alert_events: append-only log of every webhook delivery routed
-- through an incident. Powers the detail page's "raw payload" section
-- and leaves headroom for future flap detection. Cascade so a swept
-- incident takes its event trail with it.
CREATE TABLE am_alert_events (
    id          BIGSERIAL   PRIMARY KEY,
    incident_id BIGINT      NOT NULL REFERENCES am_alerts (id) ON DELETE CASCADE,
    event_type  TEXT        NOT NULL,   -- 'firing' | 'resolved' | 'repeat-firing' | 'late-resolve'
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    raw_payload JSONB       NOT NULL
);

CREATE INDEX am_alert_events_incident_id ON am_alert_events (incident_id);
CREATE INDEX am_alert_events_received_at ON am_alert_events (received_at);
