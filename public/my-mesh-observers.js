/* === CoreScope — my-mesh-observers.js ===
 *
 * Issue #262. A My Mesh card describes a device the operator owns. That
 * device may ALSO be a registered observer (an MQTT uplink reporting what it
 * receives), and that is a SEPARATE property from its mesh role: an observer
 * can be a repeater or a room server at the same time, so an OBSERVER badge
 * must never overwrite the node role.
 *
 * This helper answers three questions for one card, with no network access
 * of its own:
 *   1. is this node also an observer?  -- identity, by normalised FULL
 *      public key only (observers.id == nodes.public_key, modulo case),
 *      never by display name and never by a short prefix. "Dokk1" and
 *      "Dokk1 Meshview" are different devices that happen to share a name
 *      prefix, and the staging report in #262 came from conflating them.
 *   2. how is the observer reporting?  -- the observer's OWN status report
 *      (observers.last_seen) classified by the observers page's thresholds
 *      (window.observerHealthStatus, #1552/#1563). This file defines NO
 *      thresholds of its own; without that classifier the answer is an
 *      explicit Unknown, never a guessed Online/Offline.
 *   3. how much is the observer reporting?  -- observers.packetsLastHour,
 *      always labelled with its population AND its window. Missing or
 *      unusable data renders an em dash, never a fabricated zero.
 *
 * Performance: the caller fetches /api/observers ONCE per grid render and
 * builds one Map here (O(n) build, O(1) per card). There is no per-card
 * request and no nested scan of the observer list.
 */
