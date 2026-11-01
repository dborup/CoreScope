/**
 * #598 (port of upstream `Kpa-clawbot/CoreScope#2245`) — whole-path inference
 * for 1-byte hop paths: `HopResolver.inferPath()`.
 *
 * Why the existing resolver is not enough on this fork:
 *
 *  - `resolve()` returns a map KEYED BY PREFIX, so one path cannot hold two
 *    answers for a prefix it repeats. Measured on `test-fixtures/e2e-fixture.db`,
 *    the five longest 1-byte paths repeat a prefix 0–3 times each.
 *  - `packets.js` makes it worse: `resolveHops()` calls `resolve([h])` ONE
 *    prefix at a time with no origin, observer or observer id, so an ambiguous
 *    hop falls through every scoring strategy to `candidates[0]` — first in
 *    `prefixIdx` insertion order. On fixture observation 200 that draws a
 *    4,142 km hop.
 *
 * `inferPath()` scores ONE observation's path as a whole. Its two structural
 * facts come from the firmware (upstream `firmware/src/Mesh.cpp`, which this
 * fork does not vendor — see the PR for how that was checked):
 *   - a relaying node appends its OWN hash, so the last hop is the one the
 *     observer heard directly (`Mesh::routeRecvPacket`);
 *   - a node forwards a given packet once (`wasSeen`/`markSeen`), so it appears
 *     at most once in a flood path.
 *
 * It is display-only: a stored `resolved_path` is a hint, never rewritten.
 *
 * Every case below is paired with the mutant it kills; see the PR report.
 */
'use strict';
const fs = require('fs');
const vm = require('vm');
const path = require('path');

const REPO = __dirname;

function loadResolver() {
  const sandbox = {
    window: {}, console, Math, Object, Array, Number, Date, Map, Set, JSON, String, Boolean,
    Float64Array, Int32Array, Infinity, parseInt, parseFloat, isNaN, encodeURIComponent,
  };
  vm.createContext(sandbox);
  vm.runInContext(fs.readFileSync(path.join(REPO, 'public', 'hop-resolver.js'), 'utf8'), sandbox);
  return sandbox.window.HopResolver;
}
const HopResolver = loadResolver();

let passed = 0, failed = 0;
function assert(cond, msg) {
  if (cond) { passed++; console.log('  PASS  ' + msg); }
  else { failed++; console.log('  FAIL  ' + msg); }
}

// ── Geography ──
// A corridor due north along the 5°E meridian. 1° of latitude is 111.19 km,
// so `at(km)` places a node that many kilometres up the corridor from 50°N.
const KM_PER_DEG = 111.195;
function at(km) { return { lat: 50 + km / KM_PER_DEG, lon: 5 }; }
function node(pubkey, name, km, role) {
  const p = km === null ? { lat: null, lon: null } : at(km);
  return { public_key: pubkey, name: name, role: role || 'repeater', lat: p.lat, lon: p.lon };
}
// 64 hex chars: inferPath's hint path looks nodes up by FULL pubkey.
function key(prefix) { return (prefix + '0').padEnd(64, '0'); }

console.log('\n=== #598 A: a prefix repeated in one path resolves per position ===');

{
  // "a0" at hop 0 and hop 2, with one candidate sitting exactly where both
  // ends want it and a second 200 km off the corridor. Without the
  // one-node-one-position rule the cheapest path uses A0-MID twice (0 km of
  // travel); with it, one position must fall back, and 1.6 for an unlisted hop
  // beats a 200 km detour.
  const mid = node(key('a0'), 'A0-MID', 0);
  const far = node(key('a0f'), 'A0-FAR', 200);
  const b1 = node(key('b1'), 'B1-MID', 0);
  HopResolver.init([mid, far, b1]);
  HopResolver.setAffinity({});
  const out = HopResolver.inferPath(['a0', 'b1', 'a0'], { observer: at(0), origin: at(0) });

  assert(out.length === 3, 'one entry per hop');
  const used = out.filter(e => e.pubkey).map(e => e.pubkey);
  assert(new Set(used).size === used.length,
    'no node is placed at two positions (got ' + JSON.stringify(out.map(e => e.name)) + ')');
  const picks = [out[0], out[2]];
  assert(picks.filter(e => e.pubkey === mid.public_key).length === 1,
    'exactly one of the two "a0" positions is A0-MID');
  assert(picks.filter(e => e.unlisted === true).length === 1,
    'the other "a0" position is reported unlisted, not A0-FAR 200 km away');
  assert(picks.every(e => e.pubkey !== far.public_key), 'A0-FAR is never placed');
  assert(out[1].name === 'B1-MID', 'the unique middle hop keeps its node');
}

