/**
 * #598 (port of upstream `Kpa-clawbot/CoreScope#2245`) — rendering and wiring
 * for the whole-path hop inference.
 *
 * The model itself is covered by test-issue-598-hop-path-inference.js. This
 * file covers what the UI does with it:
 *
 *   - `HopDisplay.renderHop()` renders an inferred hop (one with a numeric
 *     `confidence`) as an answer, a guess or an unlisted hop, and leaves
 *     `resolve()` entries rendering exactly as before.
 *   - `packets.js` renderPath() keys the detail hops BY POSITION from the
 *     inferred entries, so a prefix a path repeats can be two nodes.
 *   - renderDetail() infers the SELECTED observation's own path, renders
 *     without waiting for the neighbour graph, and redraws the hop rows when
 *     the first graph lands — dropping a redraw whose render is superseded.
 *   - the shared helpers in `packet-helpers.js`.
 *   - the map route view takes the same inferred hops instead of calling
 *     /api/resolve-hops per prefix, and shows an unlisted hop as `unlisted`.
 */
'use strict';
const fs = require('fs');
const vm = require('vm');
const path = require('path');
const assert = require('assert');

const REPO = __dirname;
const src = f => fs.readFileSync(path.join(REPO, f), 'utf8');

// A hang (for instance a renderDetail that awaits something unresolved) must
// not look like success: the exit code starts at 1 and is only cleared when
// the last assertion has run.
process.exitCode = 1;

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  PASS  ' + name); }
  catch (e) { failed++; console.log('  FAIL  ' + name + ': ' + e.message); }
}
async function testAsync(name, fn) {
  try { await fn(); passed++; console.log('  PASS  ' + name); }
  catch (e) { failed++; console.log('  FAIL  ' + name + ': ' + e.message); }
}

// ── A minimal element stub. Enough for innerHTML assertions and the handful
// of querySelector lookups the detail pane makes; the repo carries no jsdom.
function makeEl(tag) {
  const el = {
    tagName: String(tag || 'div').toUpperCase(), id: '', className: '', textContent: '',
    innerHTML: '', style: {}, dataset: {}, children: [],
    appendChild(c) { el.children.push(c); return c; },
    setAttribute() {}, removeAttribute() {}, addEventListener() {}, removeEventListener() {},
    getBoundingClientRect: () => ({ top: 0, left: 0, bottom: 0, right: 0 }),
    classList: { add() {}, remove() {}, contains: () => false, toggle() {} },
    querySelector: () => null, querySelectorAll: () => [],
    closest: () => null, remove() {},
  };
  return el;
}

function makeSandbox() {
  const registeredPages = {};
  const ctx = {
    window: { addEventListener() {}, removeEventListener() {}, dispatchEvent() {}, innerWidth: 1200,
      scrollX: 0, scrollY: 0, PacketFilter: null },
    document: {
      readyState: 'complete', createElement: makeEl, head: { appendChild() {} },
      body: { appendChild() {}, removeChild() {} },
      getElementById: () => null, addEventListener() {}, removeEventListener() {},
      querySelector: () => null, querySelectorAll: () => [],
    },
    console: { log() {}, warn() {}, error(...a) { console.error(...a); }, debug() {} },
    Date, Infinity, Math, Array, Object, String, Number, JSON, RegExp, Boolean,
    Error, TypeError, RangeError, parseInt, parseFloat, isNaN, isFinite,
    Float64Array, Int32Array, WeakMap, Map, Set, Promise, URLSearchParams,
    encodeURIComponent, decodeURIComponent,
    setTimeout: (fn) => { if (fn) fn(); return 0; }, clearTimeout() {},
    setInterval: () => 0, clearInterval() {},
    fetch: () => Promise.resolve({ ok: true, json: () => Promise.resolve({}) }),
    performance: { now: () => Date.now() },
    localStorage: (() => { const s = {}; return { getItem: k => (k in s ? s[k] : null), setItem(k, v) { s[k] = String(v); }, removeItem(k) { delete s[k]; } }; })(),
    location: { hash: '' }, history: { replaceState() {} },
    CustomEvent: class CustomEvent {},
    addEventListener() {}, removeEventListener() {}, dispatchEvent() {},
    requestAnimationFrame: (cb) => { cb(); return 0; },
    _registeredPages: registeredPages,
    registerPage: (name, handler) => { registeredPages[name] = handler; },
  };
  vm.createContext(ctx);
  return ctx;
}

function loadInCtx(ctx, file) {
  vm.runInContext(src(file), ctx, { filename: file });
  for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k];
}

// The whole hop chain, for real: resolver, filter, display, helpers, packets.
function loadPackets(opts) {
  opts = opts || {};
  const ctx = makeSandbox();
  loadInCtx(ctx, 'public/payload-labels.js');
  loadInCtx(ctx, 'public/roles.js');
  loadInCtx(ctx, 'public/app.js');
  loadInCtx(ctx, 'public/hop-resolver.js');
  loadInCtx(ctx, 'public/hop-filter.js');
  loadInCtx(ctx, 'public/hop-display.js');
  loadInCtx(ctx, 'public/packet-helpers.js');
  // app.js re-declares registerPage at top level (sloppy-mode function
  // declarations become globals), so re-apply the capture, as test-packets.js
  // and test-issue-608-observation-route.js do.
  ctx.registerPage = (name, handler) => { ctx._registeredPages[name] = handler; };
  if (opts.api) ctx.api = ctx.window.api = opts.api;
  loadInCtx(ctx, 'public/packets.js');
  if (opts.api) ctx.api = ctx.window.api = opts.api;
  return ctx;
}

// ── Geography, as in the model tests: a corridor due north along 5°E.
const KM_PER_DEG = 111.195;
const at = km => ({ lat: 50 + km / KM_PER_DEG, lon: 5 });
const pk = p => (p + '0').padEnd(64, '0');
function node(prefix, name, km, role) {
  const p = km === null ? { lat: null, lon: null } : at(km);
  return { public_key: pk(prefix), name: name, role: role || 'repeater', lat: p.lat, lon: p.lon };
}

console.log('\n=== #598 M: HopDisplay renders an inferred hop ===');

