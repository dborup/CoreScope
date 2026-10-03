/**
 * #165 — hop resolution per observer on the packets page.
 *
 * The port of upstream `Kpa-clawbot/CoreScope#2099` resolves every path hop
 * with the observer that heard it and caches the result under `hop:observer`.
 * Review of that port found defects which these tests pin, each against the
 * real public/packets.js, public/hop-resolver.js and public/hop-display.js
 * loaded into a vm sandbox (no copies of production code):
 *
 *   1. Missing observer coordinates must not become a (0, 0) anchor.
 *   2. A hop the server resolved (resolved_path) keeps the server's node; the
 *      client heuristic must not overwrite it under the same observer.
 *   3. The incremental path (WS/poll) resolves the same prefix separately for
 *      each observer instead of reusing another observer's bare-key entry.
 *   B. The hop:observer cache is bounded (AGENTS.md: no unbounded maps) and is
 *      cleared at the page's existing reset point, destroy().
 *   C. Resolving per observer costs more than one global resolve, so
 *      resolveHopsForPackets() yields to the event loop between observer
 *      groups instead of blocking the page for the whole initial load.
 *   UI. The list's one-per-path ambiguity indicator stays visible when the
 *      path overflows its cell, and is not counted as a hidden hop.
 *
 * Run: node test-issue-165-hop-resolution-per-observer.js
 */
'use strict';
const fs = require('fs');
const path = require('path');
const vm = require('vm');

let passed = 0, failed = 0;
const pending = [];
function test(name, fn) { pending.push({ name, fn }); }
function section(title) { pending.push({ title }); }
function assert(cond, msg) { if (!cond) throw new Error(msg); }

function escapeHtml(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

// Load the real packets page into a fresh sandbox. `nodes` and `observers`
// are what /api/nodes and /api/observers return to ensureHopResolver().
function loadPackets(nodes, observers) {
  const store = {};
  const ctx = {
    window: { addEventListener() {}, dispatchEvent() {}, innerWidth: 1280 },
    document: {
      readyState: 'complete',
      createElement: () => ({ style: {}, dataset: {}, classList: { add() {}, remove() {} }, appendChild() {}, setAttribute() {} }),
      head: { appendChild() {} }, body: { appendChild() {}, removeChild() {} },
      getElementById: () => null, querySelector: () => null, querySelectorAll: () => [],
      addEventListener() {}, removeEventListener() {},
    },
    localStorage: { getItem: k => (k in store ? store[k] : null), setItem: (k, v) => { store[k] = String(v); }, removeItem: k => { delete store[k]; } },
    console, Math, Object, Array, Number, Date, Map, Set, JSON, String, Boolean, RegExp, Error, TypeError, Promise,
    parseInt, parseFloat, isNaN, isFinite, encodeURIComponent, decodeURIComponent, URLSearchParams,
    setTimeout, clearTimeout, setInterval: () => 0, clearInterval() {},
    requestAnimationFrame: () => 0, cancelAnimationFrame() {},
    performance: { now: () => Date.now() },
    location: { hash: '' },
    escapeHtml,
    registerPage() {}, onWS() {}, offWS() {}, debouncedOnWS: fn => fn,
    CLIENT_TTL: {},
    fetchAllNodes: () => Promise.resolve({ nodes }),
    api: (p) => Promise.resolve(p.startsWith('/observers') ? { observers } : p.startsWith('/iata-coords') ? { coords: {} } : {}),
    invalidateApiCache() {},
  };
  ctx.window.localStorage = ctx.localStorage;
  vm.createContext(ctx);
  for (const f of ['payload-labels.js', 'packet-helpers.js', 'hop-resolver.js', 'hop-display.js', 'packets.js']) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, 'public', f), 'utf8'), ctx, { filename: f });
    for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k];
  }
  return ctx.window._packetsTestAPI;
}

// Three repeaters share the 1-byte prefix "ef". FAR-AWAY is listed first:
// with no anchor the resolver keeps candidate order, so it is the default pick.
// NULL-ISLAND sits next to (0, 0), so a false (0, 0) anchor selects it.
const FAR = { public_key: 'efbf0eeacc', name: 'FAR-AWAY', role: 'repeater', lat: 50.87, lon: 5.52 };
const NULL_ISLAND = { public_key: 'ef00000011', name: 'NULL-ISLAND', role: 'repeater', lat: 0.2, lon: 0.2 };
const NEAR = { public_key: 'ef0069c0aa', name: 'NEAR-ONE', role: 'repeater', lat: 51.08, lon: 3.78 };

