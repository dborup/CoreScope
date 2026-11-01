#!/bin/sh
# Regenerate test-fixtures/hop-path-598.json from the committed E2E fixture DB.
#
# #598 — test-issue-598-hop-path-inference.js measures HopResolver.inferPath()
# on the five longest 1-byte observation paths in test-fixtures/e2e-fixture.db.
# A unit test runs under plain `node` with no sqlite3 binding, so the rows it
# needs are extracted here into JSON and committed alongside the DB.
#
# Usage: sh tools/dump-hop-path-fixture.sh [db] [out]
set -eu
DB="${1:-test-fixtures/e2e-fixture.db}"
OUT="${2:-test-fixtures/hop-path-598.json}"
command -v python3 >/dev/null || { echo "python3 is required" >&2; exit 1; }
DB="$DB" OUT="$OUT" python3 - <<'PY'
import json, os, sqlite3
db = sqlite3.connect(os.environ['DB'])
db.row_factory = sqlite3.Row
nodes = [dict(public_key=r['public_key'], name=r['name'], role=r['role'], lat=r['lat'], lon=r['lon'])
         for r in db.execute('select public_key,name,role,lat,lon from nodes')]
observers = [dict(id=r['id'], iata=r['iata']) for r in db.execute('select id,iata from observers')]
# The five longest paths whose every hop is a 1-byte prefix.
rows = db.execute("""
  select o.id as id, t.hash as hash, o.path_json as path_json, o.resolved_path as resolved_path,
         (select id from observers ob where ob.rowid = o.observer_idx) as observer_id
    from observations o join transmissions t on t.id = o.transmission_id
   where o.path_json is not null and json_array_length(o.path_json) > 0
   order by json_array_length(o.path_json) desc, o.id
""").fetchall()
obs = []
for r in rows:
    hops = json.loads(r['path_json'])
    if not hops or any(len(h) != 2 for h in hops):
        continue
    obs.append(dict(id=r['id'], packet=r['hash'], observer_id=r['observer_id'],
                    path_json=r['path_json'], resolved_path=r['resolved_path']))
    if len(obs) == 5:
        break
obs.sort(key=lambda o: o['id'])
json.dump(dict(
    _source=('Extracted from test-fixtures/e2e-fixture.db; the five longest 1-byte observation '
             'paths plus the node and observer catalogue they resolve against. '
             'Regenerate with tools/dump-hop-path-fixture.sh.'),
    nodes=nodes, observers=observers, observations=obs,
), open(os.environ['OUT'], 'w'), indent=1, sort_keys=True)
print('%s: %d observations, %d nodes, %d observers' % (os.environ['OUT'], len(obs), len(nodes), len(observers)))
PY
