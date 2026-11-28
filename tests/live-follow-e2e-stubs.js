/* tests/live-follow-e2e-stubs.js — the stubbed follow data shared by the
 * #880 Live "Follow a packet" E2E suites (test-issue-880-live-follow-e2e.js,
 * test-880-m3-follow-customizer-e2e.js). Moved here unchanged from the M1
 * suite (#880 M3), so both follow the same packet.
 *
 * The CI fixture has no packet heard by more than one observer, so:
 * /api/packets/<HASH|HASH2> is an ADVERT over three fixture relays heard by
 * six observers (one late, one naive clock, one skewed), and /api/observers
 * and /api/observers/clock-skew are stubbed to match.
 *
 * createFollowStubs(BASE) → { HASH, HASH2, pickNodes(), observers(),
 * detailFor(hash, t0Sec), frameFor(hash, ageMs), installRoutes(page) }.
 * pickNodes() must run first (it reads four positioned fixture nodes);
 * installRoutes counts requests in page.__fetches.
 */
'use strict';

const HASH = '880f0110a5ed0001';
const HASH2 = '880f0110a5ed0002';

function assert(cond, msg) { if (!cond) throw new Error(msg || 'assertion failed'); }

function createFollowStubs(BASE) {
  let FIX = null;

  async function pickNodes() {
    const res = await fetch(BASE + '/api/nodes?limit=2000');
    const data = await res.json();
    const list = (data.nodes || data).filter((n) => n.lat != null && n.lon != null && !(n.lat === 0 && n.lon === 0) && n.public_key);
    // Distinct 1-byte prefixes so the heuristic cannot mix them up.
    const seen = new Set();
    const pick = [];
    for (const n of list) {
      const p = n.public_key.slice(0, 2).toUpperCase();
      if (seen.has(p)) continue;
      seen.add(p);
      pick.push(n);
      if (pick.length === 4) break;
    }
    assert(pick.length === 4, 'fixture needs 4 positioned nodes, got ' + pick.length);
    FIX = pick;
    return pick;
  }

  function observers() {
    const [o, r1, r2, r3] = FIX;
    const near = (n, d) => [n.lat + d, n.lon + d];
    return [
      { id: 'F880A', name: 'Obs Alpha', iata: 'CPH', pos: near(o, 0.01), clock_naive: false },
      { id: 'F880B', name: 'Obs Bravo', iata: 'CPH', pos: near(r1, 0.01), clock_naive: false },
      { id: 'F880C', name: 'Obs Charlie', iata: 'AAR', pos: near(r1, -0.01), clock_naive: true },
      { id: 'F880D', name: 'Obs Delta', iata: 'AAR', pos: near(r2, 0.01), clock_naive: false },
      { id: 'F880E', name: 'Obs Echo <b>x</b>', iata: 'ODE', pos: near(r3, 0.01), clock_naive: false },
      { id: 'F880F', name: 'Obs Foxtrot', iata: 'ODE', pos: near(r1, 0.02), clock_naive: false },
    ];
  }

  function detailFor(hash, t0Sec) {
    const [o, r1, r2, r3] = FIX;
    const pre = (n) => n.public_key.slice(0, 2).toUpperCase();
    const iso = (s) => new Date(s * 1000).toISOString();
    const ob = observers();
    const mk = (id, idx, dt, relays) => ({
      id, observer_id: ob[idx].id, observer_name: ob[idx].name, observer_iata: ob[idx].iata,
      snr: 5, rssi: -90, path_json: JSON.stringify(relays.map(pre)),
      resolved_path: relays.map((n) => n.public_key), timestamp: iso(t0Sec + dt),
    });
    return {
      packet: {
        hash, route_type: 1, payload_type: 4, first_seen: iso(t0Sec),
        decoded_json: JSON.stringify({ type: 'ADVERT', pubKey: o.public_key, name: o.name, lat: o.lat, lon: o.lon }),
      },
      path: [],
      observation_count: 7,
      observations: [
        mk(1, 0, 0, []),
        mk(2, 1, 1, [r1]),
        mk(3, 2, 0, [r1]),
        mk(4, 3, 2, [r1, r2]),
        mk(5, 4, 3, [r1, r2, r3]),
        mk(6, 4, 5, [r1, r2, r3, r1]), // later and deeper: not Echo's first report
        mk(7, 5, 95, [r1]),           // late upload
      ],
    };
  }

  // A WS-shaped frame for bufferPacket(): the transmission as Live sees it.
  function frameFor(hash, ageMs) {
    const [o, r1] = FIX;
    return {
      id: 880, hash, timestamp: new Date(Date.now() - (ageMs || 10000)).toISOString(),
      route_type: 1, payload_type: 4, observer_id: 'F880B', observer_name: 'Obs Bravo',
      path_json: JSON.stringify([r1.public_key.slice(0, 2).toUpperCase()]),
      resolved_path: [r1.public_key],
      decoded: { header: { payloadTypeName: 'ADVERT' },
        payload: { type: 'ADVERT', pubKey: o.public_key, name: o.name, lat: o.lat, lon: o.lon } },
    };
  }

  async function installRoutes(page) {
    page.__fetches = { [HASH]: 0, [HASH2]: 0, skew: 0, other: [] };
    const t0 = Math.floor(Date.now() / 1000) - 120;
    await page.route(/\/api\/packets\/880f0110a5ed000[12](\?.*)?$/, (route) => {
      const h = route.request().url().match(/880f0110a5ed000[12]/)[0];
      page.__fetches[h]++;
      route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(detailFor(h, t0)) });
    });
    await page.route(/\/api\/packets\/[0-9a-z]+(\?.*)?$/, (route) => {
      const u = route.request().url();
      if (!/880f0110a5ed000[12]/.test(u) && /\/api\/packets\/[0-9a-f]{16}/.test(u)) page.__fetches.other.push(u);
      route.fallback();
    });
    await page.route(/\/api\/observers\/clock-skew(\?.*)?$/, (route) => {
      page.__fetches.skew++;
      route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify([{ observerID: 'F880D', offsetSec: 900, samples: 20 }]) });
    });
    await page.route(/\/api\/observers(\?.*)?$/, (route) => {
      const list = observers().map((o) => ({ id: o.id, name: o.name, iata: o.iata, lat: o.pos[0], lon: o.pos[1], clock_naive: o.clock_naive }));
      route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ observers: list, server_time: new Date().toISOString() }) });
    });
  }

  return { HASH, HASH2, pickNodes, observers, detailFor, frameFor, installRoutes };
}

module.exports = { createFollowStubs, HASH, HASH2 };
