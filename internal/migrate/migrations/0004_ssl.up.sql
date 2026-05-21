-- Issue 4: SSL incident class. Each monitor carries an independent
-- SSL state machine alongside its uptime state. Status is encoded in
-- ssl_status (rather than the main `status` column) so the two
-- incident classes can be active simultaneously.

ALTER TABLE monitors
    ADD COLUMN ssl_status            TEXT,                  -- 'ok' | 'ssl-expiring' | 'ssl-skipped'
    ADD COLUMN ssl_expires_at        TIMESTAMPTZ,
    ADD COLUMN ssl_issuer            TEXT,
    ADD COLUMN ssl_subject           TEXT,
    ADD COLUMN ssl_opened_at         TIMESTAMPTZ,           -- when the current ssl-expiring incident started
    ADD COLUMN ssl_last_reminder_at  TIMESTAMPTZ,
    ADD COLUMN ssl_thread_channel    TEXT,
    ADD COLUMN ssl_thread_ts         TEXT;
