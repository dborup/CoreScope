-- #2073 E2E seed (test-issue-2073-recent-adverts-e2e.js): one repeater whose
-- adverts cover every route class of the node-detail Recent Adverts panel.
-- Applied by CI after the fixture is migrated (route_mask must exist):
--   sqlite3 test-fixtures/e2e-fixture.db < test-fixtures/seed-2073-route-adverts.sql
--
-- route_mask bit r = raw route r seen for the hash (#89): 1 = transport
-- flood, 2 = flood, 4 = direct (zero-hop), 8 = transport direct.
-- Out-of-band negative ids and old observation timestamps keep these rows
-- last in the packets views (same approach as the #1486 / #1791 seeds);
-- first_seen is relative to now so the 24h / 7d windows are stable.
--
-- Expected on /api/nodes/2073e2e0...01:
--   Flood    : e2e2073flood0001 (route 1), e2e2073flood0002 (route 0),
--              e2e2073legacy001 (NULL mask, route_type 1 fallback)
--   Zero-hop : e2e2073zerohop01, e2e2073zerohop02
--   Mixed    : e2e2073mixed0001 (first stored as zero-hop),
--              e2e2073mixed0002 (first stored as flood)
--   24h: flood 2, zero_hop 1, mixed 1    7d: flood 3, zero_hop 2, mixed 2
-- and /api/nodes/2073e2e0...02 ("Zero Hop Only E2E"): one zero-hop advert,
-- so its Flood and Mixed tabs show the empty state.
INSERT INTO nodes (public_key, name, role, last_seen, first_seen, advert_count) VALUES
  ('2073e2e000000000000000000000000000000000000000000000000000000001', 'Route Mix E2E', 'repeater',
   strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-1 hours'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-3 days'), 7),
  ('2073e2e000000000000000000000000000000000000000000000000000000002', 'Zero Hop Only E2E', 'repeater',
   strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-2 hours'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-3 days'), 1);

INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, payload_version, decoded_json, channel_hash, from_pubkey, route_mask) VALUES
  (-2073001, '1200e2e207300001', 'e2e2073zerohop01', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-1 hours'),  2, 4, 0, '{"type":"ADVERT","name":"Route Mix E2E","pubKey":"2073e2e000000000000000000000000000000000000000000000000000000001"}', NULL, '2073e2e000000000000000000000000000000000000000000000000000000001', 4),
  (-2073002, '1100e2e207300002', 'e2e2073flood0001', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-2 hours'),  1, 4, 0, '{"type":"ADVERT","name":"Route Mix E2E","pubKey":"2073e2e000000000000000000000000000000000000000000000000000000001"}', NULL, '2073e2e000000000000000000000000000000000000000000000000000000001', 2),
  (-2073003, '1000000000e2e207300003', 'e2e2073flood0002', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-3 hours'), 0, 4, 0, '{"type":"ADVERT","name":"Route Mix E2E","pubKey":"2073e2e000000000000000000000000000000000000000000000000000000001"}', NULL, '2073e2e000000000000000000000000000000000000000000000000000000001', 1),
  (-2073004, '1200e2e207300004', 'e2e2073mixed0001', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-4 hours'),  2, 4, 0, '{"type":"ADVERT","name":"Route Mix E2E","pubKey":"2073e2e000000000000000000000000000000000000000000000000000000001"}', NULL, '2073e2e000000000000000000000000000000000000000000000000000000001', 6),
  (-2073005, '1200e2e207300005', 'e2e2073zerohop02', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-30 hours'), 2, 4, 0, '{"type":"ADVERT","name":"Route Mix E2E","pubKey":"2073e2e000000000000000000000000000000000000000000000000000000001"}', NULL, '2073e2e000000000000000000000000000000000000000000000000000000001', 4),
  (-2073006, '1100e2e207300006', 'e2e2073legacy001', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-50 hours'), 1, 4, 0, '{"type":"ADVERT","name":"Route Mix E2E","pubKey":"2073e2e000000000000000000000000000000000000000000000000000000001"}', NULL, '2073e2e000000000000000000000000000000000000000000000000000000001', NULL),
  (-2073007, '1100e2e207300007', 'e2e2073mixed0002', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-60 hours'), 1, 4, 0, '{"type":"ADVERT","name":"Route Mix E2E","pubKey":"2073e2e000000000000000000000000000000000000000000000000000000001"}', NULL, '2073e2e000000000000000000000000000000000000000000000000000000001', 6),
  (-2073008, '1200e2e207300008', 'e2e2073zhonly001', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-2 hours'),  2, 4, 0, '{"type":"ADVERT","name":"Zero Hop Only E2E","pubKey":"2073e2e000000000000000000000000000000000000000000000000000000002"}', NULL, '2073e2e000000000000000000000000000000000000000000000000000000002', 4);

INSERT INTO observations (transmission_id, observer_idx, direction, snr, rssi, score, path_json, timestamp) VALUES
  (-2073001, 1, 'rx', 6.5, -92, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-2073002, 1, 'rx', 5.0, -97, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-2073003, 1, 'rx', 4.0, -99, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-2073004, 1, 'rx', 3.5, -101, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-2073005, 1, 'rx', 6.0, -93, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-2073006, 1, 'rx', 2.0, -104, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-2073007, 1, 'rx', 1.5, -106, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-2073008, 1, 'rx', 5.5, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER));
