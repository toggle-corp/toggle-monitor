-- ADR-0002: ingress annotations are no longer consulted (the kube.match
-- tree owns every monitor field). Drop the discovery_snapshot.annotations
-- column so the row shape matches the new model. A future operator who
-- wants the raw labels/annotations of an Ingress should run `kubectl
-- describe`; persisting them here was load-bearing only for the now-
-- removed `/kube.*` and `/config.*` annotation layer.
ALTER TABLE discovery_snapshot DROP COLUMN annotations;
