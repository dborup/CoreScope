/* === CoreScope — packet-helpers.js (shared packet utilities) === */
'use strict';

/**
 * Cached JSON.parse helpers for packet data (issue #387).
 * Avoids repeated parsing of path_json / decoded_json on the same packet object.
 * Results are cached as _parsedPath / _parsedDecoded properties on the packet.
 *
 * Handles pre-parsed objects (non-string values) gracefully — returns them as-is.
 */

window.getParsedPath = function getParsedPath(p) {
  if (p._parsedPath !== undefined) return p._parsedPath || [];
  var raw = p.path_json;
  if (typeof raw !== 'string') {
    p._parsedPath = Array.isArray(raw) ? raw : [];
    return p._parsedPath;
  }
  try { p._parsedPath = JSON.parse(raw) || []; } catch (e) { p._parsedPath = []; }
  return p._parsedPath;
};

/**
 * Clear cached _parsedPath/_parsedDecoded from a packet object.
 * Must be called after spreading a parent packet into an observation/child,
 * otherwise the child inherits stale cached values from the parent (issue #504).
 */
window.clearParsedCache = function clearParsedCache(p) {
  delete p._parsedPath;
  delete p._parsedDecoded;
  delete p._parsedResolvedPath;
  delete p._parsedResolvedPathSource;
  return p;
};

/**
 * Parse resolved_path (server-side resolved full pubkeys).
 * Returns array of pubkey strings (or null entries) if present, or null if absent.
 * Cached as _parsedResolvedPath on the packet object.
 */
window.getResolvedPath = function getResolvedPath(p) {
  var raw = p.resolved_path;
  // WS/poll refreshes can replace the answer on the same row object. Do not
  // keep an old parsed answer (including old null) after its source changes.
  if (p._parsedResolvedPath !== undefined && p._parsedResolvedPathSource === raw) return p._parsedResolvedPath;
  p._parsedResolvedPathSource = raw;
  if (!raw) { p._parsedResolvedPath = null; return null; }
  if (typeof raw !== 'string') {
    p._parsedResolvedPath = Array.isArray(raw) ? raw : null;
    return p._parsedResolvedPath;
  }
  try {
    var parsed = JSON.parse(raw);
    p._parsedResolvedPath = Array.isArray(parsed) ? parsed : null;
  } catch (e) { p._parsedResolvedPath = null; }
  return p._parsedResolvedPath;
};

window.getParsedDecoded = function getParsedDecoded(p) {
  if (p._parsedDecoded !== undefined) return p._parsedDecoded || {};
  var raw = p.decoded_json;
  if (typeof raw !== 'string') {
    p._parsedDecoded = (raw && typeof raw === 'object') ? raw : {};
    return p._parsedDecoded;
  }
  try { p._parsedDecoded = JSON.parse(raw) || {}; } catch (e) { p._parsedDecoded = {}; }
  return p._parsedDecoded;
};

/**
 * Whether THIS observation's own frame proves it received a flood, which is
 * what makes its observer evidence for the end of the path (#165/#408/#608).
 *
 * A relaying node appends its OWN hash before forwarding a flood, so the last
 * recorded hop of a received flood is the relay the observer heard directly:
 * `firmware/src/Mesh.cpp:344-350` (`routeRecvPacket`, gated on
 * `isRouteFlood()`). Both flood routes qualify — `firmware/src/Packet.h:64`
 * `isRouteFlood()` covers ROUTE_TYPE_FLOOD (1) and ROUTE_TYPE_TRANSPORT_FLOOD
 * (0, "flood mode + transport codes", `Packet.h:14`). Scoped traffic is all
 * TRANSPORT_FLOOD, and this repo already classes 0 and 1 together
 * (`cmd/ingestor/client_reception.go`, `cmd/server/db.go`).
 *
 * Nothing of the sort holds otherwise:
 *   - a DIRECT route (2) or TRANSPORT_DIRECT (3) carries a supplied path, not
 *     a chain the relays appended (`Packet.h:65` `isRouteDirect()`);
 *   - a TRACE's path bytes are SNR values on a PLANNED route, so its last
 *     entry is not a relay this observer heard (upstream lists TRACE support
 *     as a follow-up for the same reason);
 *   - a transmitted observation did not hear anything.
 *
 * The route and payload type come from the frame's OWN header byte (#608) —
 * one content hash is heard on several routes, so a transmission-level
 * route_type can describe a different observation. An observation carrying no
 * frame of its own has no provenance and gets no anchor: the aggregate
 * packet's raw_hex must not stand in for it.
 *
 * @param {Object} obs - an observation (direction, raw_hex)
 */
