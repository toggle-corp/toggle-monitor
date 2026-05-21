ALTER TABLE monitors
    DROP COLUMN IF EXISTS uptime_thread_ts,
    DROP COLUMN IF EXISTS uptime_thread_channel,
    DROP COLUMN IF EXISTS last_reminder_at;
