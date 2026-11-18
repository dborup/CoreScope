/* === CoreScope — live-follow.js === */
/* #880 M1 — Live "Follow a packet". Pure logic, no DOM: live.js owns the
 * fetch, the map layers, the card and the timers. Dependencies (observer
 * roster, clock-skew list, hop resolver, severity and type-name helpers,
 * haversine) are passed in, never read from window.
 *
 * Data: one GET /api/packets/{hash} per follow. Its observations carry the
 * observer-reported receive time in whole seconds (observations.timestamp
 * is INTEGER Unix seconds, cmd/ingestor/db.go), so offsets are whole
 * seconds and never "+0.8 s".
 *
 * Clock: hop depth, not timestamps. MeshCore floods append each relay's
 * hash to the path (firmware src/Mesh.cpp, routeRecvPacket), so path[j]
 * retransmitted at step j + 1 and an observation with h hops heard the
 * step-h transmission. Depth is causal and unaffected by clock skew; the
 * reported seconds are labels and order observers inside one step.
 *
 * Clock skew: flag only, never correct. The per-observer offsetSec from
 * /api/observers/clock-skew is a median deviation from the other observers
 * of the same adverts, so it contains real propagation delay as well as
 * clock error (cmd/server/clock_skew.go calibrateObservers). Subtracting
 * it would hide the spread this view exists to show.
 */
'use strict';

