/* === CoreScope — nodes-export.js (MeshCore companion contacts export) === */
'use strict';

/*
 * Exports a caller-supplied, already-ordered list of nodes as a MeshCore
 * companion-app config file: { "contacts": [ … ] }. Kept out of nodes.js so
 * the field mapping/validation stays a self-contained, independently
 * testable unit -- ordering (sort column, claimed/favorites pinning) is the
 * CALLER's responsibility (see nodes.js's computeDisplayOrder()); this file
 * never reorders what it's given, so the export is byte-for-byte WYSIWYG
 * with whatever order the table is showing (#1889 review fix 1).
 *
 * Field-shape verified 2026-08 against the primary/official MeshCore
 * firmware + reference Python bindings (github.com/meshcore-dev/MeshCore,
 * github.com/meshcore-dev/meshcore_py) -- the closed-source companion app
 * itself could not be inspected directly. Per field:
 *   - type: firmware src/helpers/AdvertDataHelpers.h defines
 *     ADV_TYPE_NONE=0, ADV_TYPE_CHAT=1 (companion), ADV_TYPE_REPEATER=2,
 *     ADV_TYPE_ROOM=3, ADV_TYPE_SENSOR=4, with 5-15 explicitly commented
 *     "//FUTURE" (reserved) -- confirmed, drives ROLE_TYPE_MAP below.
 *   - public_key: wire format is a raw pubkey; the reference reader
 *     (meshcore_py reader.py) hex-encodes it lowercase, but contact
 *     lookups elsewhere in meshcore_py are case-insensitive on import, so
 *     preserving CoreScope's own stored case (whatever the ingestor wrote)
 *     is safe and is what we do here -- never forced to lower/upper.
 *   - flags: firmware MyMesh.cpp uses only bit 0 (LSB) as a "favourite"
 *     flag; 0 (not favourited, no other bits) is a safe, correct default
 *     for a freshly-exported contact.
 *   - last_advert vs last_modified: firmware ContactInfo.h documents these
 *     as DISTINCT clocks -- `last_advert_timestamp` ("by THEIR clock", i.e.
 *     when that node was last heard advertising) vs `lastmod` ("by OUR
 *     clock", i.e. when the local contact record itself was last edited).
 *     CoreScope has no concept of "locally edited contact record" (it's a
 *     passive network monitor, not a companion device with an editable
 *     address book), so node.last_seen -- our own timestamp of when we
 *     last heard this node -- is the correct source for last_advert, and
 *     last_modified mirroring it is a deliberate best-effort default in
 *     the absence of any better source, not a literal semantic match.
 *   - custom_name / out_path_list: NOT present anywhere in the open
 *     firmware or meshcore_py source (the real wire fields are `name` only,
 *     and `out_path` + `out_path_len` with a -1/0xFF "unknown" sentinel,
 *     respectively) -- these two keys and null defaults are unverified
 *     against a primary source; kept as-is because that's what the
 *     companion app is reported to write for a fresh contact and changing
 *     them isn't part of this fix's scope.
 *   - latitude/longitude as strings: the wire protocol itself encodes
 *     these as int32 degrees*1e6 (ContactInfo.h), decoded to plain floats
 *     by meshcore_py -- no primary source confirms the companion app's
 *     JSON *export file* (as opposed to the live wire protocol) stringifies
 *     them. Kept as strings per the existing/reviewed format; flagged here
 *     as an open compatibility question for whoever can check a real
 *     companion-app-exported file.
 */