section('#165 defect 1: missing observer coordinates are not (0, 0)');

test('observerPosition() returns [null, null] for null coordinates', async () => {
  const T = loadPackets([FAR, NULL_ISLAND], [{ id: 'OBS-NULL', lat: null, lon: null }]);
  await T.resolveHops(['ef'], 'OBS-NULL'); // seeds observerMap via ensureHopResolver
  const pos = T.observerPosition('OBS-NULL');
  assert(pos[0] === null && pos[1] === null, 'expected [null, null], got ' + JSON.stringify(pos));
});

test('observerPosition() returns [null, null] when only one coordinate is valid', async () => {
  const T = loadPackets([FAR], [{ id: 'OBS-HALF', lat: 51.2, lon: null }, { id: 'OBS-EMPTY', lat: '', lon: '' }]);
  await T.resolveHops(['ef'], 'OBS-HALF');
  const half = T.observerPosition('OBS-HALF');
  const empty = T.observerPosition('OBS-EMPTY');
  assert(half[0] === null && half[1] === null, 'lat only: expected [null, null], got ' + JSON.stringify(half));
  assert(empty[0] === null && empty[1] === null, 'empty strings: expected [null, null], got ' + JSON.stringify(empty));
});

test('observerPosition() treats (0, 0) as no fix, like the server and HopResolver do', async () => {
  const T = loadPackets([FAR], [{ id: 'OBS-ZERO', lat: 0, lon: 0 }]);
  await T.resolveHops(['ef'], 'OBS-ZERO');
  const pos = T.observerPosition('OBS-ZERO');
  assert(pos[0] === null && pos[1] === null, 'expected [null, null], got ' + JSON.stringify(pos));
});

test('observerPosition() still returns real coordinates', async () => {
  const T = loadPackets([FAR], [{ id: 'OBS-OK', lat: 51.21, lon: '3.44' }]);
  await T.resolveHops(['ef'], 'OBS-OK');
  const pos = T.observerPosition('OBS-OK');
  assert(pos[0] === 51.21 && pos[1] === 3.44, 'expected [51.21, 3.44], got ' + JSON.stringify(pos));
});

test('an observer without coordinates does not steer the candidate choice', async () => {
  const T = loadPackets([FAR, NULL_ISLAND, NEAR], [{ id: 'OBS-NULL', lat: null, lon: null }]);
  await T.resolveHops(['ef'], 'OBS-NULL');
  const entry = T._hopCacheGet('ef:OBS-NULL');
  assert(entry && entry.ambiguous, 'the hop is still reported as ambiguous');
  assert(entry.name === 'FAR-AWAY',
    'with no anchor the resolver keeps candidate order (FAR-AWAY); got ' + (entry && entry.name));
});

section('#165 defect 2: the server resolved_path wins over the heuristic');

// OBS-A sits next to NEAR-ONE, so the client heuristic picks NEAR-ONE for
// "ef". The server, which has the neighbour graph, says FAR-AWAY. "c1" is
// left unresolved by the server (null) and has two candidates: that one is
// genuinely uncertain and must still be shown as such.
const C1A = { public_key: 'c1aaaa0001', name: 'C1-ALPHA', role: 'repeater', lat: 51.10, lon: 3.70 };
const C1B = { public_key: 'c1bbbb0002', name: 'C1-BRAVO', role: 'repeater', lat: 51.12, lon: 3.72 };
const OBS_A = { id: 'OBS-A', lat: 51.21, lon: 3.44 };
function serverPacket() {
  return { id: 1, hash: 'h1', observer_id: 'OBS-A', path_json: '["ef","c1"]',
    resolved_path: JSON.stringify([FAR.public_key, null]) };
}

test('initial load: cacheResolvedPaths + resolveHopsForPackets keep the server node', async () => {
  const T = loadPackets([FAR, NEAR, C1A, C1B], [OBS_A]);
  // Precondition: without the server's answer the heuristic picks NEAR-ONE,
  // so this test can only pass if the server's entry is the one kept.
  const T0 = loadPackets([FAR, NEAR, C1A, C1B], [OBS_A]);
  await T0.resolveHops(['ef'], 'OBS-A');
  assert(T0._hopCacheGet('ef:OBS-A').name === 'NEAR-ONE', 'precondition: heuristic picks NEAR-ONE');

  const pkt = serverPacket();
  await T.cacheResolvedPaths([pkt]);
  await T.resolveHopsForPackets([pkt]);
  const entry = T._hopCacheGet('ef:OBS-A');
  assert(entry && entry.pubkey === FAR.public_key,
    'ef:OBS-A must be the server node FAR-AWAY; got ' + (entry && entry.name));
  const html = T.renderPath(['ef'], 'OBS-A');
  assert(/FAR-AWAY/.test(html) && !/NEAR-ONE/.test(html), 'the rendered hop shows FAR-AWAY: ' + html);
});

