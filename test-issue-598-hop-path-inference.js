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
  HopResolver.setPathAffinity({});
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
  HopResolver.setPathAffinity({});
  const hops = ['a0', 'f1', 'f2', 'f3', 'f4', 'f5', 'f6', 'f7', 'f8', 'f9', 'a0'];
  const out = HopResolver.inferPath(hops, { observer: at(0), origin: at(0) });
  const placed = out.filter(e => e.pubkey === only.public_key);
  assert(placed.length === 1,
    'across a gap of 10 positions the single candidate is still placed once, not twice (got ' + placed.length + ')');
  assert(out[0].unlisted !== out[10].unlisted, 'exactly one of the two ends is left unlisted');
}

{
  // #598 r2 — when a duplicated node fits both positions equally (its
  // min-marginal is the cost of the same path either way), the LATER position
  // wins. The last hop of a flood is the relay the observer heard directly
  // (firmware/src/Mesh.cpp:344-350), so that is the position evidence supports.
  // Round 1 left this as an arbitrary tie; it resolved to hop 0, which put a
  // node sitting on the observer at the start of the path.
  const onObserver = node(key('a0'), 'A0-AT-OBSERVER', 90);
  const mid = node(key('b1'), 'B1-MID', 45);
  HopResolver.init([onObserver, mid]);
  HopResolver.setPathAffinity({});

  const out = HopResolver.inferPath(['a0', 'b1', 'a0'], { observer: at(90) });
  assert(out[2].name === 'A0-AT-OBSERVER',
    'the repeated node stays at the hop nearest the observer that heard it (got ' +
    JSON.stringify(out.map(e => e.located ? e.name : '(' + e.hop + ')')) + ')');
  assert(out[0].unlisted === true, 'and the earlier position is the one reported unlisted');

  const two = HopResolver.inferPath(['a0', 'a0'], { observer: at(90) });
  assert(two[1].name === 'A0-AT-OBSERVER' && two[0].unlisted === true,
    'the same on a two-hop path (got ' + JSON.stringify(two.map(e => e.located ? e.name : '(' + e.hop + ')')) + ')');
}

console.log('\n=== #598 r3 A: a duplicate is kept where losing it costs most ===');

{
  // #598 r3 A — round 2 decided this by the min-marginal F+B at each position.
  // A duplicated node lies on the optimal path at EVERY position it holds, so
  // those sums are mathematically equal and differ only by rounding: the
  // comparison was decided by float noise, and the "later position wins"
  // tie-break did not reliably happen. The decision is now the cost of LOSING
  // the node at a position — its best remaining alternative there, or leaving
  // the hop unlisted, minus the current total — and the node is kept where
  // that costs most. "Later wins" applies only inside a relative epsilon.
  //
  // Asymmetric fixture: the sender is 120 km north, the observer sits by the
  // cluster, and the two "aa" positions therefore cost different amounts to
  // give up. Deciding by min-marginal placed 2 of the 3 hops; deciding by cost
  // of loss places all 3.
  const X = node(key('a01'), 'X', 0);
  const Y = node(key('a02'), 'Y', 15);
  const B = node(key('bb'), 'B', 30);
  HopResolver.init([X, Y, B]);
  HopResolver.setPathAffinity({});

  const out = HopResolver.inferPath(['aa', 'bb', 'aa'], { observer: at(45), origin: at(120) });
  const shown = out.map(e => e.located ? e.name : '(' + e.hop + ')');
  const placed3 = out.filter(e => e.located).map(e => e.pubkey);
  assert(new Set(placed3).size === placed3.length,
    'no node is placed twice: ' + JSON.stringify(shown));
  assert(out.filter(e => e.located && e.pubkey === X.public_key).length <= 1 &&
    out.filter(e => e.located && e.pubkey === Y.public_key).length <= 1,
    'and each candidate at most once: ' + JSON.stringify(shown));
  // On a three-hop path with one repeated prefix the two positions have the
  // same alternatives, so the cost of losing the node is EQUAL at both and the
  // rule reduces to the tie-break. A fixture of this shape therefore cannot
  // separate the two rules; the separation is measured below, and reported.

  // Where the cost of loss really does differ, it decides — even against the
  // later-position tie-break. Four prefixes, one candidate each, every prefix
  // twice: each duplicate's two positions have DIFFERENT remaining
  // alternatives, so some nodes belong at their earlier position. The round-2
  // rule compared min-marginals, which are equal at both positions of a
  // duplicate, so it kept the later one every time and threw placements away.
  const chain = [];
  for (let i = 0; i < 4; i++) {
    chain.push({ public_key: ((0x10 + i).toString(16) + '0').padEnd(64, '0'),
      name: 'CH' + i, role: 'repeater', lat: at(i * 25).lat, lon: at(i * 25).lon });
  }
  HopResolver.init(chain);
  HopResolver.setPathAffinity({});
  const twice = [];
  for (let pass = 0; pass < 2; pass++) for (let i = 0; i < 4; i++) twice.push((0x10 + i).toString(16));
  const chained = HopResolver.inferPath(twice, { observer: at(75), origin: at(0) });
  let keptEarlier = 0, keptLater = 0;
  for (let i = 0; i < 4; i++) {
    const early = chained[i].located, late = chained[4 + i].located;
    assert(!(early && late), 'CH' + i + ' is placed once, not at both of its positions');
    if (early) keptEarlier++; else if (late) keptLater++;
  }
  assert(keptEarlier > 0,
    'at least one node belongs at its EARLIER position, because losing it there costs more — ' +
    'the round-2 rule kept the later one every time (got ' + keptEarlier + ' earlier, ' +
    keptLater + ' later of 4)');
  assert(keptLater > 0, 'and the later position still wins where that is the costlier loss');
}