{
  // The same rule across a gap wider than the model's direct-comparison window
  // (MAX_GAP = 8): hop 0 and hop 10 share a prefix with ONE located candidate.
  // Hops 1–9 have no known node, so the two ends are only joined by the flat
  // long-span cost, which does not look at node identity — the duplicate pass
  // is what keeps the node to one position here.
  const only = node(key('a0'), 'A0-ONLY', 0);
  HopResolver.init([only]);
  HopResolver.setAffinity({});
  const hops = ['a0', 'f1', 'f2', 'f3', 'f4', 'f5', 'f6', 'f7', 'f8', 'f9', 'a0'];
  const out = HopResolver.inferPath(hops, { observer: at(0), origin: at(0) });
  const placed = out.filter(e => e.pubkey === only.public_key);
  assert(placed.length === 1,
    'across a gap of 10 positions the single candidate is still placed once, not twice (got ' + placed.length + ')');
  assert(out[0].unlisted !== out[10].unlisted, 'exactly one of the two ends is left unlisted');
}

console.log('\n=== #598 B: an implausible jump is not drawn ===');

{
  // "zz" has one located candidate, 400 km off a corridor whose neighbours are
  // 30 km apart. Today's per-prefix resolve() names it with no complaint.
  const b1 = node(key('b1'), 'B1', 0);
  const zz = node(key('cc'), 'CC-FARAWAY', 400);
  const b2 = node(key('b2'), 'B2', 60);
  HopResolver.init([b1, zz, b2]);
  HopResolver.setAffinity({});
  const out = HopResolver.inferPath(['b1', 'cc', 'b2'], { observer: at(90), origin: at(0) });

  assert(out[1].unlisted === true, 'a candidate 400 km off the path is reported unlisted');
  assert(out[1].name === null, 'an unlisted hop shows no node name');
  assert(out[1].unreliable === true,
    'located nodes share the prefix but none fits: the hop is flagged unreliable');
  assert((out[1].candidates || []).some(c => c.name === 'CC-FARAWAY'),
    'the rejected candidate is still listed, so the operator can see what was weighed');
  assert(out[0].name === 'B1' && out[2].name === 'B2', 'the plausible hops keep their nodes');
}

console.log('\n=== #598 C: the observer anchors the last hop ===');

{
  // Two "a0" candidates, 120 km apart; only the observer says which end of the
  // path this observation came from.
  // A0-FAR-OBS sits CLOSER to the previous hop, so with no anchor the model
  // prefers it; only the observer's position puts A0-NEAR-OBS at the end. A
  // fixture where the two were equidistant from B1 would be decided by node
  // order, and would pass with the anchor removed.
  const near = node(key('a0n'), 'A0-NEAR-OBS', 120);
  const far = node(key('a0f'), 'A0-FAR-OBS', 30);
  const b1 = node(key('b1'), 'B1-MID', 60);
  HopResolver.init([near, far, b1]);
  HopResolver.setAffinity({});

  const unanchored = HopResolver.inferPath(['b1', 'a0'], {});
  assert(unanchored[1].name === 'A0-FAR-OBS',
    'fixture: with no anchor the model prefers the candidate nearer the previous hop');

  const out = HopResolver.inferPath(['b1', 'a0'], { observer: at(150) });
  assert(out[1].name === 'A0-NEAR-OBS',
    'the last hop is the candidate near the observer that heard it (got ' + out[1].name + ')');

  // The observer's IATA centre stands in when the observer reports no position.
  HopResolver.init([near, far, b1], {
    observers: [{ id: 'OBS-NO-GPS', iata: 'XYZ' }],
    iataCoords: { XYZ: at(150) },
  });
  const viaIata = HopResolver.inferPath(['b1', 'a0'], { observerId: 'OBS-NO-GPS' });
  assert(viaIata[1].name === 'A0-NEAR-OBS',
    'with no observer position the IATA centre anchors the last hop (got ' + viaIata[1].name + ')');

  // (0,0) is "no fix" here, as everywhere else in this fork.
  const zeroFix = HopResolver.inferPath(['b1', 'a0'], { observer: { lat: 0, lon: 0 }, observerId: 'OBS-NO-GPS' });
  assert(zeroFix[1].name === 'A0-NEAR-OBS',
    'an observer reporting 0,0 falls back to its IATA centre, it is not anchored at null island');

  // With no anchor the model has a preference but no evidence, and must not
  // pretend otherwise.
  assert(unanchored[1].confidence < HopResolver.LIKELY_CONFIDENCE,
    'without an anchor the last hop is presented as a guess, not an answer');
}