window.observationIsRxFlood = function observationIsRxFlood(obs) {
  if (!obs || String(obs.direction || '').toLowerCase() !== 'rx') return false;
  var hex = typeof obs.raw_hex === 'string' ? obs.raw_hex : '';
  // Two full hex digits or there is no header byte to read: parseInt would
  // take a single nibble as a byte.
  if (!/^[0-9a-f]{2}/i.test(hex)) return false;
  var header = parseInt(hex.slice(0, 2), 16);
  // TRANSPORT_FLOOD (0) and FLOOD (1); DIRECT (2) and TRANSPORT_DIRECT (3) not.
  if ((header & 0x03) > 1) return false;
  return ((header >> 2) & 0x0f) !== 9;            // not a TRACE
};

/**
 * #598 r2 F5 — install the FULL node catalogue into the shared HopResolver,
 * once per page life.
 *
 * `HopResolver.ready()` is not the test for this. It only says "some nodes are
 * indexed", and the map page used to index its own filtered list
 * (`lastHeard=30d` plus the area filter); a packets page visited afterwards
 * then reused that narrower index and inferred different hops than a cold
 * packets page would. This helper keeps its own flag, so whichever page gets
 * here first installs the same full catalogue, observers and IATA centres, and
 * both pages feed the model the same inputs.
 *
 * Non-fatal on failure: hops render as unresolved prefixes and the next call
 * retries.
 *
 * @returns {Promise<boolean>} true once the full catalogue is in place
 */
var hopCatalogueLoad = null;
var hopCatalogueLoaded = false;
var hopObserverList = [];
window.ensureHopCatalogue = function ensureHopCatalogue() {
  if (hopCatalogueLoaded) return Promise.resolve(true);
  if (!hopCatalogueLoad) {
    hopCatalogueLoad = Promise.all([
      fetchAllNodes('', { ttl: 60000 }),
      api('/observers', { ttl: 60000 }),
      api('/iata-coords', { ttl: 300000 }).catch(function () { return { coords: {} }; }),
    ]).then(function (res) {
      var nodeData = res[0], obsData = res[1], coordData = res[2];
      var obsList = obsData.observers || obsData || [];
      window.HopResolver.init(nodeData.nodes || [], {
        observers: obsList,
        iataCoords: coordData.coords || {},
      });
      hopCatalogueLoaded = true;
      hopObserverList = obsList;
      return true;
    }).catch(function (e) {
      console.warn('[hops] catalogue init failed:', e);
      hopCatalogueLoad = null;
      return false;
    });
  }
  return hopCatalogueLoad;
};

/** Whether the full catalogue is installed (tests and callers that must know). */
window.hasHopCatalogue = function hasHopCatalogue() {
  return hopCatalogueLoaded;
};

/**
 * The observer list ensureHopCatalogue() installed, with the lat/lon
 * /api/observers reports. Callers need it to anchor the end of a path and
 * must not refetch it or fall back to a page-local, filtered list.
 */
window.hopObservers = function hopObservers() {
  return hopObserverList;
};

/**
 * #598 r2 F5 — where the sender of this packet is, or null.
 *
 * The packet detail pane and the map route view must anchor hop 0 on the same
 * point. The pane resolved the sender through `/api/nodes/{key}` and a name
 * search; the map read `decoded.lat` only, so a GRP_TXT or TXT_MSG packet (no
 * GPS in the payload) got an anchor in one view and none in the other. One
 * implementation now serves both.
 *
 * @param {Object} decoded - the observation's decoded_json
 * @returns {Promise<{lat,lon}|null>}
 */
window.senderPosition = function senderPosition(decoded) {
  var d = decoded || {};
  var lat = d.lat != null ? d.lat : (d.latitude != null ? d.latitude : null);
  var lon = d.lon != null ? d.lon : (d.longitude != null ? d.longitude : null);
  if (lat != null && lon != null) return Promise.resolve({ lat: lat, lon: lon });

  var key = d.pubKey || d.srcPubKey;
  var name = d.sender || d.name;
  var byKey = key
    ? api('/nodes/' + key, { ttl: 30000 }).catch(function () { return null; })
    : Promise.resolve(null);
  return byKey.then(function (nd) {
    var n = nd && nd.node;
    if (n && n.lat && n.lon) return { lat: n.lat, lon: n.lon };
    if (!name) return null;
    return api('/nodes/search?q=' + encodeURIComponent(name), { ttl: 30000 })
      .catch(function () { return null; })
      .then(function (sd) {
        var m = sd && sd.nodes && sd.nodes[0];
        return m && m.lat && m.lon ? { lat: m.lat, lon: m.lon } : null;
      });
  }).catch(function () { return null; });
};

