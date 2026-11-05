/* === CoreScope — hop-filter.js === */
/* #1633 — Render-time filter that hides 1-byte path hops in the "hide"
 * mode of the customize-v2 setting (#674: Show / Mark / Hide, Mark by
 * default). Pure render-time; firmware behavior and wire/store contents
 * are untouched.
 *
 * A "hop" here is a hex-string prefix as stored in observations.path_json
 * (e.g. "AB" = 1-byte, "CDEF" = 2-byte, "ABCDEF" = 3-byte). Byte count
 * is `floor(hopHex.length / 2)`.
 *
 * Wire & store stay intact: every consumer call site reads the toggle
 * via window.MC_getHide1ByteHops() and filters its rendered/aggregated
 * view at the boundary (no upstream mutation).
 */
'use strict';

(function () {
  // #674 — three modes for 1-byte path hops:
  //   show — as before #674, no markings;
  //   mark — (default) visible, uncertain hops marked in the map route view;
  //   hide — the #1633 behaviour: 1-byte hops left out everywhere.
  // The default is stored as "no key". The #1633 boolean key migrates once:
  // "true" becomes mode "hide"; anything else is dropped.
  var MODE_KEY = 'meshcore-1byte-hops-mode';
  var LEGACY_KEY = 'meshcore-hide-1byte-hops';
  var MODES = { show: true, mark: true, hide: true };
  var DEFAULT_MODE = 'mark';

  function get1ByteHopsMode() {
    try {
      var stored = localStorage.getItem(MODE_KEY);
      if (stored && MODES[stored]) return stored;
      var legacy = localStorage.getItem(LEGACY_KEY);
      if (legacy != null) {
        localStorage.removeItem(LEGACY_KEY);
        if (legacy === 'true') {
          localStorage.setItem(MODE_KEY, 'hide');
          return 'hide';
        }
      }
    } catch (_e) { /* storage unavailable */ }
    return DEFAULT_MODE;
  }

  function set1ByteHopsMode(mode) {
    if (!MODES[mode]) return;
    try {
      if (mode === DEFAULT_MODE) localStorage.removeItem(MODE_KEY);
      else localStorage.setItem(MODE_KEY, mode);
      localStorage.removeItem(LEGACY_KEY);
    } catch (_e) { /* private mode */ }
    if (typeof window !== 'undefined' && typeof window.CustomEvent === 'function') {
      window.dispatchEvent(new window.CustomEvent('mc-hide-1byte-hops-changed', {
        detail: { mode: mode, value: mode === 'hide' }
      }));
    }
  }

  // #1633 API, kept for its callers: "hide" is mode hide; turning it off
  // restores the pre-#674 unmarked view (show).
  function getHide1ByteHops() { return get1ByteHopsMode() === 'hide'; }

  function setHide1ByteHops(on) { set1ByteHopsMode(on ? 'hide' : 'show'); }

  // bytes of a hop hex token — handles undefined / non-string.
  function hopByteLen(h) {
    if (h == null) return 0;
    var s = String(h);
    return s.length >> 1;
  }

  // Packet-level path-hash size (1|2|3), or 0 when unresolvable.
  // Reads the path-length byte from raw_hex; its top two bits encode
  // hashSize-1. The route comes from THIS frame's own header byte (low 2
  // bits), not a caller-supplied route_type: one content hash can be heard
  // on several routes (cmd/server/store.go StoreTx.routeMask), so a group's
  // transmission-level route_type can describe a different observation than
  // the raw_hex being read here (#608). Offset is 5 for transport routes
  // (0/3, which carry 4 next/last-hop code bytes before path-length) else 1
  // — mirrors getPathLenOffset()/computeBreakdownRanges() in public/app.js.
  // This file loads before app.js in index.html, so it derives the route
  // itself rather than depending on that module's routeTypeFromRawHex().
  // Reading from raw_hex (not path hops) is correct even for zero-hop packets.
  function packetHashSize(rawHex) {
    if (!rawHex || typeof rawHex !== 'string') return 0;
    var clean = rawHex.replace(/\s+/g, '');
    var bytes = clean.length >> 1;
    if (bytes < 1) return 0;
    var headerByte = parseInt(clean.slice(0, 2), 16);
    if (isNaN(headerByte)) return 0;
    var routeType = headerByte & 0x03;
    var offset = (routeType === 0 || routeType === 3) ? 5 : 1;
    if (bytes < offset + 1) return 0;
    var pathByte = parseInt(clean.slice(offset * 2, offset * 2 + 2), 16);
    if (isNaN(pathByte)) return 0;
    return (pathByte >> 6) + 1;
  }

  // Render-time predicate. opts may be omitted — if so, falls back to the
  // current localStorage value. The hop is hidden only when:
  //   - opts.hide1ByteHops === true AND
  //   - the hop hex encodes exactly 1 byte (length === 2)
  // Anything else (origin/destination payload hops, multi-byte path hops,
  // null/undefined sentinels) stays visible — those callers already
  // bypass the filter when they pass undefined/falsey tokens.
  function isVisibleHop(hop, opts) {
    var enabled = (opts && typeof opts.hide1ByteHops === 'boolean')
      ? opts.hide1ByteHops
      : getHide1ByteHops();
    if (!enabled) return true;
    return hopByteLen(hop) !== 1;
  }

  // Filter a path hop array. Returns a NEW array; never mutates the input
  // (callers depend on the original path_json staying intact for downstream
  // consumers like hash-size detection / raw-hex rendering).
  function filterPathHops(hops, opts) {
    if (!hops || !hops.length) return hops || [];
    var enabled = (opts && typeof opts.hide1ByteHops === 'boolean')
      ? opts.hide1ByteHops
      : getHide1ByteHops();
    if (!enabled) return hops;
    var out = [];
    for (var i = 0; i < hops.length; i++) {
      if (hopByteLen(hops[i]) !== 1) out.push(hops[i]);
    }
    return out;
  }

  if (typeof window !== 'undefined') {
    window.MC_getHide1ByteHops = getHide1ByteHops;
    window.MC_setHide1ByteHops = setHide1ByteHops;
    window.MC_get1ByteHopsMode = get1ByteHopsMode;
    window.MC_set1ByteHopsMode = set1ByteHopsMode;
    window.MC_isVisibleHop = isVisibleHop;
    window.MC_filterPathHops = filterPathHops;
    window.MC_hopByteLen = hopByteLen;
    window.MC_packetHashSize = packetHashSize;
  }

  if (typeof module !== 'undefined' && module.exports) {
    module.exports = {
      getHide1ByteHops: getHide1ByteHops,
      setHide1ByteHops: setHide1ByteHops,
      get1ByteHopsMode: get1ByteHopsMode,
      set1ByteHopsMode: set1ByteHopsMode,
      isVisibleHop: isVisibleHop,
      filterPathHops: filterPathHops,
      hopByteLen: hopByteLen,
      packetHashSize: packetHashSize,
      _MODE_KEY: MODE_KEY,
      _LEGACY_KEY: LEGACY_KEY
    };
  }
})();