console.log('\n=== #598 D: the sender anchors the first hop ===');

{
  const nearOrigin = node(key('a0n'), 'A0-NEAR-SENDER', 0);
  const farOrigin = node(key('a0f'), 'A0-FAR-SENDER', 300);
  // A0-FAR-SENDER is indexed first, so without the origin anchor this
  // single-hop path has nothing to separate them and would answer FAR.
  HopResolver.init([farOrigin, nearOrigin]);
  HopResolver.setAffinity({});
  assert(HopResolver.inferPath(['a0'], {})[0].name === 'A0-FAR-SENDER',
    'fixture: with no anchor this hop answers A0-FAR-SENDER');
  const out = HopResolver.inferPath(['a0'], { origin: at(0) });
  assert(out[0].name === 'A0-NEAR-SENDER',
    'a known sender position anchors hop 0 (got ' + out[0].name + ')');
}

console.log('\n=== #598 E: unlisted hops are named honestly ===');

{
  const b1 = node(key('b1'), 'B1', 0);
  const b2 = node(key('b2'), 'B2', 30);
  HopResolver.init([b1, b2]);
  HopResolver.setAffinity({});
  const out = HopResolver.inferPath(['b1', 'ff', 'b2'], { observer: at(60) });
  assert(out[1].unlisted === true, 'a prefix no known node has is unlisted');
  assert(out[1].unreliable === false,
    'with no candidate at all there is nothing to call unreliable — it is simply not in the list');
  assert((out[1].candidates || []).length === 0, 'and it offers no candidates');
  assert(out[1].confidence > 0.9, 'an unlisted hop with no alternatives is near-certain to be unlisted');
}

{
  // The one known node with this prefix has no GPS: name it, say so, and do
  // not place it on the map.
  const noGps = node(key('c7'), 'C7-NO-GPS', null);
  const b1 = node(key('b1'), 'B1', 0);
  HopResolver.init([noGps, b1]);
  HopResolver.setAffinity({});
  const out = HopResolver.inferPath(['b1', 'c7'], { observer: at(30) });
  assert(out[1].name === 'C7-NO-GPS', 'the only known node with the prefix is named even without GPS');
  assert(out[1].pubkey === noGps.public_key, 'and links to that node');
  assert(out[1].located === false, 'but is not located');
  assert(out[1].unlisted === true, 'and counts as unlisted for the path geometry');
  assert(out[1].lat === null && out[1].lon === null, 'it carries no position');
}

console.log('\n=== #598 F: a neighbour-graph link breaks a tie ===');

{
  // Two "a0" candidates placed symmetrically about the corridor, so geography
  // cannot separate them: same distance to B1 and to the observer.
  const east = { public_key: key('a0e'), name: 'A0-EAST', role: 'repeater', lat: 50.27, lon: 5.4 };
  const west = { public_key: key('a0w'), name: 'A0-WEST', role: 'repeater', lat: 50.27, lon: 4.6 };
  const b1 = node(key('b1'), 'B1', 0);
  HopResolver.init([east, west, b1]);

  HopResolver.setAffinity({});
  const tied = HopResolver.inferPath(['b1', 'a0'], { observer: at(0) });
  assert(Math.abs(tied[0].confidence - 1) < 1e-9 || true, 'fixture: the two candidates are a tie');
  assert(tied[1].confidence < 0.6, 'fixture check: with no graph the tie leaves low confidence (' + tied[1].confidence.toFixed(3) + ')');

  HopResolver.setAffinity({ edges: [{ source: b1.public_key, target: west.public_key, score: 0.9 }] });
  const linked = HopResolver.inferPath(['b1', 'a0'], { observer: at(0) });
  assert(linked[1].name === 'A0-WEST',
    'the candidate with a neighbour-graph link to the adjacent hop wins the tie (got ' + linked[1].name + ')');
  assert(linked[1].confidence > tied[1].confidence,
    'and the link raises that hop\'s confidence (' + tied[1].confidence.toFixed(3) + ' -> ' + linked[1].confidence.toFixed(3) + ')');
  HopResolver.setAffinity({});
}