test('a hop the server left unresolved is still resolved and flagged', async () => {
  const T = loadPackets([FAR, NEAR, C1A, C1B], [OBS_A]);
  const pkt = serverPacket();
  await T.cacheResolvedPaths([pkt]);
  await T.resolveHopsForPackets([pkt]);
  const c1 = T._hopCacheGet('c1:OBS-A');
  assert(c1 && c1.ambiguous, 'c1 (server null) is client-resolved and ambiguous');
  const list = T.renderPath(['ef', 'c1'], 'OBS-A', { summary: true });
  const m = list.match(/hop-path-warn[^>]*>[\s\S]*?<\/svg>(\d+)<\/span>/);
  assert(m && m[1] === '1', 'the list summary counts exactly the one uncertain hop (c1); got ' + (m ? m[1] : 'no summary'));
});

test('incremental path: resolveIncomingHops keeps the server node too', async () => {
  const T = loadPackets([FAR, NEAR, C1A, C1B], [OBS_A]);
  await T.resolveHops(['00'], 'OBS-A'); // HopResolver ready, as after the initial load
  await T.resolveIncomingHops([serverPacket()]);
  const entry = T._hopCacheGet('ef:OBS-A');
  assert(entry && entry.pubkey === FAR.public_key,
    'ef:OBS-A must be the server node FAR-AWAY; got ' + (entry && entry.name));
});

section('#165 defect 3: the incremental path resolves per observer');

// OBS-A is next to NEAR-ONE, OBS-B next to FAR-AWAY. The same 1-byte prefix
// heard by each must resolve to that observer's neighbour.
const OBS_B = { id: 'OBS-B', lat: 50.88, lon: 5.50 };
function livePacket(id, obs) { return { id, hash: 'live' + id, observer_id: obs, path_json: '["ef"]' }; }

test('same prefix from observer A, then B, via resolveIncomingHops: each gets its own name', async () => {
  const T = loadPackets([FAR, NEAR], [OBS_A, OBS_B]);
  await T.resolveIncomingHops([livePacket(1, 'OBS-A')]);
  await T.resolveIncomingHops([livePacket(2, 'OBS-B')]);
  const a = T._hopCacheGet('ef:OBS-A');
  const b = T._hopCacheGet('ef:OBS-B');
  assert(a && a.name === 'NEAR-ONE', 'ef:OBS-A is NEAR-ONE; got ' + (a && a.name));
  assert(b && b.name === 'FAR-AWAY', 'ef:OBS-B is resolved for B (FAR-AWAY); got ' + (b ? b.name : 'no entry'));
  const html = T.renderPath(['ef'], 'OBS-B');
  assert(/FAR-AWAY/.test(html) && !/NEAR-ONE/.test(html), 'B does not inherit A\'s name: ' + html);
});

test('one incremental batch with both observers resolves each separately', async () => {
  const T = loadPackets([FAR, NEAR], [OBS_A, OBS_B]);
  await T.resolveHopsForPackets([livePacket(1, 'OBS-A')]); // initial load saw only A
  await T.resolveIncomingHops([livePacket(2, 'OBS-A'), livePacket(3, 'OBS-B')]);
  const b = T._hopCacheGet('ef:OBS-B');
  assert(b && b.name === 'FAR-AWAY', 'ef:OBS-B is resolved for B; got ' + (b ? b.name : 'no entry'));
});

section('#165 B: the hop cache is bounded');

test('the cache never holds more than HOP_CACHE_MAX entries', async () => {
  const T = loadPackets([FAR, NEAR], [OBS_A]);
  const max = T.HOP_CACHE_MAX;
  assert(Number.isInteger(max) && max > 0, 'HOP_CACHE_MAX is a positive integer; got ' + max);
  // Distinct 3-byte prefixes no node has: each writes hop:OBS-A and hop.
  const hops = [];
  for (let i = 0; hops.length < max; i++) hops.push((0x100000 + i).toString(16));
  await T.resolveHops(hops, 'OBS-A');
  assert(T._hopCacheSize() <= max, 'size ' + T._hopCacheSize() + ' exceeds the cap ' + max);
});

