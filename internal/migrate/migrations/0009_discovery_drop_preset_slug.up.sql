-- ADR-0002: presets were deleted entirely (the kube.match tree owns
-- every monitor field). The discovery_snapshot.preset_slug column was
-- carried for a release as nullable backwards-compat baggage; drop it
-- now that nothing reads or writes it. Operators wanting to see why a
-- snapshot row materialized should consult discovery_snapshot.reason
-- (which carries the rule-chain summary).
ALTER TABLE discovery_snapshot DROP COLUMN preset_slug;
