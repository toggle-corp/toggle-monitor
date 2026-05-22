-- Follow-up 11.B: remember the Slack channel slug a monitor was
-- configured to use, so when the monitor is removed from config we
-- can still resolve the channel + token at removal time.

ALTER TABLE monitors
    ADD COLUMN slack_channel_slug TEXT;