(function (root) {
  'use strict';

  // The primary badge. The node's mesh role stays as its own secondary chip.
  var PRIMARY_BADGE = 'Observer';
  // Section headings that keep the two data sources apart on one card.
  var OBSERVER_SECTION_LABEL = 'Observer reporting';
  var NODE_SECTION_LABEL = 'Node activity';
  // The observer traffic metric states the population and the window. The
  // value behind it is observers.packetsLastHour, a rolling 60-minute count
  // of the packets THIS observer reported -- not the node's own traffic and
  // not a daily total.
  var TRAFFIC_LABEL = 'Packets reported · last hour';
  // Row labels sit directly under the OBSERVER_SECTION_LABEL heading, so
  // they do not repeat the word "observer"; each row's tooltip spells the
  // source out in full. Kept short so a 390 px card does not wrap them.
  var STATUS_ROW_LABEL = 'Status';
  var LAST_REPORT_ROW_LABEL = 'Last packet reported';
  var STATUS_REPORTED_PREFIX = ' · reported ';
  var UNKNOWN_VALUE = '—';
  var UNAVAILABLE_NOTICE = 'Observer status unavailable: the observer list could not be loaded, ' +
    'so observer identity, status and reported traffic are not shown on these cards.';
  var STATUS_TIP = 'The observer’s own status report over MQTT, classified by the observers page thresholds. ' +
    'Independent of the node’s adverts: a silent node can still be a reporting observer, and vice versa.';
  var TRAFFIC_TIP = 'Packets this observer reported in the last 60 minutes. Counts the observer’s receptions, ' +
    'not traffic sent by the node on this card.';

  // A full MeshCore public key / observer ID: exactly 64 hex characters.
  // Anything shorter, longer or non-hex is NOT an identity we will match on,
  // which is what makes prefix and name matching structurally impossible.
  var FULL_KEY = /^[0-9a-f]{64}$/;

  function normalizeId(value) {
    if (typeof value !== 'string') return null;
    var id = value.trim().toLowerCase();
    return FULL_KEY.test(id) ? id : null;
  }

  // index(payload) -> { byId: Map<normalisedKey, observerRow>, available }
  //
  // `available` is false whenever the caller did not get a usable observer
  // list (request failed, unexpected shape). The distinction matters: an
  // unavailable index must present as "unknown" in the UI, while an
  // available-but-empty one legitimately means "none of these nodes is an
  // observer".
  function index(payload) {
    var byId = new Map();
    var rows = payload && !Array.isArray(payload) && Array.isArray(payload.observers) ? payload.observers : null;
    if (!rows) return { byId: byId, available: false };
    for (var i = 0; i < rows.length; i++) {
      var id = normalizeId(rows[i] && rows[i].id);
      if (id) byId.set(id, rows[i]);
    }
    return { byId: byId, available: true };
  }

  // match(index, pubkey) -> observer row | null. Full-key equality only.
  function match(idx, pubkey) {
    if (!idx || !idx.available || !idx.byId || typeof idx.byId.get !== 'function') return null;
    var id = normalizeId(pubkey);
    if (!id) return null;
    return idx.byId.get(id) || null;
  }

  // roleSummary(nodeRole) -> 'Observer · Repeater' | 'Observer'. The mesh
  // role is retained as the secondary term and is never replaced.
  function roleSummary(nodeRole) {
    if (typeof nodeRole !== 'string') return PRIMARY_BADGE;
    var role = nodeRole.trim();
    if (!role) return PRIMARY_BADGE;
    return PRIMARY_BADGE + ' · ' + role.charAt(0).toUpperCase() + role.slice(1).toLowerCase();
  }

  function parseTs(value) {
    if (typeof value !== 'string' || !value.trim()) return null;
    return isFinite(Date.parse(value)) ? value : null;
  }

  // Maps the shared observers-page classifier's class onto the card's dot
  // classes. Only reached with a parseable status timestamp, so health-red
  // genuinely means Offline here rather than the classifier's no-data case.
  var DOT_BY_HEALTH = { 'health-green': 'status-ok', 'health-yellow': 'status-warn', 'health-red': 'status-err' };

  // status(observer, classify?) -> { label, cls, reportedAt } | null
  //
  // `classify` defaults to window.observerHealthStatus, the single source of
  // truth for observer liveness thresholds (#1552/#1563). When it is absent,
  // or the observer never reported a status, the result is Unknown with a
  // muted dot -- never a red Offline conclusion drawn from missing data.
  function status(observer, classify) {
    if (!observer) return null;
    var reportedAt = parseTs(observer.last_seen);
    var fn = typeof classify === 'function' ? classify
      : (root && typeof root.observerHealthStatus === 'function' ? root.observerHealthStatus : null);
    if (!reportedAt || !fn) return { label: 'Unknown', cls: 'status-muted', reportedAt: null };
    var h = fn(reportedAt) || {};
    return { label: h.label || 'Unknown', cls: DOT_BY_HEALTH[h.cls] || 'status-muted', reportedAt: reportedAt };
  }

  // lastReport(observer) -> the observer's last reported-packet timestamp,
  // or null when it never reported one / the value does not parse.
  function lastReport(observer) {
    return observer ? parseTs(observer.last_packet_at) : null;
  }

  // traffic(observer) -> { available, value, label }. A finite, non-negative
  // integer count renders; everything else renders UNKNOWN_VALUE so a failed
  // or absent metric is never presented as a measured zero.
  function traffic(observer) {
    var n = observer ? observer.packetsLastHour : undefined;
    var ok = typeof n === 'number' && isFinite(n) && n >= 0;
    return {
      available: ok,
      value: ok ? Number(n).toLocaleString('en-US') : UNKNOWN_VALUE,
      label: TRAFFIC_LABEL
    };
  }

  // pageHref(observer) -> the observer detail route for THIS observer, built
  // from the record's own id. observers rows are stored uppercase while nodes
  // rows are lowercase (#1836), so the node key must not be re-cased by hand.
  function pageHref(observer) {
    var id = observer && typeof observer.id === 'string' ? observer.id.trim() : '';
    if (!id) return null;
    return '#/observers/' + encodeURIComponent(id);
  }

  root.MyMeshObservers = {
    PRIMARY_BADGE: PRIMARY_BADGE,
    OBSERVER_SECTION_LABEL: OBSERVER_SECTION_LABEL,
    NODE_SECTION_LABEL: NODE_SECTION_LABEL,
    TRAFFIC_LABEL: TRAFFIC_LABEL,
    STATUS_ROW_LABEL: STATUS_ROW_LABEL,
    LAST_REPORT_ROW_LABEL: LAST_REPORT_ROW_LABEL,
    STATUS_REPORTED_PREFIX: STATUS_REPORTED_PREFIX,
    UNKNOWN_VALUE: UNKNOWN_VALUE,
    UNAVAILABLE_NOTICE: UNAVAILABLE_NOTICE,
    STATUS_TIP: STATUS_TIP,
    TRAFFIC_TIP: TRAFFIC_TIP,
    normalizeId: normalizeId,
    index: index,
    match: match,
    roleSummary: roleSummary,
    status: status,
    lastReport: lastReport,
    traffic: traffic,
    pageHref: pageHref
  };
})(typeof window !== 'undefined' ? window : this);