{
  const ctx = makeSandbox();
  loadInCtx(ctx, 'public/hop-display.js');
  const HopDisplay = ctx.window.HopDisplay;

  const confident = { hop: 'a0', name: 'SURE-ONE', pubkey: pk('a0'), located: true, unlisted: false,
    unreliable: false, ambiguous: false, confidence: 0.93, candidates: [], conflicts: [] };
  test('a confident hop shows its name with no badge', () => {
    const html = HopDisplay.renderHop('a0', confident, {});
    assert.ok(html.includes('SURE-ONE'), 'name rendered: ' + html);
    assert.ok(!html.includes('hop-conflict-btn'), 'no candidate badge: ' + html);
    assert.ok(/93% likely/.test(html), 'the tooltip states the likelihood: ' + html);
    assert.ok(!/hop-ambiguous/.test(html), 'not dashed');
  });

  const guess = { hop: 'a0', name: 'MAYBE-ONE', pubkey: pk('a0'), located: true, unlisted: false,
    unreliable: false, ambiguous: true, confidence: 0.41,
    candidates: [
      { name: 'MAYBE-ONE', pubkey: pk('a0'), located: true, confidence: 0.41 },
      { name: 'MAYBE-TWO', pubkey: pk('a0b'), located: true, confidence: 0.33 },
      { name: 'ELSEWHERE', pubkey: pk('a0c'), located: true, confidence: 0, usedAt: 4 },
    ] };
  guess.conflicts = guess.candidates;
  test('a hop below 70% is dashed and carries a badge to its candidates', () => {
    const html = HopDisplay.renderHop('a0', guess, {});
    assert.ok(/hop-ambiguous/.test(html), 'dashed underline class: ' + html);
    assert.ok(/hop-conflict-btn/.test(html), 'candidate badge: ' + html);
    assert.ok(/>3</.test(html), 'the badge counts all three candidates: ' + html);
    assert.ok(/3 nodes share this prefix/.test(html), 'tooltip says how many share the prefix');
  });
  test('the badge carries the inferred pick, so the popover can state it', () => {
    const html = HopDisplay.renderHop('a0', guess, {});
    const m = html.match(/data-conflict='([^']*)'/);
    assert.ok(m, 'data-conflict present: ' + html);
    const data = JSON.parse(m[1].replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&amp;/g, '&'));
    assert.strictEqual(data.pick.name, 'MAYBE-ONE');
    assert.ok(Math.abs(data.pick.confidence - 0.41) < 1e-9);
    assert.strictEqual(data.conflicts.length, 3);
  });
  test('opts.badge === false drops the badge (the packets list summarises per path)', () => {
    const html = HopDisplay.renderHop('a0', guess, { badge: false });
    assert.ok(!/hop-conflict-btn/.test(html), 'no badge: ' + html);
    assert.ok(/MAYBE-ONE/.test(html), 'still named');
  });
  test('opts.hexMode shows the prefix instead of the name', () => {
    const html = HopDisplay.renderHop('a0', confident, { hexMode: true });
    assert.ok(!/SURE-ONE/.test(html) && />a0</.test(html), html);
  });

  const unlisted = { hop: '5e', name: null, pubkey: null, located: false, unlisted: true,
    unreliable: true, ambiguous: true, confidence: 0.77,
    candidates: [{ name: 'REJECTED-FAR', pubkey: pk('5e'), located: true, confidence: 0.1 },
      { name: 'REJECTED-TOO', pubkey: pk('5eb'), located: true, confidence: 0.1 }] };
  unlisted.conflicts = unlisted.candidates;
  test('an unlisted hop shows its hex prefix in muted mono, not a node name', () => {
    const html = HopDisplay.renderHop('5e', unlisted, {});
    assert.ok(/hop-unlisted/.test(html), 'hop-unlisted class: ' + html);
    assert.ok(/>5e</.test(html), 'the prefix is the label: ' + html);
    assert.ok(!/REJECTED-FAR<\/a>|>REJECTED-FAR</.test(html), 'no node is presented as the answer');
    assert.ok(/no listed node with this prefix fits this point in the path/.test(html),
      'the tooltip explains what unlisted means: ' + html);
    assert.ok(/hop-conflict-btn/.test(html), 'the rejected candidates stay reachable: ' + html);
  });
  test('an unlisted hop with no candidates at all offers no badge', () => {
    const bare = Object.assign({}, unlisted, { candidates: [], conflicts: [], unreliable: false });
    const html = HopDisplay.renderHop('5e', bare, {});
    assert.ok(!/hop-conflict-btn/.test(html), 'nothing to weigh, so no badge: ' + html);
    assert.ok(/no known node has this prefix/.test(html), html);
  });

  const noGps = { hop: 'c7', name: 'NO-GPS-NODE', pubkey: pk('c7'), located: false, unlisted: true,
    unreliable: false, ambiguous: false, confidence: 0.8,
    candidates: [{ name: 'NO-GPS-NODE', pubkey: pk('c7'), located: false, confidence: null }] };
  noGps.conflicts = noGps.candidates;
  test('the only known node with a prefix, without GPS, is named and labelled', () => {
    const html = HopDisplay.renderHop('c7', noGps, {});
    assert.ok(/NO-GPS-NODE/.test(html), html);
    assert.ok(/hop-nogps">\(no GPS\)/.test(html), 'labelled (no GPS): ' + html);
    assert.ok(html.includes('#/nodes/' + pk('c7')), 'and still links to the node');
    assert.ok(!/hop-conflict-btn/.test(html), 'a named hop is not also offered as a choice');
  });

  test('a candidate name carrying markup is escaped', () => {
    const nasty = Object.assign({}, guess, {
      name: '<img src=x onerror=alert(1)>',
      candidates: [{ name: '<script>alert(1)</script>', pubkey: pk('a0'), located: true, confidence: 0.4 },
        { name: 'b', pubkey: pk('a0b'), located: true, confidence: 0.4 }],
    });
    nasty.conflicts = nasty.candidates;
    const html = HopDisplay.renderHop('<b>hop</b>', nasty, {});
    assert.ok(!/<img src=x/.test(html), 'the name is escaped: ' + html);
    assert.ok(!/<script>/.test(html), 'the candidate list is escaped: ' + html);
    assert.ok(!/<b>hop<\/b>/.test(html), 'the prefix is escaped: ' + html);
  });

  // #1283 — a hop the ingestor resolved is the answer, not a guess with odds.
  const canonical = { hop: 'b1', name: 'SERVER-SAYS', pubkey: pk('b1'), located: true, unlisted: false,
    unreliable: false, ambiguous: false, canonical: true, confidence: 1,
    candidates: [{ name: 'SERVER-SAYS', pubkey: pk('b1'), located: true, confidence: 1 }] };
  canonical.conflicts = canonical.candidates;
  test('a hop the ingestor resolved says so, and claims no likelihood', () => {
    const html = HopDisplay.renderHop('b1', canonical, {});
    assert.ok(/SERVER-SAYS/.test(html), html);
    assert.ok(/resolved by the ingestor/.test(html), 'the tooltip names the source: ' + html);
    assert.ok(!/% likely/.test(html), 'and invents no odds for an authoritative answer: ' + html);
    assert.ok(!/hop-conflict-btn|hop-unreliable-btn/.test(html), 'no badge: ' + html);
    assert.ok(!/hop-ambiguous/.test(html), 'and no dashed underline: ' + html);
  });

  // The other side: the ingestor's answer is kept and flagged, not replaced
  // and not mislabelled "(no GPS)" — the node has a position, it just does not
  // fit where the hop sits.
  const misplaced = { hop: 'b1', name: 'WRONG-REGION', pubkey: pk('b1'), lat: null, lon: null,
    located: false, unlisted: true, unreliable: true, ambiguous: false, confidence: 0.9,
    candidates: [{ name: 'WRONG-REGION', pubkey: pk('b1'), located: true, confidence: 0 }] };
  misplaced.conflicts = misplaced.candidates;
  test('a hop the ingestor resolved that does not fit keeps its name and the #165 warning', () => {
    const html = HopDisplay.renderHop('b1', misplaced, {});
    assert.ok(/WRONG-REGION/.test(html), 'the ingestor\'s answer is still what the hop is called: ' + html);
    assert.ok(/hop-unreliable-btn/.test(html), 'flagged with the existing unreliable warning: ' + html);
    assert.ok(/hop-unreliable\b/.test(html), 'and the pill carries the unreliable class: ' + html);
    assert.ok(!/hop-unlisted/.test(html),
      'but not hop-unlisted, which mutes a hex prefix and this hop shows a name: ' + html);
    assert.ok(!/\(no GPS\)/.test(html), 'it is NOT labelled no GPS — it has a position: ' + html);
    assert.ok(/does not fit this point in the path/.test(html), 'the tooltip says why: ' + html);
    assert.ok(!/hop-conflict-btn/.test(html), 'and offers no candidate list in its place: ' + html);
  });

  test('routeHopFromInferred keeps a misplaced ingestor answer off the "no GPS" chip', () => {
    const ctx2 = makeSandbox();
    loadInCtx(ctx2, 'public/packet-helpers.js');
    const h = ctx2.window.routeHopFromInferred('b1', misplaced, 'repeater');
    assert.strictEqual(h.name, 'WRONG-REGION', 'the name survives into the route sidebar');
    assert.strictEqual(h.resolved, false);
    assert.strictEqual(h.unlisted, true, 'the chip reads "unlisted", which is what happened');
    assert.ok(!h.gpsless, 'and never "no GPS": the node has coordinates');
    assert.ok(h.lat === undefined && h.lon === undefined, 'with nothing to draw');
  });

  test('a resolve() entry (no numeric confidence) renders exactly as before', () => {
    const legacy = { name: 'OLD-WAY', pubkey: pk('b1'), ambiguous: true, globalFallback: true,
      conflicts: [{ name: 'OLD-WAY', pubkey: pk('b1'), distKm: 12 }, { name: 'OTHER', pubkey: pk('b1b'), distKm: 44 }] };
    const html = HopDisplay.renderHop('b1', legacy, {});
    assert.ok(/OLD-WAY/.test(html) && /hop-conflict-btn/.test(html), html);
    assert.ok(!/hop-unlisted/.test(html), 'no inferred-only class leaks in');
    const m = html.match(/data-conflict='([^']*)'/);
    const data = JSON.parse(m[1].replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&amp;/g, '&'));
    assert.strictEqual(data.pick, null, 'a resolve() badge carries no inferred pick');
  });

  test('the Path symbols legend explains unlisted hops', () => {
    const items = HopDisplay.PATH_SYMBOLS_LEGEND;
    const entry = items.find(e => /unlisted/.test(e.description));
    assert.ok(entry, 'legend has an unlisted entry: ' + JSON.stringify(items.map(e => e.description)));
    assert.ok(/hop-unlisted/.test(entry.glyphHtml), 'and its glyph is the real class: ' + entry.glyphHtml);
    const html = HopDisplay.renderPathSymbolsLegend();
    assert.ok(/unlisted/.test(html), 'and it reaches the rendered legend');
  });

  test('HopDisplay and HopResolver agree on what counts as likely', () => {
    const rs = makeSandbox();
    loadInCtx(rs, 'public/hop-resolver.js');
    assert.strictEqual(HopDisplay.LIKELY, rs.window.HopResolver.LIKELY_CONFIDENCE);
  });

  test('the popover states the inferred pick and each candidate\'s odds', () => {
    const appended = [];
    ctx.document.body.appendChild = el => { appended.push(el); return el; };
    const btn = makeEl('button');
    HopDisplay._showConflictPopover(btn, 'a0', guess.candidates, false, { name: 'MAYBE-ONE', confidence: 0.41 });
    const html = appended[appended.length - 1].innerHTML;
    assert.ok(/likely MAYBE-ONE \(41%\)/.test(html), 'header names the pick: ' + html);
    assert.ok(/33%/.test(html), 'an alternative shows its likelihood: ' + html);
    assert.ok(/hop 4/.test(html), 'a candidate the path uses elsewhere says where: ' + html);
  });

  test('the popover for an unlisted hop says so', () => {
    const appended = [];
    ctx.document.body.appendChild = el => { appended.push(el); return el; };
    HopDisplay._showConflictPopover(makeEl('button'), '5e', unlisted.candidates, false,
      { name: null, confidence: 0.77 });
    const html = appended[appended.length - 1].innerHTML;
    assert.ok(/unlisted node \(77%\)/.test(html), html);
  });

  test('the popover shows "no GPS" for a candidate without a position', () => {
    const appended = [];
    ctx.document.body.appendChild = el => { appended.push(el); return el; };
    HopDisplay._showConflictPopover(makeEl('button'), 'c7',
      [{ name: 'A', pubkey: pk('c7'), located: true, confidence: 0.5 },
        { name: 'B', pubkey: pk('c7b'), located: false, confidence: null }],
      false, { name: 'A', confidence: 0.5 });
    const html = appended[appended.length - 1].innerHTML;
    assert.ok(/no GPS/.test(html), html);
  });
}

