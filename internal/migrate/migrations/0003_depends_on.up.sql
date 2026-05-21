-- Issue 7: dependsOn dependency graph. Stored as a TEXT[] column so
-- the UI can render gating parents without re-parsing the YAML. The
-- list comes from monitors[].dependsOn at config load and is rewritten
-- on every ReconcileMonitor.

ALTER TABLE monitors
    ADD COLUMN depends_on TEXT[] NOT NULL DEFAULT '{}'::TEXT[];
