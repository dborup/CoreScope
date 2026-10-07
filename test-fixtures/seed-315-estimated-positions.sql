-- Synthetic, private-network-independent fixture for issue #315. Apply only
-- to an empty, migrated test database (the E2E harness creates that database).
INSERT INTO nodes (public_key, name, role, lat, lon, first_seen, last_seen, advert_count) VALUES
 ('3151000000000000000000000000000000000000000000000000000000000000', 'Position Policy Reported GPS', 'repeater', 56.5, 12,
  datetime('now', '-1 day'), datetime('now'), 1),
 ('3152000000000000000000000000000000000000000000000000000000000000', 'Position Policy Missing GPS', 'repeater', NULL, NULL,
  datetime('now', '-1 day'), datetime('now'), 1),
 ('3153000000000000000000000000000000000000000000000000000000000000', 'Position Policy Anchor One', 'repeater', 55, 12,
  datetime('now', '-1 day'), datetime('now'), 1),
 ('3154000000000000000000000000000000000000000000000000000000000000', 'Position Policy Anchor Two', 'repeater', 55.02, 12.02,
  datetime('now', '-1 day'), datetime('now'), 1);

INSERT INTO neighbor_edges (node_a, node_b, count, last_seen) VALUES
 ('3151000000000000000000000000000000000000000000000000000000000000', '3153000000000000000000000000000000000000000000000000000000000000', 20, datetime('now')),
 ('3151000000000000000000000000000000000000000000000000000000000000', '3154000000000000000000000000000000000000000000000000000000000000', 10, datetime('now')),
 ('3152000000000000000000000000000000000000000000000000000000000000', '3153000000000000000000000000000000000000000000000000000000000000', 20, datetime('now')),
 ('3152000000000000000000000000000000000000000000000000000000000000', '3154000000000000000000000000000000000000000000000000000000000000', 10, datetime('now'));

INSERT INTO observers (rowid, id, name, first_seen, last_seen, packet_count) VALUES
 (1, '3153000000000000000000000000000000000000000000000000000000000000', 'Position Policy Anchor One', datetime('now', '-1 day'), datetime('now'), 1);

INSERT INTO transmissions (id, raw_hex, hash, first_seen, last_seen, route_type, payload_type, payload_version, decoded_json, from_pubkey, route_mask) VALUES
 (315001, '1100315001', 'e2e3150000000001', strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), CAST(strftime('%s', 'now') AS INTEGER), 1, 4, 0,
  '{"type":"ADVERT","name":"Position Policy Reported GPS","pubKey":"3151000000000000000000000000000000000000000000000000000000000000"}',
  '3151000000000000000000000000000000000000000000000000000000000000', 2);

INSERT INTO observations (transmission_id, observer_idx, direction, snr, rssi, score, path_json, resolved_path, timestamp) VALUES
 (315001, 1, 'rx', 10, -80, 0, '["3151","3152","3155"]',
  '["3151000000000000000000000000000000000000000000000000000000000000","3152000000000000000000000000000000000000000000000000000000000000","3155000000000000000000000000000000000000000000000000000000000000"]',
  CAST(strftime('%s', 'now') AS INTEGER));