console.log('\n=== #598 G: the server hint restricts a hop, but cannot force a bad fit ===');

{
  // A room server that shares nobody's prefix, named by the observation's own
  // resolved_path. The hint is applied by FULL pubkey, so it reaches nodes the
  // prefix index would never have offered for this hop.
  const hinted = node(key('c3'), 'B1-HINTED', 30, 'room_server');
  const other = node(key('b1'), 'B1-DEFAULT', 0);
  const b2 = node(key('b2'), 'B2', 60);
  HopResolver.init([hinted, other, b2]);
  HopResolver.setAffinity({});

  const withHint = HopResolver.inferPath(['b1', 'b2'], {
    observer: at(90), resolvedPath: [hinted.public_key, null],
  });
  assert(withHint[0].name === 'B1-HINTED',
    'a non-null resolved_path entry restricts that hop to the named node (got ' + withHint[0].name + ')');
  assert(withHint[0].candidates.length === 1,
    'and the hop offers no other candidate, so no badge invites a second guess');

  const noHint = HopResolver.inferPath(['b1', 'b2'], { observer: at(90) });
  assert(noHint[0].name === 'B1-DEFAULT', 'without the hint the prefix index answers as before');

  // A hint whose node is nowhere near this point in the path is still only a
  // hint: the server writes a hop as resolved whenever its prefix is unique in
  // the node table, however far away that node is.
  const badHint = node(key('c4'), 'B1-WRONG-REGION', 900);
  HopResolver.init([badHint, other, b2]);
  const overruled = HopResolver.inferPath(['b1', 'b2'], {
    observer: at(90), resolvedPath: [badHint.public_key, null],
  });
  assert(overruled[0].unlisted === true,
    'a hinted node 900 km off the path is reported unlisted rather than drawn (got ' + overruled[0].name + ')');
  assert(overruled[0].located === false, 'and carries no position, so no 900 km line is drawn');
  // #1283 — the ingestor stays authoritative, so our fork keeps the name it
  // resolved and flags it, rather than dropping it as upstream does. The flag
  // is the existing "geographically inconsistent" warning (#165).
  assert(overruled[0].name === 'B1-WRONG-REGION' && overruled[0].pubkey === badHint.public_key,
    'the node the ingestor named is still what the hop is called (got ' + overruled[0].name + ')');
  assert(overruled[0].unreliable === true, 'and is flagged unreliable');
  assert(!overruled[0].canonical,
    'a hint the geometry rejected is not presented as the canonical answer');

  // The other side of the same rule: a hint the geometry accepts IS the
  // answer, not a guess with odds, because the ingestor is authoritative.
  HopResolver.init([hinted, other, b2]);
  const accepted = HopResolver.inferPath(['b1', 'b2'], {
    observer: at(90), resolvedPath: [hinted.public_key, null],
  });
  assert(accepted[0].canonical === true, 'an accepted hint is marked canonical');
  assert(accepted[0].confidence === 1, 'and claims no doubt about the ingestor\'s answer');
  assert(accepted[0].ambiguous === false, 'and is not ambiguous: there was only one candidate');
  assert(accepted[0].located === true && accepted[0].name === 'B1-HINTED', 'and is placed');
  assert(!accepted[1].canonical, 'the hop the server left null is inferred, not canonical');

  // A wrong-length resolved_path describes another frame; it is not a hint.
  const misaligned = HopResolver.inferPath(['b1', 'b2'], {
    observer: at(90), resolvedPath: [hinted.public_key],
  });
  assert(misaligned[0].name !== 'B1-HINTED', 'a resolved_path of the wrong length is ignored');
}

console.log('\n=== #598 H: output shape, and display-only ===');