(function () {
  // #1889 review fix 2: the full decoded-ADVERT role vocabulary (see
  // cmd/ingestor/decoder.go's advertRole()) plus the companion type enum.
  // A role with no entry here (and no matching type-N, N in [5,15]) is
  // UNKNOWN and must be skipped -- never silently coerced to companion.
  var ROLE_TYPE_MAP = { none: 0, companion: 1, repeater: 2, room: 3, sensor: 4 };
  var TYPE_N_RE = /^type-(\d+)$/;
  var MIN_RESERVED_TYPE = 5;
  var MAX_RESERVED_TYPE = 15;

  // Returns the companion `type` int for a role string, or null when the
  // role is empty/missing/unrecognized (caller must skip the node).
  function roleToType(role) {
    var r = String(role == null ? '' : role).trim().toLowerCase();
    if (!r) return null;
    if (Object.prototype.hasOwnProperty.call(ROLE_TYPE_MAP, r)) return ROLE_TYPE_MAP[r];
    var m = TYPE_N_RE.exec(r);
    if (m) {
      var n = parseInt(m[1], 10);
      if (n >= MIN_RESERVED_TYPE && n <= MAX_RESERVED_TYPE) return n;
    }
    return null;
  }

  // #1889 review fix 3: full 64-char hex only, case preserved (never
  // forced to lower/upper -- see file header note on public_key case).
  var PUBKEY_RE = /^[0-9a-fA-F]{64}$/;
  function validPubkey(v) {
    return typeof v === 'string' && PUBKEY_RE.test(v);
  }

  // #1889 review fix 4: finite number within [min,max]. Rejects NaN,
  // +/-Infinity, empty/whitespace-only strings, and out-of-range values.
  // A plain 0 is a legitimate value here (equator/prime-meridian nodes) --
  // the "null island" (0,0) rule is applied by the caller, not here.
  function validCoord(raw, min, max) {
    if (raw === null || raw === undefined) return null;
    if (typeof raw === 'string' && raw.trim() === '') return null;
    var n = Number(raw);
    if (!isFinite(n)) return null;
    if (n < min || n > max) return null;
    return n;
  }

  function epochSeconds(ts) {
    if (!ts) return 0;
    var ms = new Date(ts).getTime();
    return isNaN(ms) ? 0 : Math.floor(ms / 1000);
  }

  // Returns the companion contact for a node, or null when the node cannot
  // be represented. Skip rules (all must pass):
  //   - name: non-empty string after trimming
  //   - public_key: exactly 64 hex chars (case preserved on output)
  //   - role: must map to a known type (see roleToType) -- unknown/empty
  //     roles are skipped, never defaulted to companion
  //   - lat/lon: finite numbers in range, not both exactly zero
  function contactFor(n) {
    if (!n) return null;
    if (typeof n.name !== 'string' || !n.name.trim()) return null;
    if (!validPubkey(n.public_key)) return null;
    var type = roleToType(n.role);
    if (type === null) return null;
    var lat = validCoord(n.lat, -90, 90);
    var lon = validCoord(n.lon, -180, 180);
    if (lat === null || lon === null) return null;
    if (lat === 0 && lon === 0) return null; // null island

    var advert = epochSeconds(n.last_seen);
    return {
      type: type,
      name: n.name,
      custom_name: null,
      public_key: n.public_key,
      flags: 0,
      latitude: String(lat),
      longitude: String(lon),
      last_advert: advert,
      last_modified: advert,
      out_path_list: null,
    };
  }

  // `nodes` must already be in the desired output order -- this function
  // never sorts or reorders, only maps + filters (#1889 review fix 1).
  function buildContacts(nodes) {
    var contacts = [];
    (nodes || []).forEach(function (n) {
      var c = contactFor(n);
      if (c) contacts.push(c);
    });
    return { contacts: contacts };
  }

  function pad2(v) { return v < 10 ? '0' + v : String(v); }

  function filename(areaKey, date) {
    var d = date || new Date();
    var stamp = d.getFullYear() + '-' + pad2(d.getMonth() + 1) + '-' + pad2(d.getDate()) +
      '-' + pad2(d.getHours()) + pad2(d.getMinutes()) + pad2(d.getSeconds());
    var area = String(areaKey || '').replace(/[^A-Za-z0-9_-]+/g, '_').replace(/^_+|_+$/g, '');
    return 'corescope_nodes_' + (area || 'all') + '_' + stamp + '.json';
  }

  // Triggers the browser download. `nodes` must already be in the desired
  // output order (see buildContacts). Returns the number of exported
  // contacts.
  function download(nodes, areaKey) {
    var payload = buildContacts(nodes);
    var blob = new Blob([JSON.stringify(payload, null, 2)], { type: 'application/json;charset=utf-8' });
    var url = URL.createObjectURL(blob);
    var a = document.createElement('a');
    a.href = url;
    a.download = filename(areaKey, new Date());
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
    setTimeout(function () { URL.revokeObjectURL(url); }, 0);
    return payload.contacts.length;
  }

  window.NodesExport = {
    buildContacts: buildContacts,
    filename: filename,
    download: download,
    // Exposed for unit testing the mapping/validation rules in isolation.
    roleToType: roleToType,
    validPubkey: validPubkey,
    validCoord: validCoord,
    contactFor: contactFor,
  };
})();
