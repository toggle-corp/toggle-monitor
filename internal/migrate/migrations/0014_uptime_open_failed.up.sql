-- 0014: uptime_open_failed_at records that a monitor's uptime Open was
-- sent to Slack during the current incident and the post failed. A
-- recovery with no uptime_thread_ts reads it to tell "the page was due
-- and never landed" (post a late-notice recovery) from "nobody was ever
-- paged" (a dependsOn child paused before its Open flushed: post
-- nothing). Cleared when a thread ref is stored and on resolve.

ALTER TABLE monitors ADD COLUMN uptime_open_failed_at TIMESTAMPTZ;