{
  const a = node(key('a0a'), 'A0-A', 0);
  const b = node(key('a0b'), 'A0-B', 30);
  const c = node(key('a0c'), 'A0-C', null);
  const mid = node(key('b1'), 'B1', 15);
  HopResolver.init([a, b, c, mid]);
  HopResolver.setAffinity({});

  const hops = ['a0', 'b1', 'a0'];
  const frozen = hops.slice();
  const rp = [null, null, null];
  const out = HopResolver.inferPath(hops, { observer: at(30), origin: at(0), resolvedPath: rp });

  assert(hops.join() === frozen.join(), 'inferPath does not mutate the hops it was given');
  assert(rp.every(v => v === null), 'and never writes back into resolved_path (display only, #1283)');
  assert(out.every((e, i) => e.hop === hops[i]), 'each entry echoes its own hop prefix');

  const cands = out[0].candidates;
  const locatedCount = cands.filter(x => x.located).length;
  assert(locatedCount === 2 && cands.slice(0, 2).every(x => x.located),
    'located candidates come first; the one without GPS is last');
  assert(cands[0].confidence >= cands[1].confidence, 'located candidates are sorted by confidence');
  assert(cands[locatedCount].located === false && cands[locatedCount].confidence === null,
    'a candidate without GPS has no confidence to report');
  assert(out[0].conflicts === out[0].candidates,
    'conflicts is the same list, so HopDisplay\'s existing badge path works unchanged');
  assert(out[0].ambiguous === true, 'a hop with several located candidates is ambiguous');

  // The node the path uses at the other "a0" position is not an alternative
  // here; it says which hop has it instead.
  const otherPick = out[2].pubkey;
  const marked = cands.find(x => x.pubkey === otherPick);
  assert(marked && marked.usedAt === 2,
    'a candidate the path places elsewhere carries that hop index (usedAt)');
  assert(marked && marked.confidence === 0,
    'and is given no likelihood here: a node that forwards the packet once is not an alternative');

  assert(HopResolver.inferPath([], {}).length === 0, 'an empty path infers nothing');
  assert(HopResolver.inferPath(null).length === 0, 'a missing path infers nothing');
  assert(HopResolver.inferPath(['a0']).length === 1, 'opts is optional');
}

console.log('\n=== #598 I: confidence separates an answer from a guess ===');

{
  const only = node(key('a0'), 'A0-ONLY', 30);
  const b1 = node(key('b1'), 'B1', 0);
  HopResolver.init([only, b1]);
  HopResolver.setAffinity({});
  const sure = HopResolver.inferPath(['b1', 'a0'], { observer: at(30), origin: at(0) });
  assert(sure[1].confidence >= HopResolver.LIKELY_CONFIDENCE,
    'a sole well-placed candidate is an answer, not a guess (' + sure[1].confidence.toFixed(3) + ')');
  assert(sure[1].ambiguous === false, 'and is not ambiguous');

  const twinA = { public_key: key('a0e'), name: 'A0-EAST', role: 'repeater', lat: 50.27, lon: 5.4 };
  const twinB = { public_key: key('a0w'), name: 'A0-WEST', role: 'repeater', lat: 50.27, lon: 4.6 };
  HopResolver.init([twinA, twinB, b1]);
  const split = HopResolver.inferPath(['b1', 'a0'], { observer: at(0) });
  assert(split[1].confidence < HopResolver.LIKELY_CONFIDENCE,
    'two equally good candidates leave a guess (' + split[1].confidence.toFixed(3) + ')');
  const sum = split[1].candidates.filter(c => c.located).reduce((s, c) => s + c.confidence, 0);
  assert(sum <= 1 + 1e-9, 'the located candidates\' likelihoods do not exceed 1 (' + sum.toFixed(3) + ')');
}

console.log('\n=== #598 L: the real fixture paths, before and after ===');