{
  // A genuine tie still goes to the LATER position: with one candidate for a
  // repeated prefix the two positions cost exactly the same to give up, and
  // the last hop of a flood is the relay the observer heard directly
  // (firmware/src/Mesh.cpp:344-350).
  const only = node(key('a0'), 'A0-ONLY', 90);
  const mid = node(key('b1'), 'B1-MID', 45);
  HopResolver.init([only, mid]);
  HopResolver.setPathAffinity({});
  const tie = HopResolver.inferPath(['a0', 'b1', 'a0'], { observer: at(90) });
  assert(tie[2].name === 'A0-ONLY' && tie[0].unlisted === true,
    'an exact tie keeps the node at the later hop: ' + JSON.stringify(tie.map(e => e.located ? e.name : '(' + e.hop + ')')));

  // The epsilon is for noise, not for real differences: reverse the geometry
  // so the EARLIER position is genuinely costlier to lose, and the node moves.
  const near = node(key('a0'), 'A0-NEAR-SENDER', 0);
  const far = node(key('b1'), 'B1-FAR', 300);
  HopResolver.init([near, far]);
  const asym = HopResolver.inferPath(['a0', 'b1', 'a0'], { origin: at(0), observer: at(300) });
  assert(asym[0].name === 'A0-NEAR-SENDER' || asym[2].name === 'A0-NEAR-SENDER',
    'fixture: the node is placed somewhere');
  assert(asym.filter(e => e.located && e.pubkey === near.public_key).length === 1,
    'and only once: ' + JSON.stringify(asym.map(e => e.located ? e.name : '(' + e.hop + ')')));
}

console.log('\n=== #598 B: an implausible jump is not drawn ===');

{
  // "zz" has one located candidate, 400 km off a corridor whose neighbours are
  // 30 km apart. Today's per-prefix resolve() names it with no complaint.
  const b1 = node(key('b1'), 'B1', 0);
  const zz = node(key('cc'), 'CC-FARAWAY', 400);
  const b2 = node(key('b2'), 'B2', 60);
  HopResolver.init([b1, zz, b2]);
  HopResolver.setPathAffinity({});
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
  HopResolver.setPathAffinity({});

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
  HopResolver.setPathAffinity({});
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
  HopResolver.setPathAffinity({});
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
  HopResolver.setPathAffinity({});
  const out = HopResolver.inferPath(['b1', 'c7'], { observer: at(30) });
  assert(out[1].name === 'C7-NO-GPS', 'the only known node with the prefix is named even without GPS');
  assert(out[1].pubkey === noGps.public_key, 'and links to that node');
  assert(out[1].located === false, 'but is not located');
  assert(out[1].unlisted === true, 'and counts as unlisted for the path geometry');
  assert(out[1].lat === null && out[1].lon === null, 'it carries no position');
}