console.log('\n=== #598 N: the detail Path row keys hops by POSITION ===');

// "10" at hops 0 and 2 — the shape fixture observation 147 has three times
// over. The per-prefix cache cannot hold two answers for it; the inferred
// entries can.
const NODES_POS = [
  node('10', 'TEN-SOUTH', 0), node('10b', 'TEN-NORTH', 60),
  node('b1', 'MIDDLE', 30), node('c2', 'END', 90),
];
const POS_HOPS = ['10', 'b1', '10', 'c2'];

{
  const ctx = loadPackets();
  const api = ctx._packetsTestAPI;
  ctx.HopResolver.init(NODES_POS);
  ctx.HopResolver.setAffinity({});
  const entries = ctx.HopResolver.inferPath(POS_HOPS, { observer: at(120), origin: at(0) });

  test('fixture: the model really does answer the repeated prefix differently', () => {
    assert.notStrictEqual(entries[0].pubkey, entries[2].pubkey,
      'hop 0 and hop 2 resolve to different nodes: ' + JSON.stringify([entries[0].name, entries[2].name]));
    assert.strictEqual(entries[0].name, 'TEN-SOUTH');
    assert.strictEqual(entries[2].name, 'TEN-NORTH');
  });

  test('renderPath with entries renders each position from its own entry', () => {
    const html = api.renderPath(POS_HOPS, 'OBS-1', { entries: entries });
    assert.ok(html.indexOf('TEN-SOUTH') < html.indexOf('TEN-NORTH'),
      'both names appear, in path order: ' + html);
    assert.strictEqual((html.match(/TEN-SOUTH/g) || []).length, 1, 'TEN-SOUTH once: ' + html);
    assert.strictEqual((html.match(/TEN-NORTH/g) || []).length, 1, 'TEN-NORTH once: ' + html);
  });

  test('without entries the same path still falls back to the per-prefix lookup', () => {
    const html = api.renderPath(POS_HOPS, 'OBS-1', {});
    assert.ok(!/TEN-SOUTH[\s\S]*TEN-NORTH/.test(html),
      'the per-prefix path cannot answer the repeat twice (that is the bug): ' + html);
  });

  test('an entries array of the wrong length is ignored, not misaligned', () => {
    // Without the length guard the extra positions read `undefined` and the
    // first two still render inferred names: a path half from one source.
    const short = api.renderPath(POS_HOPS, 'OBS-1', { entries: entries.slice(0, 2) });
    const none = api.renderPath(POS_HOPS, 'OBS-1', {});
    assert.strictEqual(short, none,
      'a short entries array renders exactly as no entries at all:\n  ' + short + '\n  ' + none);
  });

  test('summary mode (the packets list) does not use the inferred entries', () => {
    const html = api.renderPath(POS_HOPS, 'OBS-1', { entries: entries, summary: true });
    assert.ok(!/TEN-NORTH/.test(html),
      'the list keeps its per-prefix summary, so one row cannot cost a path inference: ' + html);
  });

  test('with the 1-byte filter ON the remaining hops keep their OWN entries', () => {
    // "c2" is 1-byte too; use a mixed path so something survives the filter.
    const mixed = ['10', 'b1b1', '10'];
    const mixedNodes = [node('10', 'TEN-SOUTH', 0), node('10b', 'TEN-NORTH', 60),
      { public_key: 'b1b1' + '0'.repeat(60), name: 'TWO-BYTE', role: 'repeater', lat: at(30).lat, lon: at(30).lon }];
    ctx.HopResolver.init(mixedNodes);
    const e = ctx.HopResolver.inferPath(mixed, { observer: at(90), origin: at(0) });
    ctx.localStorage.setItem('meshcore-hide-1byte-hops', 'true');
    try {
      const html = api.renderPath(mixed, 'OBS-1', { entries: e });
      assert.ok(/TWO-BYTE/.test(html), 'the 2-byte hop renders: ' + html);
      assert.ok(!/TEN-SOUTH|TEN-NORTH/.test(html), 'the 1-byte hops are hidden: ' + html);
    } finally {
      ctx.localStorage.removeItem('meshcore-hide-1byte-hops');
      ctx.HopResolver.init(NODES_POS);
    }
  });
}