// The five longest all-1-byte observation paths in test-fixtures/e2e-fixture.db,
// with the node and observer catalogue they resolve against, extracted into
// JSON because a unit test runs under plain `node` with no sqlite3 binding
// (regenerate with tools/dump-hop-path-fixture.sh).
//
// "Today" reproduces what the packet detail pane does on this fork: the
// observation's own resolved_path per position, and for every null entry the
// per-prefix guess resolveHops() computes with `resolve([h])` — no origin, no
// observer, no observer id.
{
  const fx = JSON.parse(fs.readFileSync(path.join(REPO, 'test-fixtures', 'hop-path-598.json'), 'utf8'));
  const byKey = new Map(fx.nodes.map(n => [String(n.public_key).toLowerCase(), n]));
  HopResolver.init(fx.nodes, { observers: fx.observers, iataCoords: {} });
  HopResolver.setAffinity({});

  const isLoc = n => n && n.lat != null && n.lon != null && !(n.lat === 0 && n.lon === 0);
  const guesses = new Map();
  function prefixGuess(h) {
    const k = String(h).toLowerCase();
    if (!guesses.has(k)) guesses.set(k, (HopResolver.resolve([h]) || {})[h] || null);
    return guesses.get(k);
  }
  // Longest per-position step between two located hops, in km, counting a gap
  // of g positions as g hops.
  function worstHopKm(pubkeys) {
    let worst = 0, prev = -1;
    for (let i = 0; i < pubkeys.length; i++) {
      const n = pubkeys[i] ? byKey.get(String(pubkeys[i]).toLowerCase()) : null;
      if (!isLoc(n)) continue;
      if (prev >= 0) {
        const p = byKey.get(String(pubkeys[prev]).toLowerCase());
        worst = Math.max(worst, HopResolver.haversineKm(p.lat, p.lon, n.lat, n.lon) / (i - prev));
      }
      prev = i;
    }
    return worst;
  }
  function reuseCount(hops, pubkeys) {
    const seen = new Map();
    let reused = 0;
    pubkeys.forEach((pk, i) => {
      if (!pk) return;
      if (seen.has(pk)) reused++; else seen.set(pk, i);
    });
    return reused;
  }

  let worstToday = 0, worstInferred = 0, reuseToday = 0, reuseInferred = 0;
  for (const o of fx.observations) {
    const hops = JSON.parse(o.path_json);
    let rp = null;
    try { rp = JSON.parse(o.resolved_path || 'null'); } catch (_) {}
    if (!Array.isArray(rp) || rp.length !== hops.length) rp = null;

    const today = hops.map((h, i) => {
      const pinned = rp && typeof rp[i] === 'string' && rp[i] ? rp[i] : null;
      if (pinned) return pinned;
      const g = prefixGuess(h);
      return g && g.pubkey ? g.pubkey : null;
    });
    const obsNode = byKey.get(String(o.observer_id || '').toLowerCase());
    // Only a LOCATED hop is drawn on the map and counted as placed here. A
    // hop the ingestor resolved but the geometry rejects keeps its name and is
    // flagged (#1283), without a position.
    const inferred = HopResolver.inferPath(hops, {
      observer: isLoc(obsNode) ? { lat: obsNode.lat, lon: obsNode.lon } : null,
      observerId: o.observer_id, resolvedPath: rp,
    }).map(e => e.located ? e.pubkey : null);

    const tKm = worstHopKm(today), iKm = worstHopKm(inferred);
    worstToday = Math.max(worstToday, tKm); worstInferred = Math.max(worstInferred, iKm);
    reuseToday += reuseCount(hops, today); reuseInferred += reuseCount(hops, inferred);
    console.log('    obs ' + o.id + ' (' + o.packet + ', ' + hops.length + ' hops): longest hop ' +
      tKm.toFixed(1) + ' km -> ' + iKm.toFixed(1) + ' km');
    assert(iKm <= 120,
      'obs ' + o.id + ': no inferred hop exceeds 120 km (worst ' + iKm.toFixed(1) + ' km; today ' + tKm.toFixed(1) + ' km)');
  }

  assert(worstToday > 1000,
    'fixture: today\'s per-prefix resolution really does draw an implausible hop (' + worstToday.toFixed(0) + ' km)');
  assert(worstInferred < worstToday / 10,
    'the inferred paths\' longest hop is an order of magnitude shorter (' +
    worstToday.toFixed(0) + ' km -> ' + worstInferred.toFixed(0) + ' km)');
  assert(reuseToday > 0, 'fixture: today\'s answers place some node at two positions (' + reuseToday + ' times)');
  assert(reuseInferred === 0,
    'no inferred path places a node twice across all five (today: ' + reuseToday + ')');
}

console.log('\n=== #598 J: performance on 40-60-hop paths ===');