/**
 * One observation's hops, inferred as a whole path (HopResolver.inferPath).
 * Shared by the packet detail pane and the map route view so both show the
 * same nodes for the selected observation (#598). The observation's own
 * resolved_path is the server hint — this never writes it back; the ingestor
 * stays authoritative (#1283) and the inference is display-only.
 *
 * The observer anchor (and its IATA-centre fallback) is applied only when this
 * observation's own frame proves a received flood, so neither caller can
 * borrow receiver evidence the data does not support. Both callers go through
 * here, which is what keeps the detail pane and the map route view in step.
 *
 * @param {Object} obs - an observation (path_json, observer_id, resolved_path)
 * @param {{lat,lon}|null} observer - where its observer is, when known
 * @param {{lat,lon}|null} origin - where the sender is, when known
 * @param {Object} [frame] - where to read direction/raw_hex for the provenance
 *   check, when `obs` carries the aggregate packet's bytes as a display
 *   fallback (packets.js observationPacket) rather than its own frame.
 *   Defaults to `obs`.
 * @returns {Array} one inferPath entry per hop
 */
window.inferObservationPath = function inferObservationPath(obs, observer, origin, frame) {
  var heard = window.observationIsRxFlood(frame || obs);
  return window.HopResolver.inferPath(window.getParsedPath(obs), {
    observer: heard ? observer : null,
    observerId: heard ? obs.observer_id : null,
    origin: origin,
    resolvedPath: window.getResolvedPath(obs),
  });
};

/**
 * Load the neighbour graph into HopResolver. Its links are evidence for
 * inferPath(), not a prerequisite: inferPath() works without them, so callers
 * render with whatever is loaded (hasHopAffinity) rather than wait.
 *
 * The graph is refetched once it is older than the api() cache TTL, on the
 * next call; the previous graph stays in use until the new one lands, and a
 * failed fetch keeps it and is retried on the following call.
 *
 * The default query shape is deliberate: it is the one the server keeps in its
 * 5-minute snapshot, so this costs one cached response per page life.
 *
 * @returns {Promise<boolean>} true once a graph is in place, false on failure
 */
var HOP_AFFINITY_TTL_MS = 300000;
var hopAffinityLoad = null;
var hopAffinityRequestedAt = 0;
var hopAffinityLoaded = false;
window.ensureHopAffinity = function ensureHopAffinity() {
  if (!hopAffinityLoad || Date.now() - hopAffinityRequestedAt > HOP_AFFINITY_TTL_MS) {
    hopAffinityRequestedAt = Date.now();
    hopAffinityLoad = api('/analytics/neighbor-graph', { ttl: HOP_AFFINITY_TTL_MS })
      .then(function (graph) {
        // #598 r2 F6 — inferPath()'s own graph, so opening a packet detail
        // cannot change what resolve() answers in the list, Live or node pages.
        window.HopResolver.setPathAffinity(graph);
        hopAffinityLoaded = true;
        return true;
      })
      .catch(function () {
        hopAffinityLoad = null;
        return false;
      });
  }
  return hopAffinityLoad;
};

/** Whether HopResolver holds a neighbour graph (possibly a previous one). */
window.hasHopAffinity = function hasHopAffinity() {
  return hopAffinityLoaded;
};

/**
 * A route-view hop for one inferPath entry. An unlisted hop becomes a
 * placeholder rather than the nearest node that happens to share its prefix.
 *
 * @param {string} hop - the hop's prefix
 * @param {Object} entry - its inferPath entry
 * @param {string} [role] - the node's role, for its glyph
 */
window.routeHopFromInferred = function routeHopFromInferred(hop, entry, role) {
  var e = entry || {};
  if (e.located) {
    return { lat: e.lat, lon: e.lon, name: e.name, pubkey: e.pubkey, role: role || null, resolved: true, confidence: e.confidence };
  }
  // A node the ingestor resolved that does not fit this point in the path: it
  // has a position, so "no GPS" would be wrong. Keep its name and mark the hop
  // unlisted, which is what the sidebar chip then says.
  if (e.pubkey && e.unreliable) {
    return { name: e.name, pubkey: e.pubkey, role: role || null, resolved: false, unlisted: true };
  }
  if (e.pubkey) return { name: e.name, pubkey: e.pubkey, role: role || null, resolved: false, gpsless: true };
  return { name: String(hop).slice(0, 8), pubkey: hop, resolved: false, unlisted: true };
};