console.log('\n=== #598 O: renderDetail infers the SELECTED observation\'s path ===');

// Two observations of one packet, heard at opposite ends of the corridor, with
// DIFFERENT paths. Picking the wrong observation's path or the wrong observer
// gives a different Path row, so the assertions below can tell them apart.
const OBSERVER_SOUTH = pk('5501');
const OBSERVER_NORTH = pk('5502');
function detailData() {
  return {
    packet: {
      id: 1, hash: 'pkt598', payload_type: 5, route_type: 1,
      timestamp: '2026-01-01T00:00:00Z', decoded_json: '{}',
      // The packet row's longest path AND its own observer: the aggregate row.
      path_json: JSON.stringify(['10', 'b1', '10', 'c2']),
      resolved_path: null, observer_id: OBSERVER_NORTH,
    },
    observations: [
      // Each carries its OWN received-flood frame — header 0x15 (payload
      // GRP_TXT, route FLOOD), a path-length byte of hash_size 1 and its hop
      // count, its hop bytes, then payload. Without that the observer anchor
      // is withheld (#165/#408/#608), which section R covers directly.
      { id: 10, observer_id: OBSERVER_SOUTH, timestamp: '2026-01-01T00:00:00Z',
        path_json: JSON.stringify(['10', 'b1', '10']), resolved_path: null, snr: 1, rssi: -90,
        direction: 'rx', raw_hex: '15' + '03' + '10b110' + 'AABB' },
      { id: 11, observer_id: OBSERVER_NORTH, timestamp: '2026-01-01T00:00:01Z',
        path_json: JSON.stringify(['10', 'b1', '10', 'c2']), resolved_path: null, snr: 2, rssi: -80,
        direction: 'rx', raw_hex: '15' + '04' + '10b110c2' + 'AABB' },
    ],
    observation_count: 2,
  };
}

function detailCtx(graph) {
  const calls = [];
  const nodes = NODES_POS.concat([
    // 900 km up the corridor: a node the model would never place on this
    // path, so a hint that reaches it is visible in the output.
    node('d9', 'FAR-DECOY', 900),
    { public_key: OBSERVER_SOUTH, name: 'OBS-SOUTH', role: 'repeater', lat: at(-30).lat, lon: at(-30).lon },
    { public_key: OBSERVER_NORTH, name: 'OBS-NORTH', role: 'repeater', lat: at(120).lat, lon: at(120).lon },
  ]);
  const observers = [
    { id: OBSERVER_SOUTH, name: 'OBS-SOUTH', iata: null, lat: at(-30).lat, lon: at(-30).lon },
    { id: OBSERVER_NORTH, name: 'OBS-NORTH', iata: null, lat: at(120).lat, lon: at(120).lon },
  ];
  const api = async (p) => {
    calls.push(p);
    if (p.startsWith('/nodes?') || p === '/nodes') return { nodes: nodes, total: nodes.length };
    if (p === '/observers') return { observers: observers };
    if (p === '/iata-coords') return { coords: {} };
    if (p === '/analytics/neighbor-graph') return graph ? graph() : { nodes: [], edges: [] };
    return {};
  };
  const ctx = loadPackets({ api: api });
  ctx._packetsTestAPI._setObservers(observers);
  ctx._calls = calls;
  return ctx;
}

// A panel stub that records what was written where.
function makePanel() {
  const slots = {};
  const hopNames = [];
  const panel = {
    innerHTML: '',
    querySelector(sel) {
      if (sel === '.detail-path-hops' || sel === '.detail-aggregate-hops') {
        if (!slots[sel]) {
          const writes = [];
          slots[sel] = {
            writes: writes,
            get innerHTML() { return writes.length ? writes[writes.length - 1] : '__initial__'; },
            set innerHTML(v) { writes.push(v); },
          };
        }
        return slots[sel];
      }
      return null;
    },
    querySelectorAll(sel) {
      if (sel === '.detail-hop-name') return hopNames;
      return [];
    },
    _slots: slots, _hopNames: hopNames,
  };
  return panel;
}

