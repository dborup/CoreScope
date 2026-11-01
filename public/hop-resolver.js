/**
 * Client-side hop resolver — eliminates /api/resolve-hops HTTP requests.
 * Mirrors the server's disambiguateHops() logic from server.js.
 */
window.HopResolver = (function() {
  'use strict';

  const MAX_HOP_DIST = 1.8; // ~200km in degrees
  const REGION_RADIUS_KM = 300;

  // Only repeaters and room servers can appear as path hops per protocol.
  // Companions/sensors originate but never relay packets.
  function canAppearInPath(role) {
    if (!role) return false;
    var r = String(role).toLowerCase();
    return r.indexOf('repeater') >= 0 || r.indexOf('room_server') >= 0 || r === 'room';
  }
  let prefixIdx = {};   // lowercase hex prefix → [node, ...]
  let pubkeyIdx = {};   // full lowercase pubkey → node (O(1) lookup)
  let nodesList = [];
  let observerIataMap = {}; // observer_id → iata
  let iataCoords = {};  // iata → {lat, lon}
  let affinityMap = {}; // pubkey → { neighborPubkey → score } — resolve() only
  // #598 r2 F6 — inferPath()'s own neighbour graph. The packet detail pane
  // loads a graph (packet-helpers.js ensureHopAffinity), and that used to be
  // the app's FIRST setAffinity() call: opening one packet detail switched
  // live.js's resolve() from geographic picks to graph-based ones for the rest
  // of the session. Separate maps keep each consumer's behaviour its own.
  let pathAffinityMap = {};

  function dist(lat1, lon1, lat2, lon2) {
    return Math.sqrt((lat1 - lat2) ** 2 + (lon1 - lon2) ** 2);
  }

  function haversineKm(lat1, lon1, lat2, lon2) {
    const R = 6371;
    const dLat = (lat2 - lat1) * Math.PI / 180;
    const dLon = (lon2 - lon1) * Math.PI / 180;
    const a = Math.sin(dLat / 2) ** 2 +
      Math.cos(lat1 * Math.PI / 180) * Math.cos(lat2 * Math.PI / 180) *
      Math.sin(dLon / 2) ** 2;
    return R * 2 * Math.atan2(Math.sqrt(a), Math.sqrt(1 - a));
  }

  /**
   * Initialize (or rebuild) the prefix index from the full nodes list.
   * @param {Array} nodes - Array of {public_key, name, lat, lon, ...}
   * @param {Object} [opts] - Optional: { observers: [{id, iata}], iataCoords: {code: {lat,lon}} }
   */
  function init(nodes, opts) {
    nodesList = nodes || [];
    prefixIdx = {};
    pubkeyIdx = {};
    for (const n of nodesList) {
      if (!n.public_key) continue;
      const pk = n.public_key.toLowerCase();
      // pubkeyIdx includes ALL nodes — used by resolveFromServer for
      // server-confirmed full-pubkey lookups (any node type).
      pubkeyIdx[pk] = n;
      // prefixIdx only includes nodes that can appear as path hops.
      if (!canAppearInPath(n.role)) continue;
      for (let len = 1; len <= 3; len++) {
        const p = pk.slice(0, len * 2);
        if (!prefixIdx[p]) prefixIdx[p] = [];
        prefixIdx[p].push(n);
      }
    }
    // Store observer IATA mapping and coords if provided
    observerIataMap = {};
    if (opts && opts.observers) {
      for (const o of opts.observers) {
        if (o.id && o.iata) observerIataMap[o.id] = o.iata;
      }
    }
    iataCoords = (opts && opts.iataCoords) || (window.IATA_COORDS_GEO) || {};
  }

  /**
   * Check if a node is near an IATA region center.
   * Returns { near, method, distKm } or null.
   */
  function nodeInRegion(candidate, iata) {
    const center = iataCoords[iata];
    if (!center) return null;
    if (candidate.lat && candidate.lon && !(candidate.lat === 0 && candidate.lon === 0)) {
      const d = haversineKm(candidate.lat, candidate.lon, center.lat, center.lon);
      return { near: d <= REGION_RADIUS_KM, method: 'geo', distKm: Math.round(d) };
    }
    return null; // no GPS — can't geo-filter client-side
  }

  /**
   * Pick the best candidate by scoring against BOTH prev and next resolved hops.
   *
   * Strategy (in priority order):
   * 1. Neighbor-graph edge weight: sum of edge scores to prevPubkey + nextPubkey. Pick max.
   * 2. Geographic centroid: if no candidate has graph edges, compute centroid of
   *    prev+next positions and pick closest candidate by haversine distance.
   * 3. Single-anchor geo fallback: if only one neighbor is resolved, use it as anchor.
   * 4. Original heuristic: first candidate (when no context at all).
   *
   * @param {Array} candidates - candidates with lat/lon/pubkey/name
   * @param {string|null} prevPubkey - pubkey of previous resolved hop
   * @param {string|null} nextPubkey - pubkey of next resolved hop
   * @param {Object|null} prevPos - {lat, lon} of previous resolved hop or origin
   * @param {Object|null} nextPos - {lat, lon} of next resolved hop or observer
   * @returns {Object} best candidate
   */
  function pickByAffinity(candidates, prevPubkey, nextPubkey, prevPos, nextPos) {
    const hasGraph = Object.keys(affinityMap).length > 0;
    const hasAdj = prevPubkey || nextPubkey;

    // Strategy 1: neighbor-graph edge weights (sum of prev + next)
    if (hasGraph && hasAdj) {
      const scored = candidates.map(function(c) {
        let s = 0;
        if (prevPubkey) s += getAffinity(prevPubkey, c.pubkey);
        if (nextPubkey) s += getAffinity(nextPubkey, c.pubkey);
        return { candidate: c, edgeScore: s };
      });
      const withEdges = scored.filter(function(s) { return s.edgeScore > 0; });
      if (withEdges.length > 0) {
        withEdges.sort(function(a, b) { return b.edgeScore - a.edgeScore; });
        _traceMultiCandidate(candidates, scored, withEdges[0].candidate, 'graph');
        return withEdges[0].candidate;
      }
    }

    // Strategy 2/3: geographic — centroid of prev+next, or single anchor
    let anchorLat = null, anchorLon = null, anchorCount = 0;
    if (prevPos && prevPos.lat != null && prevPos.lon != null) {
      anchorLat = (anchorLat || 0) + prevPos.lat;
      anchorLon = (anchorLon || 0) + prevPos.lon;
      anchorCount++;
    }
    if (nextPos && nextPos.lat != null && nextPos.lon != null) {
      anchorLat = (anchorLat || 0) + nextPos.lat;
      anchorLon = (anchorLon || 0) + nextPos.lon;
      anchorCount++;
    }
    if (anchorCount > 0) {
      anchorLat /= anchorCount;
      anchorLon /= anchorCount;
      const geoScored = candidates.map(function(c) {
        const d = (c.lat != null && c.lon != null && !(c.lat === 0 && c.lon === 0))
          ? haversineKm(c.lat, c.lon, anchorLat, anchorLon) : 999999;
        return { candidate: c, distKm: d };
      });
      geoScored.sort(function(a, b) { return a.distKm - b.distKm; });
      _traceMultiCandidate(candidates, geoScored, geoScored[0].candidate, 'centroid');
      return geoScored[0].candidate;
    }

    // Strategy 4: no context — return first candidate
    _traceMultiCandidate(candidates, null, candidates[0], 'fallback');
    return candidates[0];
  }

  /** Dev-mode console trace for multi-candidate picks */
  function _traceMultiCandidate(candidates, scored, chosen, method) {
    if (typeof console === 'undefined' || !console.debug) return;
    if (candidates.length < 2) return;
    try {
      const prefix = candidates[0].pubkey ? candidates[0].pubkey.slice(0, 2) : '??';
      const scoreSummary = scored ? scored.map(function(s) {
        const pk = (s.candidate || s).pubkey || '?';
        const val = s.edgeScore != null ? s.edgeScore : (s.distKm != null ? s.distKm + 'km' : '?');
        return pk.slice(0, 8) + ':' + val;
      }) : [];
      console.debug('[hop-resolver] hash=' + prefix + ' candidates=' + candidates.length +
        ' scored=[' + scoreSummary.join(',') + '] chose=' + (chosen.pubkey || '?').slice(0, 8) +
        ' method=' + method);
    } catch(e) { /* trace is best-effort */ }
  }

  /**
   * Resolve an array of hex hop prefixes to node info.
   * Returns a map: { hop: {name, pubkey, lat, lon, ambiguous, unreliable} }
   *
   * @param {string[]} hops - Hex prefixes
   * @param {number|null} originLat - Sender latitude (forward anchor)
   * @param {number|null} originLon - Sender longitude (forward anchor)
   * @param {number|null} observerLat - Observer latitude (backward anchor)
   * @param {number|null} observerLon - Observer longitude (backward anchor)
   * @returns {Object} resolved map keyed by hop prefix
   */
  function resolve(hops, originLat, originLon, observerLat, observerLon, observerId) {
    if (!hops || !hops.length) return {};

    // Determine observer's IATA for regional filtering
    const packetIata = observerId ? observerIataMap[observerId] : null;
    // This map cannot represent different answers for repeated positions.
    // Do not narrow a terminal prefix that also occurs earlier in the path.
    const earlierPrefixes = packetIata ? new Set(hops.slice(0, -1).map(h => h.toLowerCase())) : null;
    const terminalHasRegion = packetIata && !earlierPrefixes.has(hops[hops.length - 1].toLowerCase());

    const resolved = {};
    const hopPositions = {};

    // A flood's recorded path ends with the transmitter heard by its
    // observer. Earlier hops may be remote: the receiving region is not
    // evidence for them. Callers supply observer hints only for RX floods.
    for (let hopIndex = 0; hopIndex < hops.length; hopIndex++) {
      const hop = hops[hopIndex];
      const hopIata = hopIndex === hops.length - 1 && terminalHasRegion ? packetIata : null;
      const h = hop.toLowerCase();
      const allCandidates = prefixIdx[h] || [];
      if (allCandidates.length === 0) {
        resolved[hop] = { name: null, candidates: [], conflicts: [] };
      } else if (allCandidates.length === 1) {
        const c = allCandidates[0];
        const regionCheck = hopIata ? nodeInRegion(c, hopIata) : null;
        resolved[hop] = { name: c.name, pubkey: c.public_key,
          candidates: [{ name: c.name, pubkey: c.public_key, lat: c.lat, lon: c.lon, regional: regionCheck ? regionCheck.near : false, filterMethod: regionCheck ? regionCheck.method : 'none', distKm: regionCheck ? regionCheck.distKm : undefined }],
          conflicts: [] };
      } else {
        // Multiple candidates — apply geo regional filtering
        const checked = allCandidates.map(c => {
          const r = hopIata ? nodeInRegion(c, hopIata) : null;
          return { ...c, regional: r ? r.near : false, filterMethod: r ? r.method : 'none', distKm: r ? r.distKm : undefined };
        });
        const regional = checked.filter(c => c.regional);
        regional.sort((a, b) => (a.distKm || 9999) - (b.distKm || 9999));
        const candidates = regional.length > 0 ? regional : checked;
        const globalFallback = regional.length === 0 && checked.length > 0;

        const conflicts = candidates.map(c => ({
          name: c.name, pubkey: c.public_key, lat: c.lat, lon: c.lon,
          regional: c.regional, filterMethod: c.filterMethod, distKm: c.distKm
        }));

        if (candidates.length === 1) {
          resolved[hop] = { name: candidates[0].name, pubkey: candidates[0].public_key,
            candidates: conflicts, conflicts, globalFallback };
        } else {
          resolved[hop] = { name: candidates[0].name, pubkey: candidates[0].public_key,
            ambiguous: true, candidates: conflicts, conflicts, globalFallback,
            hopBytes: Math.ceil(hop.length / 2), totalGlobal: allCandidates.length, totalRegional: regional.length };
        }
      }
    }

    // Build initial positions for unambiguous hops
    for (const hop of hops) {
      const r = resolved[hop];
      if (r && !r.ambiguous && r.pubkey) {
        const node = nodesList.find(n => n.public_key === r.pubkey);
        if (node && node.lat && node.lon && !(node.lat === 0 && node.lon === 0)) {
          hopPositions[hop] = { lat: node.lat, lon: node.lon };
        }
      }
    }

    // Combined disambiguation: resolve ambiguous hops using both neighbors.
    // We iterate until no more hops can be resolved (handles cascading dependencies).
    const originPos = (originLat != null && originLon != null) ? { lat: originLat, lon: originLon } : null;
    const observerPos = (observerLat != null && observerLon != null) ? { lat: observerLat, lon: observerLon } : null;

    let changed = true;
    let maxIter = hops.length + 1; // prevent infinite loops
    while (changed && maxIter-- > 0) {
      changed = false;
      for (let i = 0; i < hops.length; i++) {
        const hop = hops[i];
        if (hopPositions[hop]) continue; // already resolved
        const r = resolved[hop];
        if (!r || !r.ambiguous) continue;
        const withLoc = r.candidates.filter(c => c.lat != null && c.lon != null && !(c.lat === 0 && c.lon === 0));
        if (!withLoc.length) continue;

        // Find prev resolved neighbor
        let prevPubkey = null, prevPos = null;
        for (let j = i - 1; j >= 0; j--) {
          if (hopPositions[hops[j]]) {
            prevPos = hopPositions[hops[j]];
            prevPubkey = resolved[hops[j]] ? resolved[hops[j]].pubkey : null;
            break;
          }
        }
        if (!prevPos && originPos) prevPos = originPos;

        // Find next resolved neighbor
        let nextPubkey = null, nextPos = null;
        for (let j = i + 1; j < hops.length; j++) {
          if (hopPositions[hops[j]]) {
            nextPos = hopPositions[hops[j]];
            nextPubkey = resolved[hops[j]] ? resolved[hops[j]].pubkey : null;
            break;
          }
        }
        if (!nextPos && observerPos) nextPos = observerPos;

        // Skip if we have zero context (wait for a later iteration or neighbor resolution)
        if (!prevPubkey && !nextPubkey && !prevPos && !nextPos) continue;

        const picked = pickByAffinity(withLoc, prevPubkey, nextPubkey, prevPos, nextPos);
        r.name = picked.name;
        r.pubkey = picked.pubkey;
        hopPositions[hop] = { lat: picked.lat, lon: picked.lon };
        changed = true;
      }
    }

    // Sanity check: drop hops impossibly far from neighbors
    for (let i = 0; i < hops.length; i++) {
      const pos = hopPositions[hops[i]];
      if (!pos) continue;
      const prev = i > 0 ? hopPositions[hops[i - 1]] : null;
      const next = i < hops.length - 1 ? hopPositions[hops[i + 1]] : null;
      if (!prev && !next) continue;
      const dPrev = prev ? dist(pos.lat, pos.lon, prev.lat, prev.lon) : 0;
      const dNext = next ? dist(pos.lat, pos.lon, next.lat, next.lon) : 0;
      const tooFarPrev = prev && dPrev > MAX_HOP_DIST;
      const tooFarNext = next && dNext > MAX_HOP_DIST;
      if ((tooFarPrev && tooFarNext) || (tooFarPrev && !next) || (tooFarNext && !prev)) {
        const r = resolved[hops[i]];
        if (r) r.unreliable = true;
        delete hopPositions[hops[i]];
      }
    }

    return resolved;
  }

  // ── Whole-path inference for one observation (#598, port of upstream 2245) ──
  //
  // resolve() picks each hop on its own and keys its answer by prefix, so a
  // prefix seen twice in one path gets one node, and nothing checks that the
  // chosen hops form a path a packet could have taken. packets.js makes that
  // worse: resolveHops() calls resolve() one prefix at a time with no anchors
  // at all, so an ambiguous hop falls to the first candidate in index order.
  //
  // inferPath() scores the path as a whole instead, using two firmware facts
  // (Mesh::routeRecvPacket, wasSeen/markSeen): a relayer appends its own hash,
  // so the last hop is the one the observer heard directly; and a node
  // forwards a given packet once, so it appears at most once in a flood path.
  //
  // Each hop is either one of the located repeaters/room servers sharing its
  // prefix, or "unlisted": a node missing from the list, or one without GPS.
  // With 1-byte prefixes the second is common, and naming it beats drawing a
  // 300 km hop. The cheapest sequence is found by dynamic programming over
  // (hop, node), and each hop's confidence comes from comparing the best path
  // through each of its alternatives.
  //
  // Display only: this never rewrites a stored resolved_path. The ingestor
  // stays authoritative (#1283); a non-null resolved_path entry is a hint.
  //
  // TODO(customizer): expose these once the detail view settles (AGENTS rule 8).
  const PATH_MODEL = {
    typicalHopKm: 50,  // a hop this long costs 1
    maxHopKm: 100,     // per hop; past it the cost climbs 4x as fast
    unlistedCost: 1.6, // a hop whose node is not located
    linkFactor: 0.5,   // a neighbour-graph link makes a hop this much cheaper
    temperature: 0.7,  // turns cost differences into confidence
    regionSlackKm: 50, // an IATA centre stands in for the observer, loosely
  };

  // An inferred hop at or above this confidence is presented as the answer
  // rather than a guess. hop-display.js renders the distinction.
  const LIKELY_CONFIDENCE = 0.7;

  function isLocated(n) {
    return n != null && n.lat != null && n.lon != null &&
      Number.isFinite(Number(n.lat)) && Number.isFinite(Number(n.lon)) &&
      !(Number(n.lat) === 0 && Number(n.lon) === 0);
  }

  // A point on the path: a candidate node or an anchor, with its radians
  // cached for the cost function.
  function pathPoint(lat, lon, node, slackKm) {
    const rlat = Number(lat) * Math.PI / 180;
    return { rlat: rlat, rlon: Number(lon) * Math.PI / 180, cos: Math.cos(rlat),
      node: node || null, pubkey: node ? node.public_key : null, slackKm: slackKm || 0 };
  }

  // Equirectangular distance — within a mesh's extent it agrees with
  // haversine to well under a percent, at a fraction of the cost.
  function pointKm(a, b) {
    const x = (b.rlon - a.rlon) * (a.cos + b.cos) / 2;
    const y = b.rlat - a.rlat;
    return 6371 * Math.sqrt(x * x + y * y);
  }

  // Hops further apart than this are not compared directly. Past it the
  // distance limit (MAX_GAP x maxHopKm) spans any regional mesh, so a longer
  // run of unlisted hops costs a flat longBase() instead, which a running
  // minimum handles. This keeps the work at O(hops x MAX_GAP x cands^2).
  const MAX_GAP = 8;

  // The flat part of a span longer than MAX_GAP between two located points:
  // span(p -> q) = longBase() + (q - p - 1) x unlistedCost.
  function longBase() {
    return PATH_MODEL.maxHopKm / PATH_MODEL.typicalHopKm;
  }

  // Cost of the stretch from a (at one position) to b (gap positions later):
  // the unlisted hops between them, plus the distance when both are located.
  function spanCost(a, b, gap) {
    const c = (gap - 1) * PATH_MODEL.unlistedCost;
    if (!a || !b) return c;
    if (gap > MAX_GAP) return c + longBase();
    const d = Math.max(0, pointKm(a, b) - a.slackKm - b.slackKm);
    const lim = gap * PATH_MODEL.maxHopKm;
    let geo = d <= lim ? d / PATH_MODEL.typicalHopKm
      : (lim + 4 * (d - lim)) / PATH_MODEL.typicalHopKm;
    if (gap === 1 && a.pubkey && b.pubkey && getPathAffinity(a.pubkey, b.pubkey) > 0) geo *= PATH_MODEL.linkFactor;
    return c + geo;
  }

  // steps[p][g][j * |C[p+g]| + l] = cost from candidate j at hop p to
  // candidate l at hop p+g. Computed once, shared by every solve and by the
  // confidence pass.
  //
  // #598 r2 — this used to force Infinity for the same node at both ends,
  // which only covered pairs inside the comparison window and so could not be
  // the mechanism that keeps a node to one position. It also masked single-
  // point mutants on the ban loop, which IS that mechanism. One place now
  // enforces the rule: dedupe() below, which runs to convergence.
  function buildSteps(C) {
    const L = C.length;
    const steps = [];
    for (let p = 0; p < L; p++) {
      steps.push([null]);
      for (let g = 1; g <= MAX_GAP && p + g < L; g++) {
        const A = C[p], Bq = C[p + g], row = new Float64Array(A.length * Bq.length);
        for (let j = 0; j < A.length; j++) {
          for (let l = 0; l < Bq.length; l++) {
            row[j * Bq.length + l] = spanCost(A[j], Bq[l], g);
          }
        }
        steps[p].push(row);
      }
    }
    return steps;
  }

  // Best path cost forward (F) and backward (B) through every (hop, candidate).
  // bans[i] holds pubkeys excluded at hop i, to keep a node to one position.
  function solvePath(C, steps, start, end, bans) {
    const L = C.length, U = PATH_MODEL.unlistedCost, LONG = longBase();
    const F = C.map(c => new Float64Array(c.length).fill(Infinity));
    const B = C.map(c => new Float64Array(c.length).fill(Infinity));
    const backP = C.map(c => new Int32Array(c.length).fill(-1));
    const backK = C.map(c => new Int32Array(c.length).fill(-1));
    const isBanned = (i, k) => bans[i] != null && bans[i].has(C[i][k].pubkey);

    // Forward. longF tracks min(F[p][j] - p*U) over hops more than MAX_GAP back.
    let longF = Infinity, longP = -1, longK = -1;
    for (let i = 0; i < L; i++) {
      const old = i - MAX_GAP - 1;
      if (old >= 0) {
        for (let j = 0; j < C[old].length; j++) {
          const v = F[old][j] - old * U;
          if (v < longF) { longF = v; longP = old; longK = j; }
        }
      }
      const K = C[i].length;
      for (let k = 0; k < K; k++) {
        if (isBanned(i, k)) continue;
        let best = spanCost(start, C[i][k], i + 1), bp = -1, bk = -1;
        for (let g = 1; g <= MAX_GAP && g <= i; g++) {
          const p = i - g, Fp = F[p], row = steps[p][g];
          for (let j = 0; j < Fp.length; j++) {
            const v = Fp[j] + row[j * K + k];
            if (v < best) { best = v; bp = p; bk = j; }
          }
        }
        const vLong = longF + (i - 1) * U + LONG;
        if (vLong < best) { best = vLong; bp = longP; bk = longK; }
        F[i][k] = best; backP[i][k] = bp; backK[i][k] = bk;
      }
    }

    // Backward. longB tracks min(B[q][l] + q*U) over hops more than MAX_GAP on.
    let longB = Infinity;
    for (let i = L - 1; i >= 0; i--) {
      const old = i + MAX_GAP + 1;
      if (old < L) {
        for (let l = 0; l < C[old].length; l++) longB = Math.min(longB, B[old][l] + old * U);
      }
      for (let k = 0; k < C[i].length; k++) {
        if (isBanned(i, k)) continue;
        let best = Math.min(spanCost(C[i][k], end, L - i), longB - (i + 1) * U + LONG);
        for (let g = 1; g <= MAX_GAP && i + g < L; g++) {
          const Bq = B[i + g], row = steps[i][g], base = k * Bq.length;
          for (let l = 0; l < Bq.length; l++) {
            const v = Bq[l] + row[base + l];
            if (v < best) best = v;
          }
        }
        B[i][k] = best;
      }
    }

    let total = spanCost(start, end, L + 1), lastP = -1, lastK = -1;
    for (let i = 0; i < L; i++) {
      for (let k = 0; k < C[i].length; k++) {
        const v = F[i][k] + spanCost(C[i][k], end, L - i);
        if (v < total) { total = v; lastP = i; lastK = k; }
      }
    }
    const assign = new Int32Array(L).fill(-1);
    for (let p = lastP, k = lastK; p >= 0;) {
      assign[p] = k;
      const np = backP[p][k], nk = backK[p][k];
      p = np; k = nk;
    }
    return { F: F, B: B, total: total, assign: assign };
  }

  // Lowest total cost of any path leaving hop i unlisted: some span from a
  // point before i (or the start) to a point after it (or the end), with
  // every hop between unlisted.
  function unlistedMarginals(C, steps, start, end, F, B) {
    const L = C.length, U = PATH_MODEL.unlistedCost, LONG = longBase();
    const mu = new Float64Array(L).fill(Infinity);
    const cover = (p, q, v) => { for (let i = p + 1; i < q; i++) if (v < mu[i]) mu[i] = v; };
    const minOf = arr => arr.reduce((m, v) => Math.min(m, v), Infinity);

    for (let p = -1; p < L - 1; p++) {
      for (let g = 2; g <= MAX_GAP && p + g <= L; g++) {
        const q = p + g;
        let best = Infinity;
        if (p < 0 && q >= L) {
          best = spanCost(start, end, g);
        } else if (p < 0) {
          for (let l = 0; l < C[q].length; l++) best = Math.min(best, spanCost(start, C[q][l], g) + B[q][l]);
        } else if (q >= L) {
          for (let j = 0; j < C[p].length; j++) best = Math.min(best, F[p][j] + spanCost(C[p][j], end, g));
        } else {
          const row = steps[p][g], K = C[q].length;
          for (let j = 0; j < C[p].length; j++) {
            for (let l = 0; l < K; l++) best = Math.min(best, F[p][j] + row[j * K + l] + B[q][l]);
          }
        }
        cover(p, q, best);
      }
    }
    // Spans longer than MAX_GAP cost the same whichever nodes bound them, as
    // in spanCost(): the flat part only applies between two located points.
    const Fmin = F.map(minOf), Bmin = B.map(minOf);
    for (let p = -1; p < L; p++) {
      for (let q = p + MAX_GAP + 1; q <= L; q++) {
        const flat = (p < 0 && !start) || (q >= L && !end) ? 0 : LONG;
        cover(p, q, (p < 0 ? 0 : Fmin[p]) + (q - p - 1) * U + flat + (q >= L ? 0 : Bmin[q]));
      }
    }
    return mu;
  }

  /**
   * Infer the most likely node behind every hop of ONE observation's path.
   *
   * @param {string[]} hops - hex prefixes, in path order (last = heard by observer)
   * @param {Object} [opts]
   * @param {{lat,lon}} [opts.observer] - the observer's position
   * @param {string} [opts.observerId] - falls back to its IATA centre when the
   *   observer reports no position (or 0,0)
   * @param {{lat,lon}} [opts.origin] - the sender's position, when known
   * @param {Array<string|null>} [opts.resolvedPath] - server-resolved pubkeys
   *   aligned with hops; a non-null entry restricts that hop to that node
   * @returns {Array} one entry per hop: { hop, name, pubkey, lat, lon,
   *   located, unlisted, unreliable, ambiguous, confidence, candidates,
   *   conflicts } — candidates sorted by confidence, located first; one the
   *   path uses at another hop carries usedAt (that hop's index)
   */
  function inferPath(hops, opts) {
    if (!hops || !hops.length) return [];
    opts = opts || {};
    const L = hops.length;
    const rp = Array.isArray(opts.resolvedPath) && opts.resolvedPath.length === L ? opts.resolvedPath : null;

    let end = null;
    if (isLocated(opts.observer)) {
      end = pathPoint(opts.observer.lat, opts.observer.lon);
    } else if (opts.observerId) {
      const center = iataCoords[observerIataMap[opts.observerId]];
      if (isLocated(center)) end = pathPoint(center.lat, center.lon, null, PATH_MODEL.regionSlackKm);
    }
    const start = isLocated(opts.origin) ? pathPoint(opts.origin.lat, opts.origin.lon) : null;

    const C = [], unplaced = [], hints = [];
    for (let i = 0; i < L; i++) {
      const hinted = rp && rp[i] ? pubkeyIdx[String(rp[i]).toLowerCase()] : null;
      hints.push(hinted || null);
      const nodes = hinted ? [hinted] : (prefixIdx[String(hops[i]).toLowerCase()] || []);
      C.push(nodes.filter(isLocated).map(n => pathPoint(n.lat, n.lon, n)));
      unplaced.push(nodes.filter(n => !isLocated(n)));
    }

    // #598 r2 F3 — the ingestor's answer belongs to the position it was given
    // for. Ban every hinted pubkey everywhere else BEFORE solving, so an
    // unresolved hop cannot take a node another hop has been told it is: that
    // showed the node twice and demoted the authoritative answer to
    // "unreliable". Kept separate from the dedup bans below, because a node the
    // ingestor placed elsewhere is no alternative for this hop at all, while a
    // node the model itself placed elsewhere is one it weighed and rejected
    // (and is reported with `usedAt`).
    const hintBans = C.map(() => null);
    hints.forEach((n, i) => {
      if (!n) return;
      for (let j = 0; j < L; j++) {
        if (j === i) continue;
        if (!hintBans[j]) hintBans[j] = new Set();
        hintBans[j].add(n.public_key);
      }
    });
    const bans = hintBans.map(b => (b ? new Set(b) : null));
    const addBan = (i, pubkey) => {
      if (!bans[i]) bans[i] = new Set();
      bans[i].add(pubkey);
    };

    const steps = buildSteps(C);

    // Keep each node to one position (a node forwards a given packet once —
    // see the firmware note above). The solver has no such constraint, so the
    // duplicates it produces are removed here, and the loop runs to
    // CONVERGENCE: stopping after a fixed number of rounds left duplicates in
    // the output (#598 r2 F4 measured 7 of them on a 63-hop path with 8
    // distinct prefixes). This is the ONE place the rule is enforced.
    //
    // Each round bans every repeated node at all but its best-fitting
    // position, judged by that position's min-marginal cost F+B from the solve
    // we already have, and then solves once more. Bans only ever accumulate,
    // and each round adds at least one, so the rounds are bounded by the total
    // number of candidate placements; past that every candidate is banned
    // everywhere and the assignment is empty. Handling all duplicates per
    // round rather than one keeps a pathological path affordable: 63 hops that
    // all share one prefix went from 377 ms to 19 ms.
    function duplicates(assign) {
      const at = new Map();
      for (let i = 0; i < L; i++) {
        if (assign[i] < 0) continue;
        const key = C[i][assign[i]].pubkey;
        if (!at.has(key)) at.set(key, []);
        at.get(key).push(i);
      }
      const out = [];
      for (const [key, positions] of at) if (positions.length > 1) out.push([key, positions]);
      return out;
    }
    const placements = C.reduce((n, c) => n + c.length, 0);
    let sol = solvePath(C, steps, start, end, bans);
    for (let round = 0; round <= placements; round++) {
      const dups = duplicates(sol.assign);
      if (!dups.length) break;
      for (const [key, positions] of dups) {
        // Keep it where it fits best; give it up everywhere else. A duplicated
        // node is at both ends of the SAME path, so its min-marginal is often
        // identical at every position it occupies — `<=` then keeps the LATEST
        // one, which is the firmware-grounded tie-break: the last hop of a
        // flood is the relay the observer heard directly
        // (firmware/src/Mesh.cpp:344-350), so the later position is the one
        // backed by evidence. Picking the earliest put a node that sits on the
        // observer at hop 0 and left the hop it actually relayed unlisted.
        let best = positions[0];
        let bestCost = sol.F[best][sol.assign[best]] + sol.B[best][sol.assign[best]];
        for (const i of positions.slice(1)) {
          const c = sol.F[i][sol.assign[i]] + sol.B[i][sol.assign[i]];
          if (c <= bestCost) { bestCost = c; best = i; }
        }
        for (const i of positions) if (i !== best) addBan(i, key);
      }
      sol = solvePath(C, steps, start, end, bans);
    }
    // This assignment is the answer. Nothing below may change it.
    const assign = sol.assign;

    // For confidence only: a node the path uses at one hop is no alternative at
    // another, so its cost there must not count as a rival. This re-solve
    // supplies the forward/backward marginals the softmax needs; the
    // assignment above stands, which is what keeps the invariant intact.
    const usedAt = new Map();
    assign.forEach((k, i) => { if (k >= 0) usedAt.set(C[i][k].pubkey, i); });
    const confidenceBans = C.map((c, i) => {
      const s = new Set(bans[i] || []);
      for (const pt of c) {
        const j = usedAt.get(pt.pubkey);
        if (j !== undefined && j !== i) s.add(pt.pubkey);
      }
      return s.size ? s : null;
    });
    const marginals = solvePath(C, steps, start, end, confidenceBans);
    const mu = unlistedMarginals(C, steps, start, end, marginals.F, marginals.B);
    const T = PATH_MODEL.temperature;
    // A node the ingestor placed at another hop is not an alternative here, so
    // it is not listed and does not make this hop ambiguous.
    const offered = C.map((c, i) => (hintBans[i] ? c.filter(pt => !hintBans[i].has(pt.pubkey)) : c));
    return hops.map((hop, i) => {
      const costs = Array.from(C[i], (_, k) => marginals.F[i][k] + marginals.B[i][k]);
      const lowest = Math.min(mu[i], ...costs);
      const weights = costs.map(c => c === Infinity ? 0 : Math.exp(-(c - lowest) / T));
      const wUnlisted = mu[i] === Infinity ? 0 : Math.exp(-(mu[i] - lowest) / T);
      const sum = weights.reduce((s, w) => s + w, wUnlisted) || 1;

      const candidates = C[i].map((pt, k) => {
        const c = { name: pt.node.name, pubkey: pt.pubkey, lat: pt.node.lat, lon: pt.node.lon,
          located: true, confidence: weights[k] / sum };
        const j = usedAt.get(pt.pubkey);
        if (j !== undefined && j !== i) c.usedAt = j; // the path has it at hop j
        return c;
      }).filter(c => !(hintBans[i] && hintBans[i].has(c.pubkey)))
        .sort((a, b) => b.confidence - a.confidence);
      for (const n of unplaced[i]) {
        candidates.push({ name: n.name, pubkey: n.public_key, lat: null, lon: null, located: false, confidence: null });
      }

      const k = assign[i];
      const entry = {
        hop: hop, name: null, pubkey: null, lat: null, lon: null,
        located: false, unlisted: k < 0, unreliable: false,
        ambiguous: offered[i].length > 1,
        confidence: k < 0 ? wUnlisted / sum : weights[k] / sum,
        candidates: candidates, conflicts: candidates,
      };
      if (k >= 0) {
        const n = C[i][k].node;
        entry.name = n.name; entry.pubkey = n.public_key;
        entry.lat = n.lat; entry.lon = n.lon; entry.located = true;
        if (hints[i]) {
          // The ingestor resolved this hop and stays authoritative (#1283):
          // the geometry agrees, so it is the answer, not a guess with odds.
          // `canonical` keeps it off the likelihood wording in hop-display.
          entry.canonical = true;
          entry.confidence = 1;
          entry.ambiguous = false;
        }
      } else if (hints[i]) {
        // The ingestor resolved this hop, so its answer is still what the hop
        // is called. Upstream drops the name entirely; this fork keeps it,
        // because the ingestor stays authoritative (#1283).
        entry.name = hints[i].name;
        entry.pubkey = hints[i].public_key;
        // #598 r2 F2 — flag it only when it is a LOCATED node that does not
        // fit where the hop sits, which is what #165's warning means ("a hash
        // to name match that is geographically inconsistent with the path
        // around it"). A node with no coordinates was never checked against
        // anything, so it reads "(no GPS)" instead.
        entry.unreliable = isLocated(hints[i]);
      } else if (offered[i].length) {
        // Located nodes share this prefix, but none fits where the hop is.
        entry.unreliable = true;
      } else if (unplaced[i].length === 1) {
        // The one known node with this prefix has no position to check.
        entry.name = unplaced[i][0].name;
        entry.pubkey = unplaced[i][0].public_key;
      }
      return entry;
    });
  }

  /**
   * Check if the resolver has been initialized with nodes.
   */
  function ready() {
    return nodesList.length > 0;
  }

  /**
   * Load neighbor-graph affinity data.
   * @param {Object} graph - { edges: [{source, target, score, weight}, ...] }
   */
  function setAffinity(graph) {
    affinityMap = buildAffinity(graph);
  }

  /**
   * Load neighbor-graph affinity data for inferPath() ONLY (#598 r2 F6).
   * Kept apart from setAffinity() so the packet detail pane cannot change what
   * resolve() answers for the packets list, the live view or node pages.
   */
  function setPathAffinity(graph) {
    pathAffinityMap = buildAffinity(graph);
  }

  function buildAffinity(graph) {
    const map = {};
    if (!graph || !graph.edges) return map;
    for (const e of graph.edges) {
      const w = e.score || e.weight || 1;
      if (!map[e.source]) map[e.source] = {};
      map[e.source][e.target] = w;
      if (!map[e.target]) map[e.target] = {};
      map[e.target][e.source] = w;
    }
    return map;
  }

  /**
   * Get the affinity score between two pubkeys (0 if not neighbors).
   */
  function getAffinity(pubkeyA, pubkeyB) {
    if (!pubkeyA || !pubkeyB || !affinityMap[pubkeyA]) return 0;
    return affinityMap[pubkeyA][pubkeyB] || 0;
  }

  /** The inferPath() graph's score between two pubkeys (0 if not neighbors). */
  function getPathAffinity(pubkeyA, pubkeyB) {
    if (!pubkeyA || !pubkeyB || !pathAffinityMap[pubkeyA]) return 0;
    return pathAffinityMap[pubkeyA][pubkeyB] || 0;
  }

  /**
   * Resolve hops using server-provided resolved_path (full pubkeys).
   * Returns the same format as resolve() — { [hop]: { name, pubkey, ... } }.
   * resolved_path is an array aligned with path_json: each element is a
   * 64-char lowercase hex pubkey or null. Skips entries that are null.
   */
  function resolveFromServer(hops, resolvedPath) {
    if (!hops || !resolvedPath || hops.length !== resolvedPath.length) return {};
    const result = {};
    for (let i = 0; i < hops.length; i++) {
      const hop = hops[i];
      const entry = serverHopEntry(resolvedPath[i]);
      if (entry) result[hop] = entry;
    }
    return result;
  }

  // A canonical answer belongs to one observation's hop POSITION, not a
  // prefix. Expose the O(1) full-key lookup without resolveFromServer's map
  // collapsing repeated prefixes. Null/malformed answers remain unresolved.
  function serverHopEntry(pubkey) {
    if (typeof pubkey !== 'string' || !pubkey) return null;
    const node = pubkeyIdx[pubkey.toLowerCase()] || null;
    return {
      name: node ? node.name : pubkey.slice(0, 8),
      pubkey: pubkey,
      candidates: node ? [{ name: node.name, pubkey: pubkey, lat: node.lat, lon: node.lon }] : [],
      conflicts: []
    };
  }

  // #1864: O(1) node-name lookup by FULL pubkey (64-char hex). Returns the
  // node name when a node with this exact public key is known, else null.
  // Used to resolve ANON_REQ source pubkeys to a display name.
  function nameForKey(pubkey) {
    if (!pubkey) return null;
    const n = pubkeyIdx[String(pubkey).toLowerCase()];
    return n && n.name ? n.name : null;
  }

  return { init: init, resolve: resolve, inferPath: inferPath, LIKELY_CONFIDENCE: LIKELY_CONFIDENCE, resolveFromServer: resolveFromServer, serverHopEntry: serverHopEntry, ready: ready, haversineKm: haversineKm, setAffinity: setAffinity, getAffinity: getAffinity, setPathAffinity: setPathAffinity, getPathAffinity: getPathAffinity, nameForKey: nameForKey };
})();