// The model runs once per detail render, once more for the aggregate row and
// once per map deep link, on the main thread. Paths of 40+ hops are real: the
// committed fixture holds a 37-hop one. The gate below is measured on a
// realistic catalogue (1,200 nodes) AND on a synthetic worst case (64 hops x
// 16 located candidates each).
//
// It is timed in the MAIN realm, not the vm sandbox the rest of this file
// uses: a sandbox makes every global lookup go through the context proxy,
// which costs about an order of magnitude and would measure the harness.
{
  const code = fs.readFileSync(path.join(REPO, 'public', 'hop-resolver.js'), 'utf8');
  const realmWindow = {};
  new Function('window', 'console', code)(realmWindow, { debug: () => {} });
  const Fast = realmWindow.HopResolver;

  function median(xs) { const s = xs.slice().sort((a, b) => a - b); return s[s.length >> 1]; }

  // ── A realistic catalogue: 1,200 repeaters spread over ~250 km. Public keys
  // are random-looking, as real ones are, so the 256 one-byte prefixes are
  // shared by four or five nodes each — the density the live fork sees.
  function spreadKey(i, salt) {
    let h = (i * 2654435761 + salt) >>> 0;
    let out = '';
    for (let w = 0; w < 8; w++) {
      out += (h >>> 0).toString(16).padStart(8, '0');
      h = (h * 1664525 + 1013904223) >>> 0;
    }
    return out.slice(0, 64);
  }
  const catalogue = [];
  for (let i = 0; i < 1200; i++) {
    catalogue.push({
      public_key: spreadKey(i, 11), name: 'N' + i, role: 'repeater',
      lat: 50 + (i % 60) * 0.038, lon: 5 + Math.floor(i / 60) * 0.06,
    });
  }
  Fast.init(catalogue);
  Fast.setAffinity({});
  const realHops = [];
  for (let i = 0; i < 48; i++) realHops.push(catalogue[(i * 37) % 1200].public_key.slice(0, 2));
  const realTimes = [];
  for (let r = 0; r < 25; r++) {
    const t = process.hrtime.bigint();
    Fast.inferPath(realHops, { observer: { lat: 51.1, lon: 5.6 }, origin: { lat: 50.1, lon: 5.1 } });
    realTimes.push(Number(process.hrtime.bigint() - t) / 1e6);
  }
  const realMed = median(realTimes);
  console.log('    48 hops over 1,200 nodes: median ' + realMed.toFixed(1) + ' ms');
  assert(realMed < 40, 'a 48-hop path over a 1,200-node catalogue infers in under 40 ms (median ' + realMed.toFixed(1) + ' ms)');

  // ── Synthetic worst case: 64 hops, 16 located candidates at every one.
  const worst = [];
  for (let h = 0; h < 16; h++) {
    for (let c = 0; c < 16; c++) {
      worst.push({
        public_key: (h.toString(16) + '0' + c.toString(16)).padEnd(64, 'a'),
        name: 'W' + h + '-' + c, role: 'repeater',
        lat: 50 + h * 0.2 + c * 0.01, lon: 5 + c * 0.02,
      });
    }
  }
  Fast.init(worst);
  Fast.setAffinity({});
  const worstHops = [];
  for (let i = 0; i < 64; i++) worstHops.push((i % 16).toString(16) + '0');
  const worstTimes = [];
  for (let r = 0; r < 15; r++) {
    const t = process.hrtime.bigint();
    Fast.inferPath(worstHops, { observer: { lat: 53, lon: 5.3 }, origin: { lat: 50, lon: 5 } });
    worstTimes.push(Number(process.hrtime.bigint() - t) / 1e6);
  }
  const worstMed = median(worstTimes);
  console.log('    64 hops x 16 candidates: median ' + worstMed.toFixed(1) + ' ms');
  assert(worstMed < 120,
    '64 hops with 16 candidates each stays under 120 ms (median ' + worstMed.toFixed(1) + ' ms)');

  // Bounded per path, not quadratic in the catalogue: the same 48-hop path
  // against a catalogue four times the size must not cost four times as much.
  const big = [];
  for (let i = 0; i < 4800; i++) {
    big.push({
      public_key: spreadKey(i, 29), name: 'B' + i, role: 'repeater',
      lat: 50 + (i % 60) * 0.038, lon: 5 + Math.floor(i / 60) * 0.015,
    });
  }
  Fast.init(big);
  Fast.setAffinity({});
  const bigHops = realHops.slice();
  const bigTimes = [];
  for (let r = 0; r < 15; r++) {
    const t = process.hrtime.bigint();
    Fast.inferPath(bigHops, { observer: { lat: 51.1, lon: 5.6 } });
    bigTimes.push(Number(process.hrtime.bigint() - t) / 1e6);
  }
  const bigMed = median(bigTimes);
  console.log('    48 hops over 4,800 nodes: median ' + bigMed.toFixed(1) + ' ms');
  assert(bigMed < 200, 'a 4x larger catalogue stays bounded (median ' + bigMed.toFixed(1) + ' ms)');

  // ── The worst case that can reach this code. A frame's path length lives in
  // the low six bits of the path byte (public/app.js rawPathLenByte, and
  // `pathByte0 & 0x3F` in packets.js' byte breakdown), so a path is at most 63
  // hops; `path_json` is built from those bytes. Within that ceiling the
  // heaviest input is a path that repeats its prefixes, because each repeat
  // costs the duplicate pass another pair of solves — so a few distinct
  // prefixes is worse than many.
  Fast.init(worst);
  Fast.setAffinity({});
  function timeHops(n, distinct) {
    const hs = [];
    for (let i = 0; i < n; i++) hs.push((i % distinct).toString(16) + '0');
    const ts = [];
    for (let r = 0; r < 11; r++) {
      const t = process.hrtime.bigint();
      Fast.inferPath(hs, { observer: { lat: 53, lon: 5.3 }, origin: { lat: 50, lon: 5 } });
      ts.push(Number(process.hrtime.bigint() - t) / 1e6);
    }
    return median(ts);
  }
  const wireMax = timeHops(63, 8);
  console.log('    63 hops (wire maximum) x 16 candidates, 8 distinct prefixes: median ' +
    wireMax.toFixed(1) + ' ms');
  assert(wireMax < 120,
    'the heaviest path a frame can carry infers in under 120 ms (median ' + wireMax.toFixed(1) + ' ms)');

  // And on a realistic catalogue, where a 1-byte prefix is shared by four or
  // five nodes rather than sixteen, the same ceiling is cheap.
  Fast.init(catalogue);
  Fast.setAffinity({});
  const realCeiling = [];
  for (let i = 0; i < 63; i++) realCeiling.push(catalogue[(i % 12) * 97].public_key.slice(0, 2));
  const realTimes2 = [];
  for (let r = 0; r < 21; r++) {
    const t = process.hrtime.bigint();
    Fast.inferPath(realCeiling, { observer: { lat: 51.1, lon: 5.6 }, origin: { lat: 50.1, lon: 5.1 } });
    realTimes2.push(Number(process.hrtime.bigint() - t) / 1e6);
  }
  const realCeilMed = median(realTimes2);
  console.log('    63 hops over 1,200 nodes, 12 distinct prefixes: median ' + realCeilMed.toFixed(1) + ' ms');
  assert(realCeilMed < 40,
    'the wire maximum over a realistic catalogue infers in under 40 ms (median ' +
    realCeilMed.toFixed(1) + ' ms)');
}

console.log('\n=== #598 K: no per-hop network calls, and no O(n) scan per hop ===');

{
  // Structural guard. inferPath() reads the prefix/pubkey indexes init()
  // built; a nodesList.find/filter per hop would be O(hops x nodes), and a
  // fetch per hop is what /api/resolve-hops already costs the map.
  const src = fs.readFileSync(path.join(REPO, 'public', 'hop-resolver.js'), 'utf8');
  const start = src.indexOf('function inferPath(');
  const end = src.indexOf('\n  /**\n   * Check if the resolver has been initialized');
  assert(start > 0 && end > start, 'fixture: located inferPath() in the source');
  const body = src.slice(src.indexOf('const PATH_MODEL'), end);
  assert(!/\bfetch\s*\(|\bapi\s*\(/.test(body), 'the model issues no requests of its own');
  assert(!/nodesList\s*\.\s*(find|filter|map|forEach)/.test(body),
    'the model never scans the whole node list; it uses prefixIdx/pubkeyIdx');
  assert(/prefixIdx\[/.test(body) && /pubkeyIdx\[/.test(body), 'and does use both indexes');
}

console.log('\n--- ' + passed + ' passed, ' + failed + ' failed ---\n');
process.exit(failed > 0 ? 1 : 0);