// Let every pending microtask/immediate settle: the graph lands through a
// promise chain several hops deep, and the sandbox's setTimeout is a stub.
const flush = async () => { for (let i = 0; i < 20; i++) await new Promise(r => setImmediate(r)); };

async function runDetail(ctx, obsId, panel) {
  const data = detailData();
  await ctx._packetsTestAPI.renderDetail(panel || makePanel(), data, obsId);
  return panel;
}

(async () => {

await testAsync('the Path row shows the SELECTED observation\'s own hops, anchored on ITS observer', async () => {
  const ctx = detailCtx();
  const south = makePanel();
  await runDetail(ctx, 10, south);
  const north = makePanel();
  await runDetail(ctx, 11, north);

  const pathOf = html => {
    const m = html.match(/<dt>Path<\/dt><dd>([\s\S]*?)<\/dd>/);
    assert.ok(m, 'Path row present: ' + html.slice(0, 400));
    return m[1];
  };
  const sPath = pathOf(south.innerHTML), nPath = pathOf(north.innerHTML);
  assert.ok(/3 hops/.test(sPath), 'the southern observation has 3 hops: ' + sPath);
  assert.ok(/4 hops/.test(nPath), 'the northern observation has 4 hops: ' + nPath);
  assert.ok(/detail-path-hops/.test(sPath), 'the hops sit in their own span, for the later redraw');
  assert.ok(/TEN-SOUTH/.test(sPath) && /TEN-NORTH/.test(sPath),
    'the repeated "10" prefix is two different nodes in one path: ' + sPath);
  assert.notStrictEqual(sPath.replace(/\s+/g, ''), nPath.replace(/\s+/g, ''),
    'the two observations do not render the same path');
});

await testAsync('the observation\'s own resolved_path is the hint, not the packet row\'s', async () => {
  const ctx = detailCtx();
  const data = detailData();
  // The packet row pins hop 0 to a node 900 km away; the shown observation
  // claims nothing. Both paths are four hops long, so a leak would not be
  // caught by the length guard — only by reading the right row. (Upstream's
  // own bug was exactly this: the detail pane read the packet row's answer.)
  data.packet.resolved_path = JSON.stringify([pk('d9'), null, null, null]);
  const panel = makePanel();
  await ctx._packetsTestAPI.renderDetail(panel, data, 11);
  const m = panel.innerHTML.match(/<dt>Path<\/dt><dd>([\s\S]*?)<\/dd>/);
  assert.ok(!/FAR-DECOY/.test(m[1]),
    'the packet row\'s hint did not reach the shown observation: ' + m[1]);
  assert.ok(/TEN-(SOUTH|NORTH)/.test(m[1]),
    'hop 0 is inferred from the observation\'s own path: ' + m[1]);

  // Sanity: the same hint ON the observation does reach it, so the assertion
  // above is about which row was read, not about hints being ignored.
  const data2 = detailData();
  data2.observations[1].resolved_path = JSON.stringify([pk('d9'), null, null, null]);
  const panel2 = makePanel();
  await ctx._packetsTestAPI.renderDetail(panel2, data2, 11);
  const m2 = panel2.innerHTML.match(/<dt>Path<\/dt><dd>([\s\S]*?)<\/dd>/);
  assert.ok(/FAR-DECOY|hop-unlisted/.test(m2[1]),
    'the observation\'s own hint is applied (and may still be overruled as unlisted): ' + m2[1]);
});

await testAsync('a non-null resolved_path entry on the shown observation IS used', async () => {
  const ctx = detailCtx();
  const data = detailData();
  data.observations[0].resolved_path = JSON.stringify([pk('10b'), pk('b1'), pk('10')]);
  const panel = makePanel();
  await ctx._packetsTestAPI.renderDetail(panel, data, 10);
  const m = panel.innerHTML.match(/<dt>Path<\/dt><dd>([\s\S]*?)<\/dd>/);
  assert.ok(m[1].indexOf('TEN-NORTH') < m[1].indexOf('TEN-SOUTH'),
    'the observation\'s own hint puts TEN-NORTH at hop 0: ' + m[1]);
});

await testAsync('the byte breakdown and the Path row name the same node at every hop', async () => {
  const ctx = detailCtx();
  const data = detailData();
  const panel = makePanel();
  await ctx._packetsTestAPI.renderDetail(panel, data, 10);
  const html = panel.innerHTML;
  const hopRows = [...html.matchAll(/Hop (\d) — <span class="detail-hop-name" data-hop-idx="(\d)">([\s\S]*?)<\/span>/g)];
  assert.strictEqual(hopRows.length, 3, 'three hop rows in the byte table: ' + html.slice(0, 200));
  hopRows.forEach((r, i) => assert.strictEqual(r[2], String(i), 'each row carries its own index'));

  // The two views must agree hop for hop. Both repeat the "10" prefix, so
  // reading either from the per-prefix cache would name one node twice.
  const pathRow = html.match(/<dt>Path<\/dt><dd>([\s\S]*?)<\/dd>/)[1];
  const names = s => [...s.matchAll(/data-hop-link="true">([^<]*)</g)].map(m => m[1]);
  const fromPath = names(pathRow);
  const fromBytes = hopRows.map(r => names(r[3])[0]);
  assert.deepStrictEqual(fromBytes, fromPath,
    'byte table ' + JSON.stringify(fromBytes) + ' vs Path row ' + JSON.stringify(fromPath));
  assert.strictEqual(new Set(fromPath).size, fromPath.length,
    'and the repeated "10" prefix is two different nodes: ' + JSON.stringify(fromPath));
});

await testAsync('the cross-observer aggregate row renders the packet\'s own longest path', async () => {
  const ctx = detailCtx();
  const panel = makePanel();
  await runDetail(ctx, 10, panel);
  const m = panel.innerHTML.match(/Longest observed path: ([\s\S]*?)<\/div>/);
  assert.ok(m, 'aggregate row present');
  assert.ok(/detail-aggregate-hops/.test(m[1]), 'its hops sit in their own span: ' + m[1]);
  assert.ok(/4 hops/.test(m[1]), 'four hops, from the packet row not the observation: ' + m[1]);
});

await testAsync('the detail pane never writes an observer-specific pick into the list\'s prefix cache', async () => {
  const ctx = detailCtx();
  await runDetail(ctx, 10);
  const after10 = ctx._packetsTestAPI._hopCacheGet('10');
  await runDetail(ctx, 11);
  const after11 = ctx._packetsTestAPI._hopCacheGet('10');
  assert.deepStrictEqual(after10, after11,
    'the shared cache entry for "10" is the same whichever observation was opened — the pane ' +
    'only ever writes the context-free guess the packets list would compute itself');
});

console.log('\n=== #598 P: the neighbour graph is evidence, not a prerequisite ===');

await testAsync('the pane renders before the graph lands, then redraws its hop rows', async () => {
  let release;
  const pending = new Promise(r => { release = r; });
  const ctx = detailCtx(() => pending);
  const panel = makePanel();

  // The render must not be waiting on the graph: it has to win this race,
  // which it cannot do if it awaits the fetch.
  const rendered = runDetail(ctx, 10, panel).then(() => 'rendered');
  const late = new Promise(r => setTimeout(() => r('still waiting for the graph'), 250));
  assert.strictEqual(await Promise.race([rendered, late]), 'rendered',
    'renderDetail returned before the neighbour graph landed');

  assert.ok(/<dt>Path<\/dt>/.test(panel.innerHTML),
    'the pane is complete while the graph is still in flight');
  assert.ok(/TEN-/.test(panel.innerHTML), 'with real hop names, inferred without the graph');
  assert.strictEqual(panel._slots['.detail-path-hops'], undefined,
    'and nothing has been redrawn yet');

  // The graph links MIDDLE to TEN-NORTH, which the model weighs at hop 0.
  release({ edges: [{ source: pk('b1'), target: pk('10b'), score: 5 }] });
  await flush();

  const slot = panel._slots['.detail-path-hops'];
  assert.ok(slot, 'the Path row\'s hop span was redrawn once the graph arrived');
  assert.ok(/TEN-/.test(slot.innerHTML), 'with real hops: ' + slot.innerHTML);
});

await testAsync('a redraw whose render has been superseded is dropped', async () => {
  let release;
  const pending = new Promise(r => { release = r; });
  const ctx = detailCtx(() => pending);
  const panel = makePanel();
  await runDetail(ctx, 10, panel);      // schedules the redraw
  await runDetail(ctx, 11, panel);      // same panel, another observation
  release({ edges: [] });
  await flush();
  assert.ok(/4 hops/.test(panel.innerHTML), 'the second render is the one on screen');
  const slot = panel._slots['.detail-path-hops'];
  const writes = slot ? slot.writes : [];
  assert.strictEqual(writes.length, 1,
    'the superseded render\'s redraw is dropped: exactly one redraw reached the Path row, got ' +
    writes.length + ' (' + writes.map(w => (w.match(/data-hop-link/g) || []).length + ' hops').join(', ') + ')');
  assert.strictEqual((writes[0].match(/data-hop-link="true"/g) || []).length, 4,
    'and it is the current 4-hop render\'s');
});

await testAsync('once a graph is loaded, a render schedules no redraw at all', async () => {
  const ctx = detailCtx(() => Promise.resolve({ edges: [] }));
  const first = makePanel();
  await runDetail(ctx, 10, first);
  await flush();
  const second = makePanel();
  await runDetail(ctx, 10, second);
  await flush();
  assert.strictEqual(second._slots['.detail-path-hops'], undefined,
    'no in-place redraw when the graph was already there');
});

await testAsync('the neighbour graph is fetched once per page, not per render', async () => {
  const ctx = detailCtx(() => Promise.resolve({ edges: [] }));
  await runDetail(ctx, 10);
  await runDetail(ctx, 11);
  await runDetail(ctx, 10);
  const n = ctx._calls.filter(c => c === '/analytics/neighbor-graph').length;
  assert.strictEqual(n, 1, 'one neighbour-graph request for three renders, got ' + n);
});

await testAsync('no /api/resolve-hops request is made for the detail pane', async () => {
  const ctx = detailCtx();
  await runDetail(ctx, 10);
  assert.ok(!ctx._calls.some(c => /resolve-hops/.test(c)),
    'hop resolution is client-side: ' + JSON.stringify(ctx._calls));
});

console.log('\n=== #598 Q: refreshDetailHops touches only the hop rows ===');

test('refreshDetailHops rewrites the Path row, the aggregate row and the byte hop names', () => {
  const ctx = loadPackets();
  ctx.HopResolver.init(NODES_POS);
  ctx.HopResolver.setAffinity({});
  const entries = ctx.HopResolver.inferPath(POS_HOPS, { observer: at(120), origin: at(0) });
  const panel = makePanel();
  const el0 = { dataset: { hopIdx: '0' }, innerHTML: '' };
  const el2 = { dataset: { hopIdx: '2' }, innerHTML: '' };
  panel._hopNames.push(el0, el2);

  ctx._packetsTestAPI.refreshDetailHops(panel, {
    pathHops: POS_HOPS, observerId: 'OBS-1', packet: null, entries: entries,
    aggregatePath: POS_HOPS, aggregateObserverId: 'OBS-1', aggregatePacket: null,
    aggregateEntries: entries,
  });
  assert.ok(/TEN-SOUTH/.test(panel._slots['.detail-path-hops'].innerHTML));
  assert.ok(/TEN-SOUTH/.test(panel._slots['.detail-aggregate-hops'].innerHTML));
  assert.ok(/TEN-SOUTH/.test(el0.innerHTML), 'hop 0 name redrawn: ' + el0.innerHTML);
  assert.ok(/TEN-NORTH/.test(el2.innerHTML), 'hop 2 name redrawn from its OWN entry: ' + el2.innerHTML);
  assert.strictEqual(panel.innerHTML, '', 'the rest of the pane is untouched');
});

test('refreshDetailHops leaves the aggregate row alone when there is none', () => {
  const ctx = loadPackets();
  ctx.HopResolver.init(NODES_POS);
  const entries = ctx.HopResolver.inferPath(POS_HOPS, { observer: at(120) });
  const panel = makePanel();
  ctx._packetsTestAPI.refreshDetailHops(panel, {
    pathHops: POS_HOPS, observerId: 'OBS-1', entries: entries, aggregateEntries: null,
  });
  assert.strictEqual(panel._slots['.detail-aggregate-hops'].innerHTML, '__initial__');
});

console.log('\n=== #598 R: the shared helpers ===');

// A frame's header byte: payload type in bits 5-2, route in bits 1-0.
const header = (payload, route) => ((payload << 2) | route).toString(16).padStart(2, '0');
const RX_FLOOD = { direction: 'rx', raw_hex: header(5, 1) + '021011' };

function helperCtx() {
  const ctx = makeSandbox();
  const seen = [];
  ctx.window.HopResolver = { inferPath: (hops, opts) => { seen.push({ hops, opts }); return []; } };
  loadInCtx(ctx, 'public/packet-helpers.js');
  return { ctx: ctx, seen: seen };
}

test('inferObservationPath passes the observation\'s own path, hint and observer', () => {
  const { ctx, seen } = helperCtx();
  const obs = Object.assign({ observer_id: 'OBS-9', path_json: '["10","b1"]',
    resolved_path: '["' + pk('10') + '",null]' }, RX_FLOOD);
  ctx.window.inferObservationPath(obs, { lat: 51, lon: 5 }, { lat: 50, lon: 5 });
  assert.strictEqual(seen.length, 1);
  assert.deepStrictEqual(seen[0].hops, ['10', 'b1']);
  assert.deepStrictEqual(seen[0].opts.observer, { lat: 51, lon: 5 });
  assert.deepStrictEqual(seen[0].opts.origin, { lat: 50, lon: 5 });
  assert.strictEqual(seen[0].opts.observerId, 'OBS-9');
  assert.deepStrictEqual(seen[0].opts.resolvedPath, [pk('10'), null]);
});

// #165/#408/#608 — the observer anchors the end of the path only when THIS
// observation's own frame proves it received a flood. Both the detail pane and
// the map deep link go through this one helper, so neither can borrow receiver
// evidence the other would refuse.
test('the observer anchor is withheld unless the observation\'s own frame proves a received flood', () => {
  const cases = [
    ['a received flood', RX_FLOOD, true],
    ['a transmitted observation', { direction: 'tx', raw_hex: header(5, 1) + '021011' }, false],
    ['no direction at all', { raw_hex: header(5, 1) + '021011' }, false],
    ['a transport-direct frame', { direction: 'rx', raw_hex: header(5, 3) + 'AABBCCDD021011' }, false],
    ['a transport-flood frame', { direction: 'rx', raw_hex: header(5, 0) + 'AABBCCDD021011' }, false],
    ['a direct frame', { direction: 'rx', raw_hex: header(5, 2) + '021011' }, false],
    ['a TRACE, whose path is planned', { direction: 'rx', raw_hex: header(9, 1) + '021011' }, false],
    ['no frame of its own', { direction: 'rx' }, false],
    ['a frame that is not hex', { direction: 'rx', raw_hex: 'zz' }, false],
    // "1" is a valid hex digit: read as a byte it would say route FLOOD.
    ['a frame of a single nibble', { direction: 'rx', raw_hex: '1' }, false],
    ['a non-string raw_hex', { direction: 'rx', raw_hex: 21 }, false],
  ];
  for (const [label, frame, anchored] of cases) {
    const { ctx, seen } = helperCtx();
    const obs = Object.assign({ observer_id: 'OBS-9', path_json: '["10","b1"]' }, frame);
    ctx.window.inferObservationPath(obs, { lat: 51, lon: 5 }, { lat: 50, lon: 5 });
    assert.strictEqual(seen[0].opts.observer !== null, anchored, label + ': observer anchor');
    assert.strictEqual(seen[0].opts.observerId !== null, anchored,
      label + ': the IATA-centre fallback is gated the same way');
    assert.deepStrictEqual(seen[0].opts.origin, { lat: 50, lon: 5 },
      label + ': the sender anchor is unaffected — it comes from the payload, not the receiver');
  }
});

test('a frame carried only as a display fallback is not provenance', () => {
  const { ctx, seen } = helperCtx();
  // packets.js observationPacket() spreads the aggregate packet first, so an
  // observation with no raw_hex of its own still has one to display.
  const row = Object.assign({ observer_id: 'OBS-9', path_json: '["10"]' }, RX_FLOOD);
  ctx.window.inferObservationPath(row, { lat: 51, lon: 5 }, null, { direction: 'rx' });
  assert.strictEqual(seen[0].opts.observer, null,
    'the explicit frame decides, so the parent packet\'s bytes cannot anchor an observation');
});

test('renderDetail reads provenance from the observation, never from the spread row', () => {
  const pktSrc = src('public/packets.js');
  assert.ok(/inferFor\(effectivePkt, currentObs \|\| pkt\)/.test(pktSrc),
    'the Path row passes the selected observation as the frame source');
  assert.ok(/inferFor\(pkt, pkt\)/.test(pktSrc),
    'and the aggregate row passes the packet row as its own');
});

test('routeHopFromInferred: a located hop becomes a resolved route hop', () => {
  const ctx = makeSandbox();
  loadInCtx(ctx, 'public/packet-helpers.js');
  const h = ctx.window.routeHopFromInferred('10', {
    name: 'TEN-SOUTH', pubkey: pk('10'), lat: 50, lon: 5, located: true, confidence: 0.9,
  }, 'repeater');
  // Built inside the sandbox realm, so compare fields rather than prototypes.
  assert.deepStrictEqual(Object.assign({}, h), { lat: 50, lon: 5, name: 'TEN-SOUTH',
    pubkey: pk('10'), role: 'repeater', resolved: true, confidence: 0.9 });
});

test('routeHopFromInferred: a named hop without GPS becomes a gpsless route hop', () => {
  const ctx = makeSandbox();
  loadInCtx(ctx, 'public/packet-helpers.js');
  const h = ctx.window.routeHopFromInferred('c7', { name: 'NO-GPS', pubkey: pk('c7'), located: false });
  assert.strictEqual(h.resolved, false);
  assert.strictEqual(h.gpsless, true);
  assert.strictEqual(h.name, 'NO-GPS');
  assert.ok(!h.unlisted, 'a node we can name is not unlisted');
});

test('routeHopFromInferred: an unlisted hop becomes a placeholder, not a nearby node', () => {
  const ctx = makeSandbox();
  loadInCtx(ctx, 'public/packet-helpers.js');
  const h = ctx.window.routeHopFromInferred('5e', {
    name: null, pubkey: null, located: false, unlisted: true,
    candidates: [{ name: 'REJECTED', pubkey: pk('5e'), lat: 50, lon: 5, located: true }],
  });
  assert.strictEqual(h.unlisted, true);
  assert.strictEqual(h.resolved, false);
  assert.strictEqual(h.pubkey, '5e', 'it carries the prefix, not a candidate\'s pubkey');
  assert.ok(h.lat === undefined && h.lon === undefined, 'and no position to draw');
});

await testAsync('ensureHopAffinity loads once, refreshes after the TTL, and keeps the old graph on failure', async () => {
  const ctx = makeSandbox();
  let now = 1000000;
  ctx.Date = Object.assign(function () {}, Date, { now: () => now });
  const applied = [];
  ctx.window.HopResolver = { setAffinity: g => applied.push(g) };
  let calls = 0, fail = false;
  ctx.api = ctx.window.api = () => {
    calls++;
    return fail ? Promise.reject(new Error('503')) : Promise.resolve({ edges: [{ id: calls }] });
  };
  loadInCtx(ctx, 'public/packet-helpers.js');
  const ensure = ctx.window.ensureHopAffinity, has = ctx.window.hasHopAffinity;

  assert.strictEqual(has(), false, 'no graph before the first call');
  assert.strictEqual(await ensure(), true);
  assert.strictEqual(await ensure(), true);
  assert.strictEqual(calls, 1, 'two calls inside the TTL share one request, got ' + calls);
  assert.strictEqual(has(), true);
  assert.strictEqual(applied.length, 1);

  now += 299999;
  await ensure();
  assert.strictEqual(calls, 1, 'still inside the 5-minute TTL');

  now += 2;
  await ensure();
  assert.strictEqual(calls, 2, 'past the TTL it refreshes');
  assert.deepStrictEqual(applied[1], { edges: [{ id: 2 }] });

  now += 300001;
  fail = true;
  assert.strictEqual(await ensure(), false, 'a failed refresh reports false');
  assert.strictEqual(has(), true, 'and keeps the graph already loaded');
  assert.strictEqual(applied.length, 2, 'without applying anything new');
  fail = false;
  assert.strictEqual(await ensure(), true, 'the next call retries immediately, without waiting out the TTL');
  assert.strictEqual(calls, 4);
});

await testAsync('ensureHopAffinity asks for the shape the server keeps cached', async () => {
  const ctx = makeSandbox();
  const asked = [];
  ctx.window.HopResolver = { setAffinity() {} };
  ctx.api = ctx.window.api = (p, o) => { asked.push([p, o]); return Promise.resolve({ edges: [] }); };
  loadInCtx(ctx, 'public/packet-helpers.js');
  await ctx.window.ensureHopAffinity();
  assert.strictEqual(asked[0][0], '/analytics/neighbor-graph',
    'the default query shape, which /api/analytics/neighbor-graph serves from its 5-minute snapshot');
  assert.strictEqual(asked[0][1].ttl, 300000, 'cached client-side for the same 5 minutes');
});

console.log('\n=== #598 S: the map route view takes the same inferred hops ===');

{
  const mapSrc = src('public/map.js');

  test('drawPacketRoute skips /api/resolve-hops when the hops were inferred', () => {
    const body = mapSrc.slice(mapSrc.indexOf('async function drawPacketRoute(hopKeys'),
      mapSrc.indexOf('async function drawPacketRouteMulti'));
    assert.ok(/opts\.inferredHops && opts\.inferredHops\.length === hopKeys\.length/.test(body),
      'it reads opts.inferredHops and checks the length');
    const fetchIdx = body.indexOf("'/api/resolve-hops?hops='");
    assert.ok(fetchIdx > 0, 'the resolve-hops call is still there for the un-inferred path');
    const guardIdx = body.lastIndexOf('if (!inferred) {', fetchIdx);
    assert.ok(guardIdx > 0 && guardIdx < fetchIdx, 'and sits behind the !inferred guard');
    assert.ok(/const raw = inferred \? inferredRouteHops\(hopKeys, inferred\)/.test(body),
      'the hops it draws come from the inferred entries');
  });

  test('drawPacketRouteMulti does the same for the canonical path', () => {
    const body = mapSrc.slice(mapSrc.indexOf('async function drawPacketRouteMulti'));
    assert.ok(/opts\.inferredHops && opts\.inferredHops\.length === canonicalPath\.length/.test(body));
    const fetchIdx = body.indexOf("'/api/resolve-hops?hops='");
    assert.ok(fetchIdx > 0 && body.lastIndexOf('if (!inferred) {', fetchIdx) > 0,
      'the resolve-hops call sits behind the !inferred guard');
    assert.ok(/if \(inferredRaw\) return Object\.assign\(inferredRaw\[hopIdx\], obsInfo\)/.test(body),
      'and each canonical hop comes from its own inferred entry');
    assert.ok(/const hopFullKey = inferred \? String\(inferred\[hopIdx\]\.pubkey \|\| hop\)/.test(body),
      'observer coverage is counted against the inferred pubkey, not the bare prefix');
  });

  test('inferredRouteHops goes through the shared helper and keeps the hex token', () => {
    const body = mapSrc.slice(mapSrc.indexOf('function inferredRouteHops'),
      mapSrc.indexOf('async function drawPacketRoute(hopKeys'));
    assert.ok(/routeHopFromInferred\(/.test(body), 'no second copy of the mapping (DRY)');
    assert.ok(/_hopHex: String\(hop\)/.test(body),
      '#1633 — the original hex token stays, so the 1-byte filter still works');
    assert.ok(!/nodes\.(find|filter)\(/.test(body), 'roles come from one pass, not a scan per hop');
  });

  test('the #/map deep link infers the chosen observation with the shared helper', () => {
    const body = mapSrc.slice(mapSrc.indexOf('async function loadRouteFromDeepLink'));
    assert.ok(/inferObservationPath\(chosen, obsPos, originPos\)/.test(body),
      'it calls the same helper the detail pane uses, on the chosen observation');
    assert.ok(/await window\.ensureHopAffinity\(\)/.test(body), 'with the neighbour graph loaded');
    assert.ok(/inferredHops: inferredHops/.test(body), 'and hands the entries to the renderers');
    assert.strictEqual((body.match(/inferredHops: inferredHops/g) || []).length, 2,
      'both the single and the multi renderer get them');
    assert.ok(!/HopResolver\.resolve\(/.test(body),
      'the deep link no longer resolves prefixes one at a time');
  });

  test('the "View route on map" button resolves nothing it then throws away', () => {
    const pktSrc = src('public/packets.js');
    const start = pktSrc.indexOf("panel.querySelector('#viewRouteBtn')");
    const body = pktSrc.slice(start, pktSrc.indexOf('function refreshDetailHops'));
    assert.ok(/window\.location\.hash = '#\/map\?packet='/.test(body), 'it still navigates by deep link');
    assert.ok(!/HopResolver\.resolve\(/.test(body), 'and computes no hops for the navigation to discard');
    assert.ok(!/(const|let)\s+(allPaths|resolvedKeys|destination)\b/.test(body),
      'nor the allPaths / resolvedKeys / destination payloads it used to build and drop');
  });

  test('route-view labels an unlisted hop "unlisted", not "unknown"', () => {
    const rv = src('public/route-view.js');
    assert.ok(/p\.unlisted\s*\n?\s*\?\s*'No listed node with this prefix fits this point in the path'/.test(rv),
      'the chip explains what unlisted means');
    assert.ok(/\(p\.unlisted \? 'unlisted' : 'unknown'\)/.test(rv),
      'and the two states read differently');
  });

  test('every new global is declared for the no-undef lint gate', () => {
    const rc = JSON.parse(src('.eslintrc.json'));
    for (const name of ['inferObservationPath', 'ensureHopAffinity', 'hasHopAffinity', 'routeHopFromInferred']) {
      assert.strictEqual(rc.globals[name], 'readonly', name + ' must be declared in .eslintrc.json');
    }
  });
}

console.log('\n--- ' + passed + ' passed, ' + failed + ' failed ---\n');
process.exitCode = failed > 0 ? 1 : 0;
})();
