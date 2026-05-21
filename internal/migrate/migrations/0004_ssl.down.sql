ALTER TABLE monitors
    DROP COLUMN IF EXISTS ssl_thread_ts,
    DROP COLUMN IF EXISTS ssl_thread_channel,
    DROP COLUMN IF EXISTS ssl_last_reminder_at,
    DROP COLUMN IF EXISTS ssl_opened_at,
    DROP COLUMN IF EXISTS ssl_subject,
    DROP COLUMN IF EXISTS ssl_issuer,
    DROP COLUMN IF EXISTS ssl_expires_at,
    DROP COLUMN IF EXISTS ssl_status;