test('eviction drops the oldest entries and keeps the newest', async () => {
  const T = loadPackets([FAR, NEAR], [OBS_A, OBS_B]);
  const max = T.HOP_CACHE_MAX;
  await T.resolveHops(['ef'], 'OBS-A'); // oldest
  const hops = [];
  for (let i = 0; hops.length < max; i++) hops.push((0x100000 + i).toString(16));
  await T.resolveHops(hops, 'OBS-B');
  await T.resolveHops(['ef'], 'OBS-B'); // newest
  assert(T._hopCacheGet('ef:OBS-A') === undefined, 'the oldest entry (ef:OBS-A) was evicted');
  const b = T._hopCacheGet('ef:OBS-B');
  assert(b && b.name === 'FAR-AWAY', 'the newest entry (ef:OBS-B) is kept; got ' + (b ? b.name : 'no entry'));
  assert(T._hopCacheSize() <= max, 'size ' + T._hopCacheSize() + ' exceeds the cap ' + max);
});

test('destroy() empties the cache', async () => {
  const T = loadPackets([FAR, NEAR], [OBS_A]);
  await T.resolveHops(['ef'], 'OBS-A');
  assert(T._hopCacheSize() > 0, 'precondition: the cache has entries');
  T._destroy();
  assert(T._hopCacheSize() === 0, 'destroy() leaves ' + T._hopCacheSize() + ' entries');
});

section('#165 C: resolveHopsForPackets does not block for the whole load');

test('a timer runs between two observer groups', async () => {
  const T = loadPackets([FAR, NEAR], [OBS_A, OBS_B]);
  await T.resolveHops(['00'], 'OBS-A'); // HopResolver ready, as after the first render
  const before = T._hopCacheSize();
  let seenAt = null;
  setTimeout(() => { seenAt = T._hopCacheSize(); }, 0);
  await T.resolveHopsForPackets([livePacket(1, 'OBS-A'), livePacket(2, 'OBS-B')]);
  const after = T._hopCacheSize();
  assert(seenAt !== null && seenAt > before && seenAt < after,
    'the timer should fire after the first group and before the last (cache sizes: before ' + before +
    ', at timer ' + seenAt + ', after ' + after + ')');
});

section('#165 UI: the list summary survives a clipped path cell');

test('the summary indicator comes before the hops, so overflow cannot clip it', async () => {
  const T = loadPackets([FAR, NEAR, C1A, C1B], [OBS_A]);
  await T.resolveHops(['ef', 'c1'], 'OBS-A');
  const html = T.renderPath(['c1', 'ef'], 'OBS-A', { summary: true });
  const warn = html.indexOf('hop-path-warn'), firstHop = html.search(/class="hop[ "]/);
  assert(warn !== -1, 'the summary indicator is rendered: ' + html);
  assert(warn < firstHop, 'the indicator precedes the first hop: ' + html);
  const detail = T.renderPath(['c1', 'ef'], 'OBS-A');
  // (Per-hop badge rendering itself is covered in test-issue-165-hop-ambiguity-badge.js.)
  assert(detail.indexOf('hop-path-warn') === -1 && /hop-ambiguous/.test(detail),
    'the detail form has no path summary and keeps the per-hop ambiguity: ' + detail);
});

test('the overflow pill counts hops only, not the summary indicator', async () => {
  const T = loadPackets([FAR], []);
  // Fake .path-hops host 100px wide: [warn past the edge] [hop inside] [arrow] [hop past the edge].
  const el = (cls, left, right) => ({ classList: { contains: c => cls.split(' ').includes(c) }, getBoundingClientRect: () => ({ left, right }) });
  const appended = [];
  const host = {
    dataset: {}, children: [el('hop-path-warn status-warn', 120, 140), el('hop', 0, 50), el('arrow', 50, 60), el('hop', 60, 160)],
    querySelector: () => null, getBoundingClientRect: () => ({ right: 100 }), appendChild: c => appended.push(c),
  };
  T._finalizePathOverflow({ querySelectorAll: () => [host] });
  assert(appended.length === 1 && appended[0].textContent === '+1',
    'expected one "+1" pill for the one clipped hop; got ' + JSON.stringify(appended.map(a => a.textContent)));
});

(async () => {
  for (const t of pending) {
    if (t.title) { console.log('\n=== ' + t.title.trim() + ' ==='); continue; }
    try { await t.fn(); passed++; console.log('  ✅ ' + t.name); }
    catch (e) { failed++; console.log('  ❌ ' + t.name + ': ' + e.message); }
  }
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
