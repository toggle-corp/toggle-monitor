-- Issue 8: auto-discovery snapshot. One row per observed (ns, name, host)
-- triple, overwritten on each reconcile. annotations is the raw
-- annotation map at observe time, used by the auto-discovery detail
-- page (Issue 12).

CREATE TABLE discovery_snapshot (
    id            BIGSERIAL    PRIMARY KEY,
    namespace     TEXT         NOT NULL,
    ingress_name  TEXT         NOT NULL,
    host          TEXT         NOT NULL,
    status        TEXT         NOT NULL,                 -- 'added' | 'kube-paused' | 'kube-invalid'
    reason        TEXT,                                  -- sub-reason for kube-invalid (or pause/added context)
    preset_slug   TEXT,
    monitor_slug  TEXT,
    annotations   JSONB        NOT NULL DEFAULT '{}'::jsonb,
    last_seen_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (namespace, ingress_name, host)
);

CREATE INDEX discovery_snapshot_status_idx ON discovery_snapshot (status);
CREATE INDEX discovery_snapshot_ns_name_idx ON discovery_snapshot (namespace, ingress_name);
