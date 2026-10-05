-- #245 E2E seed (test-issue-245-advert-intervals-e2e.js): two repeaters with
-- known advert gaps for the estimated advert intervals on node detail.
-- Applied by CI after the fixture is migrated (route_mask must exist):
--   sqlite3 test-fixtures/e2e-fixture.db < test-fixtures/seed-245-advert-intervals.sql
--
-- decoded_json.timestamp is the sender's clock. Out-of-band negative ids and
-- old observation timestamps keep these rows last in the packets views (as
-- in seed-2073); first_seen is relative to now.
--
-- Expected on /api/nodes/245e2e0...01?include=advertRoutes ("Advert Interval E2E"):
--   flood    : 10 adverts at 1, 13, 25, 49, 61, 66, 73, 109, 121, 133 h ago.
--              Gaps of 12 h (x5), 24 h (one missed), 36 h (two missed), and
--              5 h + 7 h around the manual advert at 66 h (dropped).
--              -> interval_s 43200, gaps_used 7, high.
--   zero_hop : 6 adverts every 120 min -> interval_s 7200, gaps_used 5, medium.
-- and /api/nodes/245e2e0...02 ("Flood Only Interval E2E"):
--   flood    : 8 adverts 24 h apart; the sender clock jumps back to
--              15 May 2024 after the 4th (that gap falls back to first_seen)
--              -> interval_s 86400, gaps_used 7, high.
--   zero_hop : none observed.
INSERT INTO nodes (public_key, name, role, last_seen, first_seen, advert_count) VALUES
  ('245e2e0000000000000000000000000000000000000000000000000000000001', 'Advert Interval E2E', 'repeater',
   strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-30 minutes'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-6 days'), 16),
  ('245e2e0000000000000000000000000000000000000000000000000000000002', 'Flood Only Interval E2E', 'repeater',
   strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-2 hours'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-8 days'), 8);

INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, payload_version, decoded_json, channel_hash, from_pubkey, route_mask) VALUES
  (-245001, '1100e2e2450001', 'e2e245advert0001', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-1 hours'), 1, 4, 0, '{"type":"ADVERT","name":"Advert Interval E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000001","timestamp":' || (CAST(strftime('%s', 'now', '-1 hours') AS INTEGER) - 2) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000001', 2),
  (-245002, '1100e2e2450002', 'e2e245advert0002', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-13 hours'), 1, 4, 0, '{"type":"ADVERT","name":"Advert Interval E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000001","timestamp":' || (CAST(strftime('%s', 'now', '-13 hours') AS INTEGER) - 2) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000001', 2),
  (-245003, '1100e2e2450003', 'e2e245advert0003', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-25 hours'), 1, 4, 0, '{"type":"ADVERT","name":"Advert Interval E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000001","timestamp":' || (CAST(strftime('%s', 'now', '-25 hours') AS INTEGER) - 2) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000001', 2),
  (-245004, '1100e2e2450004', 'e2e245advert0004', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-49 hours'), 1, 4, 0, '{"type":"ADVERT","name":"Advert Interval E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000001","timestamp":' || (CAST(strftime('%s', 'now', '-49 hours') AS INTEGER) - 2) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000001', 2),
  (-245005, '1100e2e2450005', 'e2e245advert0005', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-61 hours'), 1, 4, 0, '{"type":"ADVERT","name":"Advert Interval E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000001","timestamp":' || (CAST(strftime('%s', 'now', '-61 hours') AS INTEGER) - 2) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000001', 2),
  (-245006, '1100e2e2450006', 'e2e245advert0006', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-66 hours'), 1, 4, 0, '{"type":"ADVERT","name":"Advert Interval E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000001","timestamp":' || (CAST(strftime('%s', 'now', '-66 hours') AS INTEGER) - 2) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000001', 2),
  (-245007, '1100e2e2450007', 'e2e245advert0007', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-73 hours'), 1, 4, 0, '{"type":"ADVERT","name":"Advert Interval E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000001","timestamp":' || (CAST(strftime('%s', 'now', '-73 hours') AS INTEGER) - 2) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000001', 2),
  (-245008, '1100e2e2450008', 'e2e245advert0008', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-109 hours'), 1, 4, 0, '{"type":"ADVERT","name":"Advert Interval E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000001","timestamp":' || (CAST(strftime('%s', 'now', '-109 hours') AS INTEGER) - 2) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000001', 2),
  (-245009, '1100e2e2450009', 'e2e245advert0009', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-121 hours'), 1, 4, 0, '{"type":"ADVERT","name":"Advert Interval E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000001","timestamp":' || (CAST(strftime('%s', 'now', '-121 hours') AS INTEGER) - 2) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000001', 2),
  (-245010, '1100e2e2450010', 'e2e245advert0010', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-133 hours'), 1, 4, 0, '{"type":"ADVERT","name":"Advert Interval E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000001","timestamp":' || (CAST(strftime('%s', 'now', '-133 hours') AS INTEGER) - 2) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000001', 2),
  (-245011, '1200e2e2450011', 'e2e245advert0011', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-30 minutes'), 2, 4, 0, '{"type":"ADVERT","name":"Advert Interval E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000001","timestamp":' || (CAST(strftime('%s', 'now', '-30 minutes') AS INTEGER) - 1) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000001', 4),
  (-245012, '1200e2e2450012', 'e2e245advert0012', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-150 minutes'), 2, 4, 0, '{"type":"ADVERT","name":"Advert Interval E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000001","timestamp":' || (CAST(strftime('%s', 'now', '-150 minutes') AS INTEGER) - 1) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000001', 4),
  (-245013, '1200e2e2450013', 'e2e245advert0013', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-270 minutes'), 2, 4, 0, '{"type":"ADVERT","name":"Advert Interval E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000001","timestamp":' || (CAST(strftime('%s', 'now', '-270 minutes') AS INTEGER) - 1) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000001', 4),
  (-245014, '1200e2e2450014', 'e2e245advert0014', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-390 minutes'), 2, 4, 0, '{"type":"ADVERT","name":"Advert Interval E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000001","timestamp":' || (CAST(strftime('%s', 'now', '-390 minutes') AS INTEGER) - 1) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000001', 4),
  (-245015, '1200e2e2450015', 'e2e245advert0015', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-510 minutes'), 2, 4, 0, '{"type":"ADVERT","name":"Advert Interval E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000001","timestamp":' || (CAST(strftime('%s', 'now', '-510 minutes') AS INTEGER) - 1) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000001', 4),
  (-245016, '1200e2e2450016', 'e2e245advert0016', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-630 minutes'), 2, 4, 0, '{"type":"ADVERT","name":"Advert Interval E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000001","timestamp":' || (CAST(strftime('%s', 'now', '-630 minutes') AS INTEGER) - 1) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000001', 4),
  (-245017, '1100e2e2450017', 'e2e245advert0017', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-170 hours'), 1, 4, 0, '{"type":"ADVERT","name":"Flood Only Interval E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000002","timestamp":' || (CAST(strftime('%s', 'now', '-170 hours') AS INTEGER) - 2) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000002', 2),
  (-245018, '1100e2e2450018', 'e2e245advert0018', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-146 hours'), 1, 4, 0, '{"type":"ADVERT","name":"Flood Only Interval E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000002","timestamp":' || (CAST(strftime('%s', 'now', '-146 hours') AS INTEGER) - 2) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000002', 2),
  (-245019, '1100e2e2450019', 'e2e245advert0019', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-122 hours'), 1, 4, 0, '{"type":"ADVERT","name":"Flood Only Interval E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000002","timestamp":' || (CAST(strftime('%s', 'now', '-122 hours') AS INTEGER) - 2) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000002', 2),
  (-245020, '1100e2e2450020', 'e2e245advert0020', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-98 hours'), 1, 4, 0, '{"type":"ADVERT","name":"Flood Only Interval E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000002","timestamp":' || (CAST(strftime('%s', 'now', '-98 hours') AS INTEGER) - 2) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000002', 2),
  (-245021, '1100e2e2450021', 'e2e245advert0021', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-74 hours'), 1, 4, 0, '{"type":"ADVERT","name":"Flood Only Interval E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000002","timestamp":' || (1715770351 + 0) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000002', 2),
  (-245022, '1100e2e2450022', 'e2e245advert0022', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-50 hours'), 1, 4, 0, '{"type":"ADVERT","name":"Flood Only Interval E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000002","timestamp":' || (1715770351 + 86400) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000002', 2),
  (-245023, '1100e2e2450023', 'e2e245advert0023', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-26 hours'), 1, 4, 0, '{"type":"ADVERT","name":"Flood Only Interval E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000002","timestamp":' || (1715770351 + 172800) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000002', 2),
  (-245024, '1100e2e2450024', 'e2e245advert0024', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-2 hours'), 1, 4, 0, '{"type":"ADVERT","name":"Flood Only Interval E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000002","timestamp":' || (1715770351 + 259200) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000002', 2);

INSERT INTO observations (transmission_id, observer_idx, direction, snr, rssi, score, path_json, timestamp) VALUES
  (-245001, 1, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245002, 1, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245003, 1, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245004, 1, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245005, 1, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245006, 1, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245007, 1, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245008, 1, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245009, 1, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245010, 1, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245011, 1, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245012, 1, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245013, 1, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245014, 1, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245015, 1, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245016, 1, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245017, 1, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245018, 1, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245019, 1, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245020, 1, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245021, 1, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245022, 1, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245023, 1, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245024, 1, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER));