(function () {
  var HASH_PARAM = 'follow';
  var HASH_RE = /^[0-9a-f]{16}$/;
  // Bounds (AGENTS.md rule 0). /api/packets/{hash} returns every
  // observation of the hash; only the earliest 200 are processed.
  var MAX_OBSERVATIONS = 200;
  // firmware src/MeshCore.h MAX_PATH_SIZE.
  var MAX_DEPTH = 64;
  // Replay timing. Hardcoded for now; replay speed and length belong in the
  // customizer later (AGENTS.md rule 8, #880 M3).
  var TARGET_MS = 6000;
  var MIN_STEP_MS = 350;
  var MAX_STEP_MS = 1500;
  var SPEEDS = [0.5, 1, 2];
  var STAGGER_FRAC = 0.6;
  var LATE_SEC = 60;
  var CAPTION = 'Times as reported by observers (1 s resolution, not clock-corrected).';
  var KIND_ORDER = { origin: 0, ring: 1, relay: 2, observer: 3, summary: 4 };

  function parseFollowParam(v) {
    if (typeof v !== 'string') return null;
    var s = v.trim().toLowerCase();
    return HASH_RE.test(s) ? s : null;
  }

  // Observation time → whole Unix seconds, or null. The API sends ISO
  // strings; numbers (seconds or milliseconds) and digit strings are
  // accepted too.
  function toEpochSec(ts) {
    var n = null;
    if (typeof ts === 'number') n = ts;
    else if (typeof ts === 'string' && ts.trim() !== '') {
      n = /^\d+$/.test(ts.trim()) ? Number(ts) : Date.parse(ts) / 1000;
    }
    if (n == null || !isFinite(n)) return null;
    if (n > 1e12) n = n / 1000;
    return Math.floor(n);
  }

  function num(v) {
    if (v == null || v === '') return null;
    var n = Number(v);
    return isFinite(n) ? n : null;
  }
  function toPos(lat, lon) {
    var a = num(lat), b = num(lon);
    if (a == null || b == null || (a === 0 && b === 0)) return null;
    return [a, b];
  }

  function parseHops(pathJson) {
    var v = pathJson;
    if (typeof v === 'string') {
      try { v = JSON.parse(v); } catch (_) { return []; }
    }
    if (!Array.isArray(v)) return [];
    var out = [];
    for (var i = 0; i < v.length; i++) if (typeof v[i] === 'string' && v[i]) out.push(v[i]);
    return out;
  }

  function parseList(v) {
    if (Array.isArray(v)) return v;
    if (typeof v === 'string') {
      try { var p = JSON.parse(v); return Array.isArray(p) ? p : null; } catch (_) { return null; }
    }
    return null;
  }

  function parseDecoded(packet) {
    var d = packet && packet.decoded_json;
    if (d && typeof d === 'object') return d;
    if (typeof d === 'string') {
      try { var p = JSON.parse(d); if (p && typeof p === 'object') return p; } catch (_) { /* fall through */ }
    }
    return {};
  }

  // /api/observers → { id: { id, name, iata, pos, clockNaive } }.
  // Accepts { observers: [...] } and a bare array (see live.js
  // buildObserverIataMap, #1136).
  function buildObserverRoster(data) {
    var list = Array.isArray(data) ? data : (data && Array.isArray(data.observers) ? data.observers : []);
    var m = Object.create(null);
    for (var i = 0; i < list.length; i++) {
      var o = list[i];
      if (!o || o.id == null) continue;
      m[o.id] = { id: String(o.id), name: o.name || null, iata: o.iata || null,
        pos: toPos(o.lat, o.lon), clockNaive: !!o.clock_naive };
    }
    return m;
  }

  // /api/observers/clock-skew ([{observerID, offsetSec, samples}]) → { id: offsetSec }.
  function buildSkewMap(list) {
    var m = Object.create(null);
    if (!Array.isArray(list)) return m;
    for (var i = 0; i < list.length; i++) {
      var e = list[i];
      var off = e && num(e.offsetSec);
      if (e && e.observerID != null && off != null) m[e.observerID] = off;
    }
    return m;
  }

  // null | 'naive' | 'warning' | 'critical'. Flag only — see the header.
  function clockFlag(id, rosterEntry, skew, severity) {
    if (rosterEntry && rosterEntry.clockNaive) return 'naive';
    var off = skew ? skew[id] : undefined;
    if (off == null || typeof severity !== 'function') return null;
    var sev = severity(off);
    return sev && sev !== 'ok' ? sev : null;
  }

  // Flood routes only (firmware src/Packet.h: TRANSPORT_FLOOD 0, FLOOD 1).
  // A DIRECT path is the remaining source route, and TRACE path bytes are
  // SNR values, so neither describes how the packet spread.
  function isReplayable(routeType, payloadType) {
    return (routeType === 0 || routeType === 1) && payloadType !== 9;
  }

  function cmpObs(a, b) {
    if (a.t == null && b.t != null) return 1;
    if (a.t != null && b.t == null) return -1;
    if (a.t !== b.t) return a.t - b.t;
    return a.id - b.id;
  }
  function cmpHeard(a, b) {
    if (a.deltaSec == null && b.deltaSec != null) return 1;
    if (a.deltaSec != null && b.deltaSec == null) return -1;
    if (a.deltaSec !== b.deltaSec) return a.deltaSec - b.deltaSec;
    return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
  }

  /**
   * GET /api/packets/{hash} response → follow model.
   * deps: { roster, skew, resolveHops(hops, payload, resolvedPath) → per-hop
   * [{key, name, pos|null}|null], severity(offsetSec), typeName(payloadType) }.
   * O(N·H) for N ≤ 200 observations and H ≤ 64 hops.
   */
  function reduceFollow(detail, deps) {
    deps = deps || {};
    var roster = deps.roster || {};
    var packet = (detail && detail.packet) || {};
    var payload = parseDecoded(packet);
    var routeType = num(packet.route_type);
    var payloadType = num(packet.payload_type);
    var replayable = isReplayable(routeType, payloadType);
    var all = (detail && Array.isArray(detail.observations)) ? detail.observations : [];

    var list = [];
    for (var i = 0; i < all.length; i++) {
      if (all[i]) list.push({ o: all[i], t: toEpochSec(all[i].timestamp), id: num(all[i].id) || 0 });
    }
    list.sort(cmpObs);
    var total = list.length;
    if (list.length > MAX_OBSERVATIONS) list = list.slice(0, MAX_OBSERVATIONS);

    var originPos = payload.pubKey ? toPos(payload.lat, payload.lon) : null;
    var origin = originPos
      ? { key: String(payload.pubKey), name: payload.name || String(payload.pubKey).slice(0, 8), pos: originPos }
      : null;

    var relays = [];
    var relayIdx = Object.create(null);
    var byObserver = Object.create(null);
    var observers = [];
    var segments = Object.create(null);
    var segList = [];

    function addSegment(a, b) {
      var k = a.key + '|' + b.key;
      if (segments[k]) return;
      segments[k] = true;
      segList.push({ from: a, to: b });
    }

    for (var li = 0; li < list.length; li++) {
      var item = list[li];
      var o = item.o;
      var hops = replayable ? parseHops(o.path_json).slice(0, MAX_DEPTH) : [];
      var oid = o.observer_id != null ? String(o.observer_id) : 'obs-' + item.id;
      var r = roster[oid] || null;
      var obsNode = { key: 'obs:' + oid, name: o.observer_name || (r && r.name) || oid.slice(0, 8), pos: r ? r.pos : null };

      var chain = origin ? [origin] : [];
      if (hops.length) {
        var resolvedPath = parseList(o.resolved_path);
        var entries = (typeof deps.resolveHops === 'function')
          ? (deps.resolveHops(hops, payload, resolvedPath) || []) : [];
        for (var j = 0; j < hops.length; j++) {
          var e = entries[j];
          var key = e && e.key ? String(e.key) : 'hop-' + hops[j];
          var depth = j + 1;
          var ri = relayIdx[key];
          if (ri == null) {
            relayIdx[key] = relays.length;
            relays.push({ key: key, name: (e && e.name) || hops[j], pos: (e && e.pos) || null, depth: depth });
          } else if (depth < relays[ri].depth) {
            relays[ri].depth = depth;
          }
          chain.push(relays[relayIdx[key]]);
        }
      }
      chain.push(obsNode);
      if (replayable) for (var c = 1; c < chain.length; c++) addSegment(chain[c - 1], chain[c]);

      var prevIdx = byObserver[oid];
      var prev = prevIdx == null ? null : observers[prevIdx];
      // Earliest report per observer (list is time-sorted); same second →
      // fewer hops wins.
      if (!prev || (prev.tSec === item.t && hops.length < prev.hops)) {
        var rec = { id: oid, name: obsNode.name, iata: o.observer_iata || (r && r.iata) || null,
          pos: obsNode.pos, hops: hops.length, tSec: item.t, deltaSec: null, late: false,
          clock: clockFlag(oid, r, deps.skew, deps.severity) };
        if (prev) observers[prevIdx] = rec;
        else { byObserver[oid] = observers.length; observers.push(rec); }
      }
    }

    var tMin = null, tMax = null, depthMax = 0;
    for (var k = 0; k < observers.length; k++) {
      var ob = observers[k];
      if (ob.hops > depthMax) depthMax = ob.hops;
      if (ob.tSec == null) continue;
      if (tMin == null || ob.tSec < tMin) tMin = ob.tSec;
      if (tMax == null || ob.tSec > tMax) tMax = ob.tSec;
    }
    for (var q = 0; q < observers.length; q++) {
      var x = observers[q];
      if (x.tSec != null && tMin != null) {
        x.deltaSec = x.tSec - tMin;
        x.late = x.deltaSec > LATE_SEC;
      }
    }
    observers.sort(cmpHeard);
    for (var ri2 = 0; ri2 < relays.length; ri2++) if (relays[ri2].depth > depthMax) depthMax = relays[ri2].depth;

    return {
      hash: parseFollowParam(String(packet.hash || '')) || '',
      typeName: typeof deps.typeName === 'function' ? (deps.typeName(payloadType) || 'UNKNOWN') : 'UNKNOWN',
      routeType: routeType,
      replayable: replayable,
      origin: origin,
      relays: relays,
      observers: observers,
      segments: segList,
      depth: Math.min(depthMax, MAX_DEPTH),
      tMin: tMin,
      tMax: tMax,
      total: total,
      capped: total > MAX_OBSERVATIONS,
    };
  }

  function stepMsFor(depth, opts) {
    opts = opts || {};
    var speed = SPEEDS.indexOf(opts.speed) >= 0 ? opts.speed : 1;
    var target = num(opts.targetMs) || TARGET_MS;
    var d = Math.max(0, Math.min(MAX_DEPTH, num(depth) || 0));
    var step = Math.min(MAX_STEP_MS, Math.max(MIN_STEP_MS, target / (d + 1)));
    return Math.round(step / speed);
  }

  /**
   * Model → { stepMs, totalMs, events[] } sorted by `at` (ms from start).
   * Kinds: origin, ring {depth}, relay {index, depth}, observer {index, depth},
   * summary. Reduced motion puts every event at 0 (one frame).
   */
  function buildFollowSchedule(model, opts) {
    opts = opts || {};
    var D = model.depth || 0;
    var step = opts.reducedMotion ? 0 : stepMsFor(D, opts);
    var events = [];
    events.push({ at: 0, kind: 'origin' });
    for (var d = 0; d <= D; d++) events.push({ at: d * step, kind: 'ring', depth: d });
    for (var i = 0; i < model.relays.length; i++) {
      var r = model.relays[i];
      events.push({ at: r.depth * step, kind: 'relay', index: i, depth: r.depth });
    }
    // model.observers is already in heard order (offset, then id), so a
    // group keeps that order inside its step.
    var groups = [];
    for (var j = 0; j < model.observers.length; j++) {
      var o = model.observers[j];
      var od = o.late ? D : Math.min(o.hops, D);
      (groups[od] = groups[od] || []).push(j);
    }
    for (var g = 0; g <= D; g++) {
      var grp = groups[g];
      if (!grp) continue;
      var slot = (STAGGER_FRAC * step) / grp.length;
      for (var n = 0; n < grp.length; n++) {
        events.push({ at: g * step + Math.round(n * slot), kind: 'observer', index: grp[n], depth: g });
      }
    }
    var totalMs = (D + 1) * step;
    events.push({ at: totalMs, kind: 'summary' });
    for (var s = 0; s < events.length; s++) events[s].seq = s;
    events.sort(function (a, b) {
      return (a.at - b.at) || (KIND_ORDER[a.kind] - KIND_ORDER[b.kind]) || (a.seq - b.seq);
    });
    return { stepMs: step, totalMs: totalMs, events: events };
  }

  function formatOffset(o) {
    if (!o || o.deltaSec == null) return 'time unknown';
    if (o.late) return 'late (+' + o.deltaSec + ' s)';
    if (o.deltaSec === 0) return 'first second';
    return '+' + o.deltaSec + ' s';
  }

  function summarizeFollow(model, haversineKm) {
    var longest = null;
    var unpositioned = 0;
    for (var i = 0; i < model.segments.length; i++) {
      var sg = model.segments[i];
      if (!sg.from.pos || !sg.to.pos) { unpositioned++; continue; }
      if (typeof haversineKm !== 'function') continue;
      var km = haversineKm(sg.from.pos[0], sg.from.pos[1], sg.to.pos[0], sg.to.pos[1]);
      if (!longest || km > longest.km) longest = { km: km, from: sg.from.name, to: sg.to.name };
    }
    var regions = Object.create(null);
    var flagged = 0, late = 0, positioned = 0;
    for (var j = 0; j < model.observers.length; j++) {
      var o = model.observers[j];
      if (o.iata) regions[String(o.iata).toUpperCase()] = true;
      if (o.clock) flagged++;
      if (o.late) late++;
      if (o.pos) positioned++;
    }
    return {
      typeName: model.typeName,
      hash: model.hash,
      replayable: model.replayable,
      hopsMax: model.replayable ? model.depth : null,
      longestKm: longest ? longest.km : null,
      longestFrom: longest ? longest.from : null,
      longestTo: longest ? longest.to : null,
      unpositionedHops: unpositioned,
      observers: model.observers.length,
      positionedObservers: positioned,
      flagged: flagged,
      late: late,
      firstSec: model.tMin,
      lastSec: model.tMax,
      spreadSec: model.tMin != null ? model.tMax - model.tMin : null,
      regions: Object.keys(regions).sort(),
      total: model.total,
      capped: model.capped,
    };
  }

  // Summary → [{label, value}] for the card (plain strings; live.js sets
  // them with textContent). fmtTime(epochSec) formats a clock time.
  function summaryRows(s, fmtTime) {
    var rows = [];
    rows.push({ label: 'Hops (max)', value: s.hopsMax == null ? '—' : String(s.hopsMax) });
    var longest = s.longestKm == null ? '—'
      : s.longestKm.toFixed(1) + ' km (' + s.longestFrom + ' → ' + s.longestTo + ')';
    if (s.unpositionedHops) longest += ' · ' + s.unpositionedHops + ' hop' + (s.unpositionedHops === 1 ? '' : 's') + ' without position';
    rows.push({ label: 'Longest single hop', value: longest });
    var obs = String(s.observers);
    if (s.flagged) obs += ' (' + s.flagged + ' with unreliable clock)';
    if (s.capped) obs += ' · first ' + MAX_OBSERVATIONS + ' of ' + s.total + ' observations';
    rows.push({ label: 'Observers', value: obs });
    rows.push({ label: 'First / last heard', value: s.firstSec == null ? '—'
      : fmtTime(s.firstSec) + ' / ' + fmtTime(s.lastSec) + ' (+' + s.spreadSec + ' s)' });
    rows.push({ label: 'Regions reached', value: s.regions.length ? s.regions.join(', ') : '—' });
    return rows;
  }

  // Wait before the one fetch of a follow started from a line or packet
  // first seen at tsMs: what is left of the propagation window (bufferMs),
  // so late observations reach the DB first. Older packets fetch at once.
  function settleMs(tsMs, bufferMs, nowMs) {
    var buf = Number(bufferMs) || 0;
    var age = Number(nowMs) - (Number(tsMs) || 0);
    if (!(buf > 0) || !isFinite(age)) return 0;
    return Math.max(0, Math.min(buf, buf - age));
  }

  window.LiveFollow = {
    HASH_PARAM: HASH_PARAM,
    MAX_OBSERVATIONS: MAX_OBSERVATIONS,
    MAX_DEPTH: MAX_DEPTH,
    TARGET_MS: TARGET_MS,
    LATE_SEC: LATE_SEC,
    CAPTION: CAPTION,
    parseFollowParam: parseFollowParam,
    settleMs: settleMs,
    toEpochSec: toEpochSec,
    buildObserverRoster: buildObserverRoster,
    buildSkewMap: buildSkewMap,
    isReplayable: isReplayable,
    reduceFollow: reduceFollow,
    stepMsFor: stepMsFor,
    buildFollowSchedule: buildFollowSchedule,
    formatOffset: formatOffset,
    summarizeFollow: summarizeFollow,
    summaryRows: summaryRows,
  };
})();
