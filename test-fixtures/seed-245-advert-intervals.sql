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

-- #245 M2 (test-issue-245-advert-interval-analytics-e2e.js): three more
-- repeaters for Analytics -> Advert Intervals, without positions so no area
-- or map test sees them. Region = the observers' IATA (observer rowids of the
-- migrated fixture: 2 and 5 SJC, 3 OAK; the M1 nodes above are heard by 1, OAK).
-- Expected on /api/analytics/advert-intervals:
--   "Zero-hop Default E2E" (...03, SJC): zero-hop every 2 min x8 -> 120 s,
--       high, the new-install default -> outlier zero_hop_new_install_default;
--       flood every 47 h x4 -> 169200, medium.
--   "Fast Flood E2E" (...04, OAK): flood every 10 min x6 -> irregular (no
--       timer runs that fast), median gap 600 -> outlier flood_faster_than_settable.
--   "Short Flood E2E" (...05, SJC): flood every 3 h x8 -> 10800, high; no
--       zero-hop -> counted in flood_re_arm.zero_hop_maybe_suppressed.
INSERT INTO nodes (public_key, name, role, last_seen, first_seen, advert_count) VALUES
  ('245e2e0000000000000000000000000000000000000000000000000000000003', 'Zero-hop Default E2E', 'repeater', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-10 minutes'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-8 days'), 8),
  ('245e2e0000000000000000000000000000000000000000000000000000000004', 'Fast Flood E2E', 'repeater', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-10 minutes'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-8 days'), 8),
  ('245e2e0000000000000000000000000000000000000000000000000000000005', 'Short Flood E2E', 'repeater', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-10 minutes'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-8 days'), 8);

INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, payload_version, decoded_json, channel_hash, from_pubkey, route_mask) VALUES
  (-245101, '1200e2e20101', 'e2e245m2adv0001', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-10 minutes'), 2, 4, 0, '{"type":"ADVERT","name":"Zero-hop Default E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000003","timestamp":' || (CAST(strftime('%s', 'now', '-10 minutes') AS INTEGER) - 1) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000003', 4),
  (-245102, '1200e2e20102', 'e2e245m2adv0002', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-12 minutes'), 2, 4, 0, '{"type":"ADVERT","name":"Zero-hop Default E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000003","timestamp":' || (CAST(strftime('%s', 'now', '-12 minutes') AS INTEGER) - 1) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000003', 4),
  (-245103, '1200e2e20103', 'e2e245m2adv0003', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-14 minutes'), 2, 4, 0, '{"type":"ADVERT","name":"Zero-hop Default E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000003","timestamp":' || (CAST(strftime('%s', 'now', '-14 minutes') AS INTEGER) - 1) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000003', 4),
  (-245104, '1200e2e20104', 'e2e245m2adv0004', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-16 minutes'), 2, 4, 0, '{"type":"ADVERT","name":"Zero-hop Default E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000003","timestamp":' || (CAST(strftime('%s', 'now', '-16 minutes') AS INTEGER) - 1) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000003', 4),
  (-245105, '1200e2e20105', 'e2e245m2adv0005', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-18 minutes'), 2, 4, 0, '{"type":"ADVERT","name":"Zero-hop Default E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000003","timestamp":' || (CAST(strftime('%s', 'now', '-18 minutes') AS INTEGER) - 1) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000003', 4),
  (-245106, '1200e2e20106', 'e2e245m2adv0006', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-20 minutes'), 2, 4, 0, '{"type":"ADVERT","name":"Zero-hop Default E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000003","timestamp":' || (CAST(strftime('%s', 'now', '-20 minutes') AS INTEGER) - 1) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000003', 4),
  (-245107, '1200e2e20107', 'e2e245m2adv0007', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-22 minutes'), 2, 4, 0, '{"type":"ADVERT","name":"Zero-hop Default E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000003","timestamp":' || (CAST(strftime('%s', 'now', '-22 minutes') AS INTEGER) - 1) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000003', 4),
  (-245108, '1200e2e20108', 'e2e245m2adv0008', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-24 minutes'), 2, 4, 0, '{"type":"ADVERT","name":"Zero-hop Default E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000003","timestamp":' || (CAST(strftime('%s', 'now', '-24 minutes') AS INTEGER) - 1) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000003', 4),
  (-245109, '1100e2e20109', 'e2e245m2adv0009', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-1 hours'), 1, 4, 0, '{"type":"ADVERT","name":"Zero-hop Default E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000003","timestamp":' || (CAST(strftime('%s', 'now', '-1 hours') AS INTEGER) - 1) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000003', 2),
  (-245110, '1100e2e2010a', 'e2e245m2adv0010', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-48 hours'), 1, 4, 0, '{"type":"ADVERT","name":"Zero-hop Default E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000003","timestamp":' || (CAST(strftime('%s', 'now', '-48 hours') AS INTEGER) - 1) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000003', 2),
  (-245111, '1100e2e2010b', 'e2e245m2adv0011', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-95 hours'), 1, 4, 0, '{"type":"ADVERT","name":"Zero-hop Default E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000003","timestamp":' || (CAST(strftime('%s', 'now', '-95 hours') AS INTEGER) - 1) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000003', 2),
  (-245112, '1100e2e2010c', 'e2e245m2adv0012', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-142 hours'), 1, 4, 0, '{"type":"ADVERT","name":"Zero-hop Default E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000003","timestamp":' || (CAST(strftime('%s', 'now', '-142 hours') AS INTEGER) - 1) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000003', 2),
  (-245113, '1100e2e2010d', 'e2e245m2adv0013', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-5 minutes'), 1, 4, 0, '{"type":"ADVERT","name":"Fast Flood E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000004","timestamp":' || (CAST(strftime('%s', 'now', '-5 minutes') AS INTEGER) - 1) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000004', 2),
  (-245114, '1100e2e2010e', 'e2e245m2adv0014', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-15 minutes'), 1, 4, 0, '{"type":"ADVERT","name":"Fast Flood E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000004","timestamp":' || (CAST(strftime('%s', 'now', '-15 minutes') AS INTEGER) - 1) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000004', 2),
  (-245115, '1100e2e2010f', 'e2e245m2adv0015', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-25 minutes'), 1, 4, 0, '{"type":"ADVERT","name":"Fast Flood E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000004","timestamp":' || (CAST(strftime('%s', 'now', '-25 minutes') AS INTEGER) - 1) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000004', 2),
  (-245116, '1100e2e20110', 'e2e245m2adv0016', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-35 minutes'), 1, 4, 0, '{"type":"ADVERT","name":"Fast Flood E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000004","timestamp":' || (CAST(strftime('%s', 'now', '-35 minutes') AS INTEGER) - 1) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000004', 2),
  (-245117, '1100e2e20111', 'e2e245m2adv0017', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-45 minutes'), 1, 4, 0, '{"type":"ADVERT","name":"Fast Flood E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000004","timestamp":' || (CAST(strftime('%s', 'now', '-45 minutes') AS INTEGER) - 1) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000004', 2),
  (-245118, '1100e2e20112', 'e2e245m2adv0018', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-55 minutes'), 1, 4, 0, '{"type":"ADVERT","name":"Fast Flood E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000004","timestamp":' || (CAST(strftime('%s', 'now', '-55 minutes') AS INTEGER) - 1) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000004', 2),
  (-245119, '1100e2e20113', 'e2e245m2adv0019', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-2 hours'), 1, 4, 0, '{"type":"ADVERT","name":"Short Flood E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000005","timestamp":' || (CAST(strftime('%s', 'now', '-2 hours') AS INTEGER) - 1) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000005', 2),
  (-245120, '1100e2e20114', 'e2e245m2adv0020', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-5 hours'), 1, 4, 0, '{"type":"ADVERT","name":"Short Flood E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000005","timestamp":' || (CAST(strftime('%s', 'now', '-5 hours') AS INTEGER) - 1) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000005', 2),
  (-245121, '1100e2e20115', 'e2e245m2adv0021', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-8 hours'), 1, 4, 0, '{"type":"ADVERT","name":"Short Flood E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000005","timestamp":' || (CAST(strftime('%s', 'now', '-8 hours') AS INTEGER) - 1) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000005', 2),
  (-245122, '1100e2e20116', 'e2e245m2adv0022', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-11 hours'), 1, 4, 0, '{"type":"ADVERT","name":"Short Flood E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000005","timestamp":' || (CAST(strftime('%s', 'now', '-11 hours') AS INTEGER) - 1) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000005', 2),
  (-245123, '1100e2e20117', 'e2e245m2adv0023', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-14 hours'), 1, 4, 0, '{"type":"ADVERT","name":"Short Flood E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000005","timestamp":' || (CAST(strftime('%s', 'now', '-14 hours') AS INTEGER) - 1) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000005', 2),
  (-245124, '1100e2e20118', 'e2e245m2adv0024', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-17 hours'), 1, 4, 0, '{"type":"ADVERT","name":"Short Flood E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000005","timestamp":' || (CAST(strftime('%s', 'now', '-17 hours') AS INTEGER) - 1) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000005', 2),
  (-245125, '1100e2e20119', 'e2e245m2adv0025', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-20 hours'), 1, 4, 0, '{"type":"ADVERT","name":"Short Flood E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000005","timestamp":' || (CAST(strftime('%s', 'now', '-20 hours') AS INTEGER) - 1) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000005', 2),
  (-245126, '1100e2e2011a', 'e2e245m2adv0026', strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-23 hours'), 1, 4, 0, '{"type":"ADVERT","name":"Short Flood E2E","pubKey":"245e2e0000000000000000000000000000000000000000000000000000000005","timestamp":' || (CAST(strftime('%s', 'now', '-23 hours') AS INTEGER) - 1) || '}', NULL, '245e2e0000000000000000000000000000000000000000000000000000000005', 2);

INSERT INTO observations (transmission_id, observer_idx, direction, snr, rssi, score, path_json, timestamp) VALUES
  (-245101, 2, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245102, 2, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245103, 2, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245104, 2, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245105, 2, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245106, 2, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245107, 2, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245108, 2, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245109, 2, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245110, 2, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245111, 2, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245112, 2, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245113, 3, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245114, 3, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245115, 3, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245116, 3, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245117, 3, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245118, 3, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245119, 5, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245120, 5, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245121, 5, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245122, 5, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245123, 5, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245124, 5, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245125, 5, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER)),
  (-245126, 5, 'rx', 5.0, -95, 0, '[]', CAST(strftime('%s', '2026-05-15T00:00:00Z') AS INTEGER));
