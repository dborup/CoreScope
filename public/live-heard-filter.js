/* === CoreScope — live-heard-filter.js === */
/* #660 — Live map "Last heard" filter. Pure logic, no DOM: live.js owns the
 * select, the markers and the timer. Loaded after roles.js; the freshness
 * helper is passed in (getNodeFreshness) rather than read from window.
 *
 * Basis: "any traffic". The candidates are getNodeFreshness's fixed set
 * minus _liveSeen ({ includeLiveSeen: false }), because live.js stamps
 * _liveSeen with the page-load time on every API-loaded node. What is left:
 *   - last_seen / last_heard from /api/nodes. Both carry nodes.last_seen,
 *     which the ingestor advances on the node's own adverts AND on resolved
 *     flood relay hops (cmd/ingestor/db.go touchRelayNodesLocked, 5 min
 *     debounce). There is no advert-only timestamp in the bulk node list,
 *     so an "adverts only" basis is not offered.
 *   - _lastHeard, stamped by live.js from WS packets via heardKeysFromPacket
 *     (same evidence rule as the ingestor), so a node comes back as soon as
 *     traffic arrives instead of on the next reload.
 *
 * Scope: repeater and room only. Companions and sensors sleep, roam and go
 * quiet by design; their silence is not an outage signal.
 *
 * Missing or unparsable timestamps: an in-scope node with no valid
 * candidate is hidden. The filter shows nodes with evidence of being heard
 * inside the window, and such a node has none.
 */
'use strict';

(function () {
  var H = 3600000;
  // Hardcoded for now; exposing the list/default in the customizer is a
  // follow-up (AGENTS.md rule 8).
  var OPTIONS = [
    { key: 'all', ms: null, label: 'All' },
    { key: '1h', ms: H, label: '1 h' },
    { key: '3h', ms: 3 * H, label: '3 h' },
    { key: '6h', ms: 6 * H, label: '6 h' },
    { key: '12h', ms: 12 * H, label: '12 h' },
    { key: '24h', ms: 24 * H, label: '24 h' },
    { key: '7d', ms: 7 * 24 * H, label: '7 d' }
  ];
  var DEFAULT_KEY = 'all';
  var BY_KEY = Object.create(null);
  OPTIONS.forEach(function (o) { BY_KEY[o.key] = o; });

  // Route types 0 (TRANSPORT_FLOOD) and 1 (FLOOD); payload 9 is TRACE,
  // whose path bytes are SNR values. Mirrors cmd/ingestor/db.go
  // pathHopsTransmitted (internal/packetpath PathBytesAreHops, #623).
  var PAYLOAD_TRACE = 9;

  function normalizeKey(raw) {
    return typeof raw === 'string' && BY_KEY[raw] ? raw : DEFAULT_KEY;
  }

  function windowMs(key) {
    return BY_KEY[normalizeKey(key)].ms;
  }

  // A valid hash value wins (deep link), then the saved value, then All.
  function resolveInitialKey(hashValue, storedValue) {
    if (typeof hashValue === 'string' && BY_KEY[hashValue]) return hashValue;
    return normalizeKey(storedValue);
  }

  function inScope(role) {
    var r = typeof role === 'string' ? role.toLowerCase() : '';
    return r === 'repeater' || r === 'room';
  }

  // Visible iff age < window (same comparison as roles.js _statusFromAge and
  // the Map page's server-side `last_seen > since`).
  function isHidden(node, winMs, nowMs, getFreshness) {
    if (winMs == null || !node || typeof node !== 'object' || !inScope(node.role)) return false;
    var fresh = getFreshness(node, nowMs, { includeLiveSeen: false });
    return fresh === null || nowMs - fresh.timestampMs >= winMs;
  }

  // Pubkeys one WS packet proves were transmitting just now: the signed
  // sender of an ADVERT, and the server-resolved relay hops of a flood
  // packet. resolved_path only holds unambiguous resolutions (null
  // otherwise), so a 1-byte prefix collision cannot keep a dead repeater
  // alive. The WS route_type/payload_type are transmission-level.
  function heardKeysFromPacket(pkt) {
    var keys = [];
    if (!pkt || typeof pkt !== 'object') return keys;
    var seen = Object.create(null);
    function add(k) {
      if (typeof k === 'string' && k !== '' && !seen[k]) { seen[k] = true; keys.push(k); }
    }
    var d = pkt.decoded;
    if (d && d.header && d.header.payloadTypeName === 'ADVERT' && d.payload) add(d.payload.pubKey);
    var route = pkt.route_type, ptype = pkt.payload_type;
    if ((route === 0 || route === 1) && typeof ptype === 'number' && ptype !== PAYLOAD_TRACE &&
        Array.isArray(pkt.resolved_path)) {
      for (var i = 0; i < pkt.resolved_path.length; i++) add(pkt.resolved_path[i]);
    }
    return keys;
  }

  var api = {
    OPTIONS: OPTIONS,
    DEFAULT_KEY: DEFAULT_KEY,
    STORAGE_KEY: 'live-heard-filter',
    HASH_PARAM: 'heard',
    normalizeKey: normalizeKey,
    windowMs: windowMs,
    resolveInitialKey: resolveInitialKey,
    inScope: inScope,
    isHidden: isHidden,
    heardKeysFromPacket: heardKeysFromPacket
  };
  if (typeof window !== 'undefined') window.LiveHeardFilter = api;
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
})();
