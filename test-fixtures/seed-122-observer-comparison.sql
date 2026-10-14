-- #122 fixture: deterministic reference-to-many overlap data.
--
-- The committed e2e-fixture.db has observations.direction = NULL for every
-- row (it predates the firmware/MQTT "direction" field) -- correctly, the
-- #122 endpoint treats an unknown direction as "not RX" rather than
-- guessing, so without this seed every comparison in the fixture would be
-- all-zero/"no data" and the reference-to-many E2E couldn't exercise real
-- overlap, sorting-by-value, or the headline counts.
--
-- Uses the fixture's first three real, active observers (rowid 1 "Kennedy
-- Repeater", rowid 2 "GY889 Repeater", rowid 3 "DUBLIN Observer") so the
-- E2E can pick the reference by its known name rather than a brittle index.
-- Synthetic transmission ids are a deliberately out-of-band sentinel range
-- (-1220001..-1220012, mirroring the -1000000 #1791 convention in
-- deploy.yml) so they cannot collide with real ingested rows. Run AFTER
-- the schema migration (./corescope-migrate), like the other seed-*.sql
-- files, and AFTER tools/freshen-fixture.sh (timestamps use strftime('now')
-- directly, so they land inside every #122 window -- 1h/6h/24h -- no
-- matter when CI runs).
--
-- Overlap design (rowid 1 = reference "Kennedy Repeater"):
--   Kennedy sees tx 1..10                                  (|R| = 10)
--   GY889   sees tx 1..6  + tx 11,12                        (|O| = 8, shared = 6 -> 60.0% / 75.0%, the issue's own worked example)
--   DUBLIN  sees tx 1..3                                    (|O| = 3, shared = 3 -> 30.0% / 100.0%)

INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type)
VALUES
  (-1220001, '0100', '122cmp0000000001', strftime('%Y-%m-%dT%H:%M:%SZ','now'), 1, 1),
  (-1220002, '0100', '122cmp0000000002', strftime('%Y-%m-%dT%H:%M:%SZ','now'), 1, 1),
  (-1220003, '0100', '122cmp0000000003', strftime('%Y-%m-%dT%H:%M:%SZ','now'), 1, 1),
  (-1220004, '0100', '122cmp0000000004', strftime('%Y-%m-%dT%H:%M:%SZ','now'), 1, 1),
  (-1220005, '0100', '122cmp0000000005', strftime('%Y-%m-%dT%H:%M:%SZ','now'), 1, 1),
  (-1220006, '0100', '122cmp0000000006', strftime('%Y-%m-%dT%H:%M:%SZ','now'), 1, 1),
  (-1220007, '0100', '122cmp0000000007', strftime('%Y-%m-%dT%H:%M:%SZ','now'), 1, 1),
  (-1220008, '0100', '122cmp0000000008', strftime('%Y-%m-%dT%H:%M:%SZ','now'), 1, 1),
  (-1220009, '0100', '122cmp0000000009', strftime('%Y-%m-%dT%H:%M:%SZ','now'), 1, 1),
  (-1220010, '0100', '122cmp0000000010', strftime('%Y-%m-%dT%H:%M:%SZ','now'), 1, 1),
  (-1220011, '0100', '122cmp0000000011', strftime('%Y-%m-%dT%H:%M:%SZ','now'), 1, 1),
  (-1220012, '0100', '122cmp0000000012', strftime('%Y-%m-%dT%H:%M:%SZ','now'), 1, 1);

INSERT INTO observations (transmission_id, observer_idx, direction, path_json, timestamp)
VALUES
  -- Kennedy Repeater (rowid 1, reference): tx 1..10
  (-1220001, 1, 'RX', '[]', CAST(strftime('%s','now') AS INTEGER)),
  (-1220002, 1, 'RX', '[]', CAST(strftime('%s','now') AS INTEGER)),
  (-1220003, 1, 'RX', '[]', CAST(strftime('%s','now') AS INTEGER)),
  (-1220004, 1, 'RX', '[]', CAST(strftime('%s','now') AS INTEGER)),
  (-1220005, 1, 'RX', '[]', CAST(strftime('%s','now') AS INTEGER)),
  (-1220006, 1, 'RX', '[]', CAST(strftime('%s','now') AS INTEGER)),
  (-1220007, 1, 'RX', '[]', CAST(strftime('%s','now') AS INTEGER)),
  (-1220008, 1, 'RX', '[]', CAST(strftime('%s','now') AS INTEGER)),
  (-1220009, 1, 'RX', '[]', CAST(strftime('%s','now') AS INTEGER)),
  (-1220010, 1, 'RX', '[]', CAST(strftime('%s','now') AS INTEGER)),
  -- GY889 Repeater (rowid 2): tx 1..6 (shared) + 11,12 (own) = 8 total, 6 shared
  (-1220001, 2, 'RX', '[]', CAST(strftime('%s','now') AS INTEGER)),
  (-1220002, 2, 'RX', '[]', CAST(strftime('%s','now') AS INTEGER)),
  (-1220003, 2, 'RX', '[]', CAST(strftime('%s','now') AS INTEGER)),
  (-1220004, 2, 'RX', '[]', CAST(strftime('%s','now') AS INTEGER)),
  (-1220005, 2, 'RX', '[]', CAST(strftime('%s','now') AS INTEGER)),
  (-1220006, 2, 'RX', '[]', CAST(strftime('%s','now') AS INTEGER)),
  (-1220011, 2, 'RX', '[]', CAST(strftime('%s','now') AS INTEGER)),
  (-1220012, 2, 'RX', '[]', CAST(strftime('%s','now') AS INTEGER)),
  -- DUBLIN Observer (rowid 3): tx 1..3 (all shared, 100% of its own traffic)
  (-1220001, 3, 'RX', '[]', CAST(strftime('%s','now') AS INTEGER)),
  (-1220002, 3, 'RX', '[]', CAST(strftime('%s','now') AS INTEGER)),
  (-1220003, 3, 'RX', '[]', CAST(strftime('%s','now') AS INTEGER)),
  -- A TX observation on a packet Kennedy also has RX -- must never count
  -- toward GY889's total/shared (#122 "exclude TX").
  (-1220007, 2, 'TX', '[]', CAST(strftime('%s','now') AS INTEGER));
