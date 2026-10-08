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
 *   2. A hop the server resolved (resolved_path) keeps that observation's
 *      node at that hop position, without poisoning the shared prefix cache.
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
    // App-level display helpers only; hop resolution/rendering remains real.
    payloadTypeName: () => 'ADVERT', payloadTypeColor: () => 'info',
    routeTypeName: () => 'FLOOD', transportBadge: () => '', scopeCellHtml: () => '',
    getPathLenOffset: () => 1,
    truncate: (s, n) => String(s).length > n ? String(s).slice(0, n) + '…' : String(s),
    registerPage() {}, onWS() {}, offWS() {}, debouncedOnWS: fn => fn,
    CLIENT_TTL: {},
    fetchAllNodes: () => Promise.resolve({ nodes }),
    api: (p) => Promise.resolve(p.startsWith('/observers') ? { observers } : p.startsWith('/iata-coords') ? { coords: {} } : {}),
    invalidateApiCache() {},
  };
  ctx.window.localStorage = ctx.localStorage;
  vm.createContext(ctx);
  for (const f of ['payload-labels.js', 'packet-helpers.js', 'hop-resolver.js', 'hop-display.js', 'hop-filter.js', 'packets.js']) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, 'public', f), 'utf8'), ctx, { filename: f });
    for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k];
  }
  const T = ctx.window._packetsTestAPI;
  T._context = ctx;
  return T;
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
  assert(!entry, 'canonical-only ef must not populate the heuristic cache');
  const html = T.renderPath(['ef', 'c1'], 'OBS-A', {packet: pkt});
  assert(/FAR-AWAY/.test(html) && !/NEAR-ONE/.test(html), 'the rendered hop shows FAR-AWAY: ' + html);
});

test('a hop the server left unresolved is still resolved and flagged', async () => {
  // With an IATA code, so the ambiguous hop gets a badge (globalFallback) and
  // is counted; PR #185 F6 counts only badged hops.
  const T = loadPackets([FAR, NEAR, C1A, C1B], [Object.assign({ iata: 'XYZ' }, OBS_A)]);
  const pkt = serverPacket();
  await T.cacheResolvedPaths([pkt]);
  await T.resolveHopsForPackets([pkt]);
  const c1 = T._hopCacheGet('c1:OBS-A');
  assert(c1 && c1.ambiguous, 'c1 (server null) is client-resolved and ambiguous');
  const list = T.renderPath(['ef', 'c1'], 'OBS-A', { summary: true, packet: pkt });
  const m = list.match(/hop-path-warn[^>]*>[\s\S]*?<\/svg>(\d+)<\/span>/);
  assert(m && m[1] === '1', 'the list summary counts exactly the one uncertain hop (c1); got ' + (m ? m[1] : 'no summary'));
});

