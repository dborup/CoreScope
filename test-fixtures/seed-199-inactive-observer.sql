-- #199 E2E seed (test-issue-199-inactive-observer-e2e.js): an observer whose
-- node row was retired to inactive_nodes (retention.nodeDays without an
-- advert), the shape that dead-ended on "Node not found".
-- Applied by CI after the fixture is migrated (inactive_nodes must exist):
--   sqlite3 test-fixtures/e2e-fixture.db < test-fixtures/seed-199-inactive-observer.sql
--
-- Reuses fixture observer 0D3B3F38... (no nodes row, no observations), so no
-- observer list or count changes for the other E2E tests. Its node row lives
-- only in inactive_nodes. Fixture observer 424419FD... stays as is: an
-- observer with no node record at all.
INSERT OR REPLACE INTO inactive_nodes (public_key, name, role, last_seen, first_seen, advert_count) VALUES
  ('0d3b3f382173a49eb0e3bb01ab2ea9b28601d9039e4bd52c55b91a8000cc092d', 'Inactive Observer E2E', 'repeater',
   strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-10 days'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-30 days'), 3);
