-- Issue 3 additions to the monitors table:
--   * last_reminder_at: timestamp of the most recent uptime-reminder
--     Slack thread reply emitted for the current incident. Set to
--     opened_at when the incident opens, cleared on resolve.
--   * uptime_thread_channel / uptime_thread_ts: Slack thread ref for
--     the parent down message. Used to post reminders and edit the
--     parent on resolve. Cleared on resolve.

ALTER TABLE monitors
    ADD COLUMN last_reminder_at      TIMESTAMPTZ,
    ADD COLUMN uptime_thread_channel TEXT,
    ADD COLUMN uptime_thread_ts      TEXT;