test('incremental path: resolveIncomingHops keeps the server node too', async () => {
  const T = loadPackets([FAR, NEAR, C1A, C1B], [OBS_A]);
  await T.resolveHops(['00'], 'OBS-A'); // HopResolver ready, as after the initial load
  const pkt = serverPacket();
  await T.resolveIncomingHops([pkt]);
  const entry = T._hopCacheGet('ef:OBS-A');
  assert(!entry, 'incremental canonical answer must not populate the heuristic cache');
  assert(/FAR-AWAY/.test(T.renderPath(['ef', 'c1'], 'OBS-A', {packet: pkt})), 'incremental row keeps its canonical node');
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

test('large parse/grouping pass yields, and destroy cancels it before it can write', async () => {
  const T = loadPackets([FAR, NEAR], [OBS_A]);
  await T.resolveHops(['00'], 'OBS-A');
  T._destroy();
  let yielded = false;
  setTimeout(() => { yielded = true; T._destroy(); }, 0);
  await T.resolveHopsForPackets(Array.from({length: 30000}, (_, i) => livePacket(i, 'OBS-A')));
  assert(yielded, 'collection must yield before scanning the entire page');
  assert(T._hopCacheSize() === 0, 'cancelled collection cannot write into a remounted page cache');
});

section('#165 UI: the list summary survives a clipped path cell');

test('the summary indicator comes before the hops, so overflow cannot clip it', async () => {
  const T = loadPackets([FAR, NEAR, C1A, C1B], [Object.assign({ iata: 'XYZ' }, OBS_A)]);
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

test('the +N popover lists the hops, not the summary indicator (upstream 2557894d)', async () => {
  const T = loadPackets([FAR], []);
  assert(typeof T._pathPopoverHtml === 'function', '_pathPopoverHtml is exposed');
  const kid = (cls, html) => ({ classList: { contains: c => cls.split(' ').includes(c) }, outerHTML: html });
  const host = { children: [
    kid('hop-path-warn status-warn', '<span class="hop-path-warn status-warn">!2</span>'),
    kid('hop hop-named', '<a class="hop hop-named">A</a>'), kid('arrow', '<span class="arrow">→</span>'),
    kid('hop hop-named', '<a class="hop hop-named">B</a>'), kid('path-overflow-pill', '<span class="path-overflow-pill">+1</span>'),
  ] };
  const html = T._pathPopoverHtml(host);
  assert(!/hop-path-warn/.test(html), 'the summary indicator is left out: ' + html);
  assert(!/path-overflow-pill/.test(html), 'the pill is left out: ' + html);
  assert(/>A</.test(html) && />B</.test(html) && /arrow/.test(html), 'hops and arrows are listed: ' + html);
});

section('PR #185 review F1: the list warns only about hops uncertain for the row\'s observer');

// OBS-I has an IATA code, so HopDisplay badges a hop it cannot narrow down
// (globalFallback) and the list summary counts it. OBS-N has none.
const OBS_I = { id: 'OBS-I', iata: 'XYZ', lat: 51.21, lon: 3.44 };
const OBS_N = { id: 'OBS-N', lat: 51.21, lon: 3.44 };
function warnCount(html) {
  const m = html.match(/hop-path-warn[^>]*>[\s\S]*?<\/svg>(\d+)<\/span>/);
  return m ? Number(m[1]) : 0;
}

test('a grouped row as the server sends it (header resolved_path) is not flagged for the server-resolved hop', async () => {
  const T = loadPackets([FAR, NEAR, C1A, C1B], [OBS_I]);
  // groupByHash row: no observations, resolved_path of the displayed observation.
  const row = { hash: 'g1', observer_id: 'OBS-I', path_json: '["ef","c1"]', count: 3,
    resolved_path: [FAR.public_key, C1A.public_key] };
  await T.cacheResolvedPaths([row]);
  await T.resolveHopsForPackets([row]);
  const html = T.renderPath(['ef', 'c1'], 'OBS-I', { summary: true, packet: row });
  assert(warnCount(html) === 0, 'both hops are server-resolved; got a summary: ' + html);
  assert(/FAR-AWAY/.test(html) && /C1-ALPHA/.test(html), 'the server names are shown: ' + html);
});

test('the summary does not count another observer\'s entry (the bare-key fallback)', async () => {
  const T = loadPackets([FAR, NEAR, C1A, C1B], [OBS_I, OBS_B]);
  // OBS-I resolved "c1" on the client: ambiguous, badged. That also seeds the
  // bare "c1". OBS-B has not resolved "c1" yet.
  await T.resolveHops(['c1'], 'OBS-I');
  assert(warnCount(T.renderPath(['c1'], 'OBS-I', { summary: true })) === 1, 'precondition: flagged for OBS-I');
  const html = T.renderPath(['c1'], 'OBS-B', { summary: true });
  assert(warnCount(html) === 0, 'OBS-B has no answer of its own yet, so nothing is flagged for it: ' + html);
});

section('PR #185 review F1: grouped rows now bring resolved_path for the whole page');

function serverRows(n, obsIds) {
  return Array.from({ length: n }, (_, i) => ({ id: i, hash: 'r' + i, observer_id: obsIds[i % obsIds.length],
    path_json: '["ef","c1"]', resolved_path: [FAR.public_key, null] }));
}

// The old implementation rewrote canonical answers into shared prefix keys,
// requiring periodic yields. Preparation now only initialises the node index;
// these checks pin the corrected contract, not the incorrect cache poisoning.
test('canonical preparation does not walk/rewrite a 30K page after the first answer', async () => {
  const T = loadPackets([FAR, NEAR, C1A, C1B], [OBS_A, OBS_B]);
  await T.resolveHops(['00'], 'OBS-A'); // HopResolver ready
  const size = T._hopCacheSize();
  const rows = serverRows(30000, ['OBS-A', 'OBS-B']);
  Object.defineProperty(rows[1], 'resolved_path', {get() { throw new Error('unnecessary whole-page walk'); }});
  await T.cacheResolvedPaths(rows);
  assert(T._hopCacheSize() === size, 'canonical preparation must not write prefix cache entries');
});

test('a repeated server answer does not rewrite the cache entry', async () => {
  const T = loadPackets([FAR, NEAR, C1A, C1B], [OBS_A]);
  await T.resolveHops(['ef'], 'OBS-A');
  const first = T._hopCacheGet('ef:OBS-A'), bare = T._hopCacheGet('ef');
  await T.cacheResolvedPaths(serverRows(1, ['OBS-A']));
  assert(first && first.pubkey === NEAR.public_key && first.ambiguous, 'precondition: heuristic pick is cached');
  await T.cacheResolvedPaths(serverRows(50, ['OBS-A']));
  assert(T._hopCacheGet('ef:OBS-A') === first && T._hopCacheGet('ef') === bare,
    'canonical answers never replace a cached heuristic, even when repeated');
});

test('different canonical answers for the same prefix remain attached to their rows', async () => {
  const T = loadPackets([FAR, NEAR, C1A, C1B], [OBS_A]);
  const a = serverRows(1, ['OBS-A'])[0];
  const b = { id: 7, hash: 'x7', observer_id: 'OBS-A', path_json: '["ef"]', resolved_path: [NEAR.public_key] };
  await T.cacheResolvedPaths([a, b]);
  assert(/FAR-AWAY/.test(T.renderPath(['ef', 'c1'], 'OBS-A', {packet: a})), 'earlier row keeps FAR');
  assert(/NEAR-ONE/.test(T.renderPath(['ef'], 'OBS-A', {packet: b})), 'later row keeps NEAR');
  assert(!T._hopCacheGet('ef:OBS-A'), 'neither answer becomes prefix-wide certainty');
});

test('a canonical answer overrides an ambiguous client pick only for its own row', async () => {
  // The heuristic already picked NEAR-ONE for OBS-I, flagged as ambiguous;
  // the server then confirms NEAR-ONE for one observation. That observation
  // becomes definite, but the cached heuristic must keep its uncertainty.
  const T = loadPackets([FAR, NEAR, C1A, C1B], [OBS_I]);
  await T.resolveHops(['ef'], 'OBS-I');
  const pick = T._hopCacheGet('ef:OBS-I');
  assert(pick && pick.ambiguous && pick.pubkey === NEAR.public_key, 'precondition: ambiguous client pick NEAR-ONE; got ' + (pick && pick.name));
  const p = { id: 8, hash: 'x8', observer_id: 'OBS-I', path_json: '["ef"]', resolved_path: [NEAR.public_key] };
  await T.cacheResolvedPaths([p]);
  const e = T._hopCacheGet('ef:OBS-I');
  assert(e === pick && e.ambiguous, 'heuristic cache keeps its original ambiguity');
  assert(warnCount(T.renderPath(['ef'], 'OBS-I', { summary: true, packet: p })) === 0, 'canonical row is not flagged');
  assert(warnCount(T.renderPath(['ef'], 'OBS-I', { summary: true })) === 1, 'a row without canonical evidence still is');
});

section('PR #185 review F2: probes for the two surviving mutants');

// OBS-A's packet carries the server's answer for "ef" (FAR-AWAY); OBS-I's
// "ef" is client-resolved and ambiguous.
async function serverThenClient() {
  const T = loadPackets([FAR, NEAR, C1A, C1B], [OBS_A, OBS_I]);
  const a = { id: 1, hash: 'h1', observer_id: 'OBS-A', path_json: '["ef"]', resolved_path: JSON.stringify([FAR.public_key]) };
  const b = { id: 2, hash: 'h2', observer_id: 'OBS-I', path_json: '["ef"]' };
  await T.cacheResolvedPaths([a, b]);
  await T.resolveHopsForPackets([a, b]);
  return {T, a, b};
}

test('probe 1 (MD): the summary counts per observer, so OBS-I\'s ambiguous "ef" is flagged', async () => {
  const {T, a} = await serverThenClient();
  assert(T._hopCacheGet('ef:OBS-I') && T._hopCacheGet('ef:OBS-I').ambiguous, 'precondition: ef:OBS-I is ambiguous');
  const html = T.renderPath(['ef'], 'OBS-I', { summary: true });
  assert(warnCount(html) === 1, 'expected one flagged hop for OBS-I: ' + html);
  assert(warnCount(T.renderPath(['ef'], 'OBS-A', { summary: true, packet: a })) === 0, 'and none for the canonical OBS-A row');
});

test('probe 2 (MC): the bare key is heuristic-only; the canonical row stays definite', async () => {
  const {T, a} = await serverThenClient();
  const bare = T._hopCacheGet('ef');
  assert(bare && bare.ambiguous && bare.pubkey === NEAR.public_key, 'bare key contains only the ambiguous heuristic');
  const html = T.renderPath(['ef'], 'OBS-A', {summary: true, packet: a});
  assert(/FAR-AWAY/.test(html) && !warnCount(html), 'canonical row is independent of the bare heuristic: ' + html);
});

section('PR #185 review F3: the cache cap and its eviction order are pinned');

test('a bench-sized working set (42 observers x 690 hops) fits without eviction', async () => {
  const obs = Array.from({ length: 42 }, (_, i) => ({ id: 'W' + i, iata: 'XYZ', lat: 51 + i / 100, lon: 3.5 }));
  const T = loadPackets([FAR, NEAR], obs);
  const hops = Array.from({ length: 690 }, (_, i) => (0x200000 + i).toString(16));
  // One packet per observer carrying all 690 hops: 42 x 690 hop:observer keys
  // plus 690 bare keys = 29670, about the 28962 the bench leaves behind.
  const pkts = obs.map((o, i) => ({ id: i, hash: 'w' + i, observer_id: o.id, path_json: JSON.stringify(hops) }));
  await T.resolveHopsForPackets(pkts);
  assert(T._hopCacheSize() === 42 * 690 + 690, 'every key is still cached: ' + T._hopCacheSize() + ' of ' + (42 * 690 + 690));
  assert(T._hopCacheGet(hops[0] + ':W0') !== undefined, 'the first key written survives the load');
});

test('canonical answers survive eviction without refreshing shared heuristic entries', async () => {
  const T = loadPackets([FAR, NEAR], [OBS_A, OBS_B]);
  const max = T.HOP_CACHE_MAX;
  await T.resolveHops(['ef'], 'OBS-A'); // oldest: ef:OBS-A and ef
  const fill = [];
  for (let i = 0; fill.length < (max - 2) / 2; i++) fill.push((0x100000 + i).toString(16));
  await T.resolveHops(fill, 'OBS-B'); // two keys each: the cache is now exactly full
  assert(T._hopCacheSize() === max, 'precondition: cache full, nothing evicted (' + T._hopCacheSize() + ')');
  const p = { id: 9, hash: 'h9', observer_id: 'OBS-A', path_json: '["ef"]', resolved_path: JSON.stringify([FAR.public_key]) };
  await T.cacheResolvedPaths([p]);
  await T.resolveHops(['abcdef', 'abcdf0'], 'OBS-B'); // four new keys evict four
  const e = T._hopCacheGet('ef:OBS-A');
  assert(e === undefined, 'oldest heuristic still evicts: canonical preparation must not refresh it');
  assert(/FAR-AWAY/.test(T.renderPath(['ef'], 'OBS-A', {packet: p})), 'canonical row survives cache eviction');
  assert(T._hopCacheGet(fill[0] + ':OBS-B') === undefined, 'the oldest untouched entry is the one evicted');
});

section('PR #185 review F5: the +N popover title counts hops');

test('the title counts the hops it lists, not the pill, summary or arrows', async () => {
  const T = loadPackets([FAR], []);
  const kid = (cls, html) => ({ classList: { contains: c => cls.split(' ').includes(c) }, outerHTML: html });
  const host = { children: [
    kid('hop-path-warn status-warn', '<span class="hop-path-warn status-warn">!1</span>'),
    kid('hop hop-named', '<a class="hop hop-named">A</a>'), kid('arrow', '<span class="arrow">→</span>'),
    kid('hop-group', '<span class="hop-group"><a class="hop">B</a><button class="hop-conflict-btn">2</button></span>'),
    kid('arrow', '<span class="arrow">→</span>'), kid('hop', '<span class="hop">C</span>'),
    kid('path-overflow-pill', '<span class="path-overflow-pill">+1</span>'),
  ] };
  const html = T._pathPopoverHtml(host);
  assert(/Full path \(3 hops\)/.test(html), 'expected "Full path (3 hops)": ' + html);
});

section('PR #185 review F6: the list counts exactly the hops the detail pane badges');

test('an ambiguous hop without a badge (observer has no IATA) is not counted', async () => {
  const T = loadPackets([FAR, NEAR, C1A, C1B], [OBS_N]);
  await T.resolveHops(['c1'], 'OBS-N');
  const e = T._hopCacheGet('c1:OBS-N');
  assert(e && e.ambiguous, 'precondition: c1 is ambiguous for OBS-N');
  const detail = T.renderPath(['c1'], 'OBS-N');
  assert(!/hop-conflict-btn/.test(detail), 'precondition: the detail form shows no badge: ' + detail);
  const list = T.renderPath(['c1'], 'OBS-N', { summary: true });
  assert(warnCount(list) === 0, 'the list must not flag a hop the detail does not: ' + list);
});

test('a badged hop is counted, and its pill is marked hop-uncertain', async () => {
  const T = loadPackets([FAR, NEAR, C1A, C1B], [OBS_I]);
  await T.resolveHops(['c1', 'ef'], 'OBS-I');
  const detail = T.renderPath(['c1', 'ef'], 'OBS-I');
  const badged = (detail.match(/hop-conflict-btn/g) || []).length;
  assert(badged === 2, 'precondition: the detail badges both hops: ' + detail);
  const list = T.renderPath(['c1', 'ef'], 'OBS-I', { summary: true });
  assert(warnCount(list) === 2, 'the summary counts the two badged hops: ' + list);
  assert((list.match(/class="hop [^"]*hop-uncertain/g) || []).length === 2, 'both counted pills carry hop-uncertain: ' + list);
  assert(!/hop-conflict-btn/.test(list), 'the list still has no per-hop badge: ' + list);
});

section('PR #185 review F7: a hop job outliving destroy() stops writing');

test('destroy() during resolveHopsForPackets leaves the new cache empty', async () => {
  const T = loadPackets([FAR, NEAR], [OBS_A, OBS_B]);
  await T.resolveHops(['00'], 'OBS-A'); // HopResolver ready
  T._destroy();
  const job = T.resolveHopsForPackets([livePacket(1, 'OBS-A'), livePacket(2, 'OBS-B')]);
  T._destroy(); // the page is left while the job is in flight
  await job;
  assert(T._hopCacheSize() === 0, 'the old job wrote ' + T._hopCacheSize() + ' entries into the new cache');
});

test('canonical preparation cannot repopulate the cache after destroy()', async () => {
  const T = loadPackets([FAR, NEAR, C1A, C1B], [OBS_A, OBS_B]);
  await T.resolveHops(['00'], 'OBS-A'); // HopResolver ready
  T._destroy();
  const job = T.cacheResolvedPaths(serverRows(5000, ['OBS-A', 'OBS-B']));
  T._destroy();
  await job;
  assert(T._hopCacheSize() === 0, 'the old job wrote ' + T._hopCacheSize() + ' entries into the new cache');
});

section('Observation-specific canonical answers (review follow-up)');

test('same observer: an unresolved row cannot borrow another row canonical answer', async () => {
  const T = loadPackets([FAR, NEAR], [OBS_I]);
  const a = { id: 701, observer_id: 'OBS-I', path_json: '["ef"]', resolved_path: [FAR.public_key] };
  const b = { id: 702, observer_id: 'OBS-I', path_json: '["ef"]', resolved_path: [null] };
  await T.cacheResolvedPaths([a, b]);
  await T.resolveHopsForPackets([a, b]);
  const definite = T.renderPath(['ef'], 'OBS-I', { summary: true, packet: a });
  const uncertain = T.renderPath(['ef'], 'OBS-I', { summary: true, packet: b });
  assert(/FAR-AWAY/.test(definite) && warnCount(definite) === 0, 'canonical row must show FAR without warning: ' + definite);
  assert(warnCount(uncertain) === 1, 'server-null row must remain ambiguous: ' + uncertain);
});

test('same observer: canonical rows retain different nodes regardless of processing order', async () => {
  for (const reverse of [false, true]) {
    const T = loadPackets([FAR, NEAR], [OBS_I]);
    const a = { id: 703, observer_id: 'OBS-I', path_json: '["ef"]', resolved_path: [FAR.public_key] };
    const b = { id: 704, observer_id: 'OBS-I', path_json: '["ef"]', resolved_path: [NEAR.public_key] };
    await T.cacheResolvedPaths(reverse ? [b, a] : [a, b]);
    await T.resolveHopsForPackets([a, b]);
    assert(/FAR-AWAY/.test(T.renderPath(['ef'], 'OBS-I', {packet: a})), 'first row lost FAR');
    assert(/NEAR-ONE/.test(T.renderPath(['ef'], 'OBS-I', {packet: b})), 'second row lost NEAR');
  }
});

test('repeated prefix at two positions keeps the two canonical nodes', async () => {
  const T = loadPackets([FAR, NEAR], [OBS_I]);
  const p = { observer_id: 'OBS-I', path_json: '["ef","ef"]', resolved_path: [FAR.public_key, NEAR.public_key] };
  await T.cacheResolvedPaths([p]);
  await T.resolveHopsForPackets([p]);
  const html = T.renderPath(['ef', 'ef'], 'OBS-I', {packet: p});
  assert(/FAR-AWAY[\s\S]*NEAR-ONE/.test(html), 'path positions collapsed: ' + html);
});

test('missing, explicit null and invalid canonical shapes use uncertain heuristic only', async () => {
  const T = loadPackets([FAR, NEAR], [OBS_I]);
  const good = { observer_id: 'OBS-I', path_json: '["ef"]', resolved_path: [FAR.public_key] };
  await T.cacheResolvedPaths([good]);
  for (const rp of [undefined, null, [], [null], '[null]', '[', {key: FAR.public_key}, [42]]) {
    const p = { observer_id: 'OBS-I', path_json: '["ef"]', resolved_path: rp };
    await T.resolveHopsForPackets([p]);
    const html = T.renderPath(['ef'], 'OBS-I', {summary: true, packet: p});
    assert(warnCount(html) === 1, 'invalid/missing canonical must not borrow certainty (' + JSON.stringify(rp) + '): ' + html);
  }
});

test('same-row canonical replacement is visible without a cache reset', async () => {
  const T = loadPackets([FAR, NEAR], [OBS_I]);
  const p = { observer_id: 'OBS-I', path_json: '["ef"]', resolved_path: [FAR.public_key] };
  await T.cacheResolvedPaths([p]);
  await T.resolveHopsForPackets([p]);
  assert(/FAR-AWAY/.test(T.renderPath(['ef'], 'OBS-I', {packet: p})), 'precondition FAR');
  p.resolved_path = JSON.stringify([NEAR.public_key]);
  await T.resolveIncomingHops([p]);
  assert(/NEAR-ONE/.test(T.renderPath(['ef'], 'OBS-I', {packet: p})), 'updated canonical must be re-read');
  p.resolved_path = [null];
  await T.resolveIncomingHops([p]);
  assert(warnCount(T.renderPath(['ef'], 'OBS-I', {summary: true, packet: p})) === 1, 'updated null must restore uncertainty');
});

test('grouped header, expanded children and flat rows consume their own canonical paths', async () => {
  const T = loadPackets([FAR, NEAR], [OBS_I]);
  const a = { id: 707, hash: 'group-707', observer_id: 'OBS-I', path_json: '["ef"]', resolved_path: [FAR.public_key], count: 2 };
  const b = { id: 708, hash: a.hash, observer_id: 'OBS-I', path_json: '["ef"]', resolved_path: [NEAR.public_key] };
  a._children = [a, b];
  await T.cacheResolvedPaths([a, b]);
  await T.resolveHopsForPackets([a, b]);
  T._setExpanded(a.hash, true);
  const html = T.buildGroupRowHtml(a);
  const rows = html.match(/<tr[\s\S]*?<\/tr>/g) || [];
  assert(rows.length === 3, 'expected group plus two children');
  assert(/FAR-AWAY/.test(rows[0]) && /FAR-AWAY/.test(rows[1]) && /NEAR-ONE/.test(rows[2]), 'group/children borrowed answer: ' + html);
  assert(/FAR-AWAY/.test(T.buildFlatRowHtml(a)) && /NEAR-ONE/.test(T.buildFlatRowHtml(b)), 'flat rows borrowed answer');
});

test('sorting group children keeps the header canonical path and parsed cache aligned', async () => {
  const T = loadPackets([FAR, NEAR], [OBS_I]);
  for (const answer of [NEAR.public_key, null]) {
    const group = {hash: 'sort-' + answer, observer_id: 'OBS-I', path_json: '["ef","ef"]',
      resolved_path: [FAR.public_key, FAR.public_key], count: 2};
    T._context.getParsedPath(group);
    T._context.getResolvedPath(group);
    const first = {id: 720, observer_id: 'OBS-I', observer_name: 'station', timestamp: '2026-01-01T00:00:00Z',
      path_json: '["ef"]', resolved_path: [answer]};
    const second = {...group, id: 721, observer_name: 'station', timestamp: '2026-01-01T00:01:00Z'};
    group._children = [second, first];
    await T.resolveHopsForPackets(group._children);
    T.sortGroupChildren(group);
    const html = T.buildGroupRowHtml(group);
    assert(!/FAR-AWAY/.test(html), 'sorted header retained old canonical answer: ' + html);
    assert(/NEAR-ONE/.test(html) && warnCount(html) === (answer ? 0 : 1), 'sorted header certainty must follow first child: ' + html);
    assert(T._context.getParsedPath(group).length === 1, 'sorting did not clear parent parsed path');
  }
});

test('hiding a 1-byte hop preserves original canonical indices for later/repeated hops', async () => {
  const T = loadPackets([FAR, NEAR, C1A], [OBS_I]);
  const p = { observer_id: 'OBS-I', path_json: ['c1', 'ef00', 'ef00'],
    resolved_path: [C1A.public_key, NEAR.public_key, FAR.public_key] };
  await T.resolveHopsForPackets([p]);
  T._context.window.MC_setHide1ByteHops(true);
  for (const summary of [false, true]) {
    const html = T.renderPath(p.path_json, 'OBS-I', {packet: p, summary});
    assert(/NEAR-ONE[\s\S]*FAR-AWAY/.test(html) && !/C1-ALPHA/.test(html), 'filtered path shifted an answer: ' + html);
    assert(!warnCount(html), 'canonical multi-byte hops must remain definite');
  }
  assert(p.path_json.length === 3 && p.resolved_path[0] === C1A.public_key, 'filter must not mutate stored data');
  T._context.window.MC_setHide1ByteHops(false);
  assert(/C1-ALPHA[\s\S]*NEAR-ONE[\s\S]*FAR-AWAY/.test(T.renderPath(p.path_json, 'OBS-I', {packet: p})), 'toggle-off restores original positions');
});

test('API array/string shapes and absent child answers never inherit parent certainty', async () => {
  for (const text of [false, true]) {
    const T = loadPackets([FAR, NEAR], [OBS_I]);
    const parent = { observer_id: 'OBS-I', path_json: text ? '["ef"]' : ['ef'],
      resolved_path: text ? JSON.stringify([FAR.public_key]) : [FAR.public_key] };
    // Parse before spreading, reproducing the actual parent cache inheritance.
    T._context.getResolvedPath(parent);
    T._context.getParsedPath(parent);
    const own = T.observationPacket(parent, {id: 709, path_json: '["ef"]', resolved_path: [NEAR.public_key]});
    const missing = T.observationPacket(parent, {id: 710, path_json: '["ef"]'});
    const explicitNull = T.observationPacket(parent, {id: 711, path_json: '["ef"]', resolved_path: null});
    await T.resolveIncomingHops([parent, own, missing, explicitNull]);
    assert(/FAR-AWAY/.test(T.buildFlatRowHtml(parent)), 'parent keeps its canonical answer');
    assert(/NEAR-ONE/.test(T.buildFlatRowHtml(own)), 'child has its own answer');
    for (const p of [missing, explicitNull]) {
      const html = T.renderPath(['ef'], 'OBS-I', {packet: p, summary: true});
      assert(warnCount(html) === 1 && !/FAR-AWAY/.test(html), 'absent child answer inherits parent certainty: ' + html);
    }
  }
});

test('canonical answers are fresh after page destroy/remount and heuristic cache eviction', async () => {
  const T = loadPackets([FAR, NEAR], [OBS_I]);
  const a = {observer_id: 'OBS-I', path_json: '["ef"]', resolved_path: [FAR.public_key]};
  await T.resolveIncomingHops([a]);
  T._destroy();
  const b = {observer_id: 'OBS-I', path_json: '["ef"]', resolved_path: [NEAR.public_key]};
  const c = {observer_id: 'OBS-I', path_json: '["ef"]', resolved_path: [null]};
  await T.resolveIncomingHops([b, c]);
  assert(/FAR-AWAY/.test(T.buildFlatRowHtml(a)) && /NEAR-ONE/.test(T.buildFlatRowHtml(b)), 'remount overwrote row answers');
  assert(warnCount(T.renderPath(['ef'], 'OBS-I', {packet: c, summary: true})) === 1, 'remount null answer must be uncertain');
});

test('30K realistic rows use linear positional lookups, bounded heuristic cache and constant requests', async () => {
  let seed = 363;
  const rnd = () => { seed = (Math.imul(seed, 1664525) + 1013904223) >>> 0; return seed / 4294967296; };
  const nodes = Array.from({length: 2000}, (_, i) => ({
    public_key: (i % 256).toString(16).padStart(2, '0') + i.toString(16).padStart(62, '0'),
    name: 'TEST-' + i, role: 'repeater', lat: 51 + rnd(), lon: 3 + rnd(),
  }));
  const observers = Array.from({length: 42}, (_, i) => ({id: 'LOAD-' + i, iata: 'XYZ', lat: 51 + rnd(), lon: 3 + rnd()}));
  let expectedLookups = 0, totalHops = 0;
  const rows = Array.from({length: 30000}, (_, i) => {
    const width = rnd() < 0.85 ? 1 : (rnd() < 0.7 ? 2 : 3);
    const keys = Array.from({length: Math.floor(rnd() * 9)}, () => nodes[Math.floor(rnd() * nodes.length)].public_key);
    const rp = keys.map(k => { if (rnd() < 0.8) { expectedLookups++; return k; } return null; });
    totalHops += keys.length;
    return {id: i, observer_id: observers[i % 42].id, path_json: JSON.stringify(keys.map(k => k.slice(0, width * 2))), resolved_path: JSON.stringify(rp)};
  });
  const T = loadPackets(nodes, observers), ctx = T._context;
  ctx.console = {log() {}, warn() {}, error: console.error};
  let requests = 0, lookups = 0;
  const fetchNodes = ctx.fetchAllNodes, api = ctx.api, lookup = ctx.HopResolver.serverHopEntry;
  ctx.fetchAllNodes = (...args) => { requests++; return fetchNodes(...args); };
  ctx.api = (...args) => { requests++; return api(...args); };
  ctx.HopResolver.serverHopEntry = key => { lookups++; return lookup(key); };
  const start = performance.now();
  await T.cacheResolvedPaths(rows);
  await T.resolveHopsForPackets(rows);
  for (const p of rows) T.renderPath(ctx.getParsedPath(p), p.observer_id, {packet: p, summary: true});
  assert(lookups === expectedLookups, 'exactly one lookup per canonical position: ' + lookups + ' != ' + expectedLookups);
  assert(requests === 3, 'node/observer/IATA initialization only, not per-row requests: ' + requests);
  assert(T._hopCacheSize() <= T.HOP_CACHE_MAX, 'heuristic cache cap holds on real-size load');
  console.log('    30K bounded work: ' + totalHops + ' hops, ' + lookups + ' canonical lookups, ' + requests + ' requests, ' +
    T._hopCacheSize() + ' cache entries, ' + Math.round(performance.now() - start) + ' ms (informational)');
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
