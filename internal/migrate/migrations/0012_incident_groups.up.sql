-- 0012: alert coalescing. A living per-channel incident ("group")
-- collapses an outage storm into ONE digest message instead of one
-- Slack message per monitor. incident_groups holds the digest message
-- ref plus the group_wait / group_interval / repeat_interval timers;
-- incident_group_members tracks each monitor's membership so a process
-- restart can reload an open group and reattach to the existing Slack
-- message rather than re-storming. See the alert-coalescing design.

CREATE TABLE incident_groups (
    id               BIGSERIAL   PRIMARY KEY,
    channel_slug     TEXT        NOT NULL,
    opened_at        TIMESTAMPTZ NOT NULL,
    digest_channel   TEXT        NOT NULL DEFAULT '',
    digest_ts        TEXT        NOT NULL DEFAULT '',
    posted           BOOLEAN     NOT NULL DEFAULT FALSE,
    closed           BOOLEAN     NOT NULL DEFAULT FALSE,
    last_flush_at    TIMESTAMPTZ,
    last_reminder_at TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- At most one *open* group per channel: the reattach/lookup invariant.
CREATE UNIQUE INDEX incident_groups_open_channel_idx
    ON incident_groups (channel_slug) WHERE closed = FALSE;

CREATE TABLE incident_group_members (
    group_id     BIGINT      NOT NULL REFERENCES incident_groups (id) ON DELETE CASCADE,
    monitor_slug TEXT        NOT NULL,
    -- down | recovering | recovered | paused (mirrors group.MemberState)
    state        TEXT        NOT NULL,
    joined_at    TIMESTAMPTZ NOT NULL,
    down_since   TIMESTAMPTZ,
    up_since     TIMESTAMPTZ,
    changed_at   TIMESTAMPTZ NOT NULL,
    -- last render-class shown in the digest, for delta computation
    rendered     TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (group_id, monitor_slug)
);