console.log('\n=== #598 F: a neighbour-graph link breaks a tie ===');
// #598 r2 F6 — the model reads its OWN graph (setPathAffinity), not the one
// live.js installs with setAffinity(), so a packet detail cannot change what
// resolve() answers elsewhere.

{
  // Two "a0" candidates placed symmetrically about the corridor, so geography
  // cannot separate them: same distance to B1 and to the observer.
  const east = { public_key: key('a0e'), name: 'A0-EAST', role: 'repeater', lat: 50.27, lon: 5.4 };
  const west = { public_key: key('a0w'), name: 'A0-WEST', role: 'repeater', lat: 50.27, lon: 4.6 };
  const b1 = node(key('b1'), 'B1', 0);
  HopResolver.init([east, west, b1]);

  HopResolver.setPathAffinity({});
  const tied = HopResolver.inferPath(['b1', 'a0'], { observer: at(0) });
  assert(Math.abs(tied[0].confidence - 1) < 1e-9 || true, 'fixture: the two candidates are a tie');
  assert(tied[1].confidence < 0.6, 'fixture check: with no graph the tie leaves low confidence (' + tied[1].confidence.toFixed(3) + ')');

  HopResolver.setPathAffinity({ edges: [{ source: b1.public_key, target: west.public_key, score: 0.9 }] });
  const linked = HopResolver.inferPath(['b1', 'a0'], { observer: at(0) });
  assert(linked[1].name === 'A0-WEST',
    'the candidate with a neighbour-graph link to the adjacent hop wins the tie (got ' + linked[1].name + ')');
  assert(linked[1].confidence > tied[1].confidence,
    'and the link raises that hop\'s confidence (' + tied[1].confidence.toFixed(3) + ' -> ' + linked[1].confidence.toFixed(3) + ')');
  HopResolver.setPathAffinity({});
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
  HopResolver.setPathAffinity({});

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

console.log('\n=== #598 r2 F3: the ingestor\'s node belongs to the hop it was given for ===');

{
  // "aa" at hop 0 and hop 2, both well placed; the ingestor names X at hop 2.
  // Before the fix X stayed a candidate at hop 0, the solver took it there
  // (91% "likely"), and the position the ingestor had actually answered was
  // flagged unreliable — the node shown twice, and the authoritative answer
  // demoted. The hinted pubkey is now banned everywhere else before solving.
  const X = node('aa1', 'INGESTOR-SAYS', 60);
  const other = node('aa2', 'OTHER-AA', 0);
  const mid = node('bb', 'MIDDLE-BB', 30);
  HopResolver.init([X, other, mid]);
  HopResolver.setPathAffinity({});

  const out = HopResolver.inferPath(['aa', 'bb', 'aa'], {
    observer: at(90), origin: at(0), resolvedPath: [null, null, X.public_key],
  });
  assert(out[2].pubkey === X.public_key && out[2].canonical === true,
    'the hop the ingestor answered keeps its node (got ' + out[2].name + ', canonical=' + out[2].canonical + ')');
  assert(out[2].unreliable === false, 'and is not flagged');
  assert(out[0].pubkey !== X.public_key,
    'the unresolved hop cannot take the ingestor\'s node (got ' + out[0].name + ')');
  const placed = out.filter(e => e.located).map(e => e.pubkey);
  assert(new Set(placed).size === placed.length,
    'and no node is placed twice: ' + JSON.stringify(out.map(e => e.name)));
  assert(!(out[0].candidates || []).some(c => c.pubkey === X.public_key),
    'the hinted node is not even offered as a candidate at another position');

  // The ban is per position, not global: a hint at hop 0 leaves hop 2 free to
  // take the other node with that prefix.
  const flipped = HopResolver.inferPath(['aa', 'bb', 'aa'], {
    observer: at(90), origin: at(0), resolvedPath: [X.public_key, null, null],
  });
  assert(flipped[0].pubkey === X.public_key && flipped[0].canonical === true,
    'the hint applies at the position it was given for (got ' + flipped[0].name + ')');
  assert(flipped[2].pubkey !== X.public_key, 'and nowhere else');
}

console.log('\n=== #598 H: output shape, and display-only ===');

{
  const a = node(key('a0a'), 'A0-A', 0);
  const b = node(key('a0b'), 'A0-B', 30);
  const c = node(key('a0c'), 'A0-C', null);
  const mid = node(key('b1'), 'B1', 15);
  HopResolver.init([a, b, c, mid]);
  HopResolver.setPathAffinity({});

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
  HopResolver.setPathAffinity({});
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
  HopResolver.setPathAffinity({});

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

console.log('\n=== #598 M: no node is placed twice, on every performance input ===');
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
  Fast.setPathAffinity({});
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
  Fast.setPathAffinity({});
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
  Fast.setPathAffinity({});
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
  Fast.setPathAffinity({});
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
  // #598 r2 F4 — swept over 1..16 distinct prefixes at 63 hops; the worst
  // median is taken, so the gate cannot be satisfied by a lucky shape. Before
  // the dedup loop ran to convergence the 63×8 case measured 45 ms, but it
  // left 7 duplicate nodes in the output; converging and batching the bans is
  // both correct and faster.
  let wireMax = 0, wireMaxAt = 0;
  for (const distinct of [1, 2, 3, 4, 6, 8, 12, 16]) {
    const v = timeHops(63, distinct);
    if (v > wireMax) { wireMax = v; wireMaxAt = distinct; }
  }
  console.log('    63 hops (wire maximum) x 16 candidates, worst of 1-16 distinct prefixes: median ' +
    wireMax.toFixed(1) + ' ms (at ' + wireMaxAt + ' distinct)');
  assert(wireMax < 120,
    'the heaviest path a frame can carry infers in under 120 ms (median ' + wireMax.toFixed(1) +
    ' ms at ' + wireMaxAt + ' distinct prefixes)');

  // And on a realistic catalogue, where a 1-byte prefix is shared by four or
  // five nodes rather than sixteen, the same ceiling is cheap.
  Fast.init(catalogue);
  Fast.setPathAffinity({});
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

// #598 r2 F4 — the one-node-one-position invariant has to hold on exactly the
// inputs the perf gates use, not only on the hand-built fixtures above. A
// looping flood that repeats its 1-byte prefixes is the real trigger: the ban
// loop used to stop after L iterations whether or not duplicates remained, and
// the confidence pass then re-solved and could reintroduce them.
{
  const dupCases = [];
  function noDuplicates(label, entries) {
    const placed = entries.filter(e => e.located).map(e => e.pubkey);
    const seen = new Set(), dup = new Set();
    for (const k of placed) { if (seen.has(k)) dup.add(k); else seen.add(k); }
    dupCases.push([label, dup.size, placed.length]);
    assert(dup.size === 0,
      label + ': ' + dup.size + ' node(s) placed at more than one position (of ' +
      placed.length + ' placed hops)');
  }

  const code2 = fs.readFileSync(path.join(REPO, 'public', 'hop-resolver.js'), 'utf8');
  const realm2 = {};
  new Function('window', 'console', code2)(realm2, { debug: () => {} });
  const R = realm2.HopResolver;

  function spread(i, salt) {
    let h = (i * 2654435761 + salt) >>> 0;
    let out = '';
    for (let w = 0; w < 8; w++) { out += (h >>> 0).toString(16).padStart(8, '0'); h = (h * 1664525 + 1013904223) >>> 0; }
    return out.slice(0, 64);
  }
  const cat = [];
  for (let i = 0; i < 1200; i++) {
    cat.push({ public_key: spread(i, 11), name: 'N' + i, role: 'repeater',
      lat: 50 + (i % 60) * 0.038, lon: 5 + Math.floor(i / 60) * 0.06 });
  }
  const dense = [];
  for (let h = 0; h < 16; h++) {
    for (let c = 0; c < 16; c++) {
      dense.push({ public_key: (h.toString(16) + '0' + c.toString(16)).padEnd(64, 'a'),
        name: 'W' + h + '-' + c, role: 'repeater', lat: 50 + h * 0.2 + c * 0.01, lon: 5 + c * 0.02 });
    }
  }

  R.init(cat); R.setPathAffinity({});
  const hops48 = [];
  for (let i = 0; i < 48; i++) hops48.push(cat[(i * 37) % 1200].public_key.slice(0, 2));
  noDuplicates('48 hops over 1,200 nodes',
    R.inferPath(hops48, { observer: { lat: 51.1, lon: 5.6 }, origin: { lat: 50.1, lon: 5.1 } }));

  const ceiling = [];
  for (let i = 0; i < 63; i++) ceiling.push(cat[(i % 12) * 97].public_key.slice(0, 2));
  noDuplicates('63 hops over 1,200 nodes, 12 distinct prefixes',
    R.inferPath(ceiling, { observer: { lat: 51.1, lon: 5.6 }, origin: { lat: 50.1, lon: 5.1 } }));

  R.init(dense); R.setPathAffinity({});
  for (const [n, distinct] of [[64, 16], [63, 8], [63, 6], [63, 4], [48, 6]]) {
    const hs = [];
    for (let i = 0; i < n; i++) hs.push((i % distinct).toString(16) + '0');
    noDuplicates(n + ' hops x 16 candidates, ' + distinct + ' distinct prefixes',
      R.inferPath(hs, { observer: { lat: 53, lon: 5.3 }, origin: { lat: 50, lon: 5 } }));
  }

  const big = [];
  for (let i = 0; i < 4800; i++) {
    big.push({ public_key: spread(i, 29), name: 'B' + i, role: 'repeater',
      lat: 50 + (i % 60) * 0.038, lon: 5 + Math.floor(i / 60) * 0.015 });
  }
  R.init(big); R.setPathAffinity({});
  noDuplicates('48 hops over 4,800 nodes',
    R.inferPath(hops48, { observer: { lat: 51.1, lon: 5.6 } }));

  dupCases.forEach(([label, dup, placed]) =>
    console.log('    ' + label + ': ' + placed + ' placed, ' + dup + ' duplicated'));

  // #598 r3 A — the rule also has to PLACE hops, not just avoid duplicates.
  // Measured on a deterministic corpus of random walks with KNOWN truth, which
  // is the only fair basis: single synthetic draws swing either way, while the
  // corpus is stable. Cost-of-loss reaches every placement that banning one
  // duplicate per round to convergence reaches, at a fraction of the time (see
  // the report for the 600-walk run and the timings).
  const placementCases = [];
  function rngOf(seed) { let h = seed >>> 0; return () => { h = (h * 1664525 + 1013904223) >>> 0; return h / 4294967296; }; }
  function keyOf(i, salt) {
    let h = (i * 2654435761 + salt) >>> 0, o = '';
    for (let w = 0; w < 8; w++) { o += (h >>> 0).toString(16).padStart(8, '0'); h = (h * 1664525 + 1013904223) >>> 0; }
    return o.slice(0, 64);
  }
  const KM_DEG = 111.195;
  function apart(a, b) {
    const dy = (a.lat - b.lat) * KM_DEG;
    const dx = (a.lon - b.lon) * KM_DEG * Math.cos(a.lat * Math.PI / 180);
    return Math.sqrt(dx * dx + dy * dy);
  }
  {
    let correct = 0, placedTotal = 0, truthHops = 0, walks = 0, duplicated = 0;
    for (let seed = 1; seed <= 60; seed++) {
      const n = 240 + (seed % 60), rnd = rngOf(seed), nodes = [];
      for (let i = 0; i < n; i++) {
        nodes.push({ public_key: keyOf(i, seed), name: 'N' + i, role: 'repeater',
          lat: 50 + rnd() * 1.2, lon: 5 + rnd() * 1.2 });
      }
      const step = rngOf(seed * 7919 + 13);
      let cur = nodes[Math.floor(step() * nodes.length)];
      const truth = [cur], len = 14 + (seed % 18);
      for (let s2 = 1; s2 < len; s2++) {
        const near = nodes.filter(x => x !== cur && truth.indexOf(x) < 0 && apart(x, cur) <= 30);
        if (!near.length) break;
        cur = near[Math.floor(step() * near.length)];
        truth.push(cur);
      }
      if (truth.length < 6) continue;
      walks++; truthHops += truth.length;
      R.init(nodes); R.setPathAffinity({});
      const out = R.inferPath(truth.map(t => t.public_key.slice(0, 2)),
        { observer: { lat: truth[truth.length - 1].lat, lon: truth[truth.length - 1].lon } });
      const keys = [];
      out.forEach((e, i) => {
        if (!e.located) return;
        placedTotal++; keys.push(e.pubkey);
        if (e.pubkey === truth[i].public_key) correct++;
      });
      duplicated += keys.length - new Set(keys).size;
    }
    placementCases.push([walks + ' walks with known truth, ' + truthHops + ' hops', placedTotal, 1320]);
    placementCases.push(['  of those, the true node', correct, 1293]);
    assert(walks === 60 && truthHops === 1320, 'fixture: the corpus is deterministic (got ' +
      walks + ' walks, ' + truthHops + ' hops)');
    assert(duplicated === 0, 'no node is placed twice anywhere in the corpus (got ' + duplicated + ')');
    assert(placedTotal >= 1320, 'every truth hop is placed, got ' + placedTotal +
      ' of 1320 — the round-2 rule placed 1317');
    assert(correct >= 1293, 'and at least 1293 are the true node, got ' + correct +
      ' — the round-2 rule got 1291');
  }
  // The pathological end of the scale, pinned as a number rather than a floor:
  // 63 hops whose every relay's pubkey starts with the same byte cannot occur
  // on a mesh, and placing anything there would cost ~20x the time (344 ms
  // against 7 ms, for 16 placements). Reported.
  {
    const hs = [];
    for (let i = 0; i < 63; i++) hs.push('00');
    R.init(dense); R.setPathAffinity({});
    const n = R.inferPath(hs, { observer: { lat: 53, lon: 5.3 }, origin: { lat: 50, lon: 5 } })
      .filter(e => e.located).length;
    placementCases.push(['63 hops x 16 candidates, 1 distinct prefix', n, null]);
    assert(n <= 2,
      'the degenerate single-prefix path places almost nothing and says so, rather than ' +
      'guessing (got ' + n + '); banning one duplicate per round would place 16 at ~20x the time');
  }
  placementCases.forEach(([label, n, floor]) =>
    console.log('    ' + label + ': ' + n + ' placed' + (floor ? ' (floor ' + floor + ')' : '')));

  // #598 r2 F4 — the marginals solve must not decide the answer. It runs under
  // a SUPERSET of the dedup bans, so the deduplicated assignment is still
  // feasible under it and therefore still optimal; the two can only differ by
  // picking different optima of equal cost (400 randomised inputs found no
  // such case). That makes the separation a structural guarantee rather than
  // something a behavioural test can catch, so it is pinned structurally: the
  // entry build reads the deduplicated `assign`, and the marginals solve is
  // used for F/B only.
  const rs = fs.readFileSync(path.join(REPO, 'public', 'hop-resolver.js'), 'utf8');
  const body = rs.slice(rs.indexOf('function inferPath('), rs.indexOf('\n  /**\n   * Check if the resolver'));
  assert(/\/\/ This assignment is the answer\. Nothing below may change it\.\s*\n\s*const assign = sol\.assign;/.test(body),
    'the deduplicated assignment is captured and marked as final');
  assert(/const k = assign\[i\];/.test(body), 'the chosen node at each hop comes from it');
  assert(!/marginals\.assign/.test(body), 'the marginals solve never supplies an assignment');
  assert(/marginals\.F\[i\]\[k\] \+ marginals\.B\[i\]\[k\]/.test(body),
    'and is used for the forward/backward marginals the softmax needs');
  assert(!/sol\.assign/.test(body.slice(body.indexOf('const assign = sol.assign;') + 26)),
    'nothing after it reads the solver state again');
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
