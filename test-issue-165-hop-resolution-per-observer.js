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
  // With an IATA code, so the ambiguous hop gets a badge (globalFallback) and
  // is counted; PR #185 F6 counts only badged hops.
  const T = loadPackets([FAR, NEAR, C1A, C1B], [Object.assign({ iata: 'XYZ' }, OBS_A)]);
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
  const html = T.renderPath(['ef', 'c1'], 'OBS-I', { summary: true });
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

test('cacheResolvedPaths yields while it walks a large page', async () => {
  const T = loadPackets([FAR, NEAR, C1A, C1B], [OBS_A, OBS_B]);
  await T.resolveHops(['00'], 'OBS-A'); // HopResolver ready
  let ranDuring = false, done = false;
  setTimeout(() => { ranDuring = !done; }, 0);
  await T.cacheResolvedPaths(serverRows(5000, ['OBS-A', 'OBS-B']));
  done = true;
  assert(ranDuring, 'a timer queued before the call must run before it finishes');
});

test('a repeated server answer does not rewrite the cache entry', async () => {
  const T = loadPackets([FAR, NEAR, C1A, C1B], [OBS_A]);
  await T.cacheResolvedPaths(serverRows(1, ['OBS-A']));
  const first = T._hopCacheGet('ef:OBS-A'), bare = T._hopCacheGet('ef');
  assert(first && first.pubkey === FAR.public_key, 'precondition: the server answer is cached');
  await T.cacheResolvedPaths(serverRows(50, ['OBS-A']));
  assert(T._hopCacheGet('ef:OBS-A') === first && T._hopCacheGet('ef') === bare,
    'the same answer for the same key is skipped, not written again per row');
});

test('a different server answer for the same key still replaces it', async () => {
  const T = loadPackets([FAR, NEAR, C1A, C1B], [OBS_A]);
  await T.cacheResolvedPaths(serverRows(1, ['OBS-A']));
  await T.cacheResolvedPaths([{ id: 7, hash: 'x7', observer_id: 'OBS-A', path_json: '["ef"]', resolved_path: [NEAR.public_key] }]);
  const e = T._hopCacheGet('ef:OBS-A');
  assert(e && e.pubkey === NEAR.public_key, 'ef:OBS-A follows the newer server answer; got ' + (e && e.name));
});

test('a server answer replaces an ambiguous client pick of the same node', async () => {
  // The heuristic already picked NEAR-ONE for OBS-I, flagged as ambiguous;
  // the server then confirms NEAR-ONE. Same pubkey, but the entry must
  // become the server's, or the list keeps warning about a resolved hop.
  const T = loadPackets([FAR, NEAR, C1A, C1B], [OBS_I]);
  await T.resolveHops(['ef'], 'OBS-I');
  const pick = T._hopCacheGet('ef:OBS-I');
  assert(pick && pick.ambiguous && pick.pubkey === NEAR.public_key, 'precondition: ambiguous client pick NEAR-ONE; got ' + (pick && pick.name));
  await T.cacheResolvedPaths([{ id: 8, hash: 'x8', observer_id: 'OBS-I', path_json: '["ef"]', resolved_path: [NEAR.public_key] }]);
  const e = T._hopCacheGet('ef:OBS-I');
  assert(e && !e.ambiguous, 'ef:OBS-I is the server answer now, not the ambiguous pick');
  assert(warnCount(T.renderPath(['ef'], 'OBS-I', { summary: true })) === 0, 'and the list no longer flags it');
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
  return T;
}

test('probe 1 (MD): the summary counts per observer, so OBS-I\'s ambiguous "ef" is flagged', async () => {
  const T = await serverThenClient();
  assert(T._hopCacheGet('ef:OBS-I') && T._hopCacheGet('ef:OBS-I').ambiguous, 'precondition: ef:OBS-I is ambiguous');
  const html = T.renderPath(['ef'], 'OBS-I', { summary: true });
  assert(warnCount(html) === 1, 'expected one flagged hop for OBS-I: ' + html);
  assert(warnCount(T.renderPath(['ef'], 'OBS-A', { summary: true })) === 0, 'and none for OBS-A, which has the server answer');
});

test('probe 2 (MC): the client heuristic does not overwrite the server answer under the bare key', async () => {
  const T = await serverThenClient();
  const bare = T._hopCacheGet('ef');
  assert(bare && bare.pubkey === FAR.public_key, 'bare "ef" stays the server node FAR-AWAY; got ' + (bare && bare.name));
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

test('rewriting an entry refreshes it, so the server answer is not the next to be evicted', async () => {
  const T = loadPackets([FAR, NEAR], [OBS_A, OBS_B]);
  const max = T.HOP_CACHE_MAX;
  await T.resolveHops(['ef'], 'OBS-A'); // oldest: ef:OBS-A and ef
  const fill = [];
  for (let i = 0; fill.length < (max - 2) / 2; i++) fill.push((0x100000 + i).toString(16));
  await T.resolveHops(fill, 'OBS-B'); // two keys each: the cache is now exactly full
  assert(T._hopCacheSize() === max, 'precondition: cache full, nothing evicted (' + T._hopCacheSize() + ')');
  // The server's answer for OBS-A arrives and rewrites both oldest keys.
  await T.cacheResolvedPaths([{ id: 9, hash: 'h9', observer_id: 'OBS-A', path_json: '["ef"]', resolved_path: JSON.stringify([FAR.public_key]) }]);
  await T.resolveHops(['abcdef', 'abcdf0'], 'OBS-B'); // four new keys evict four
  const e = T._hopCacheGet('ef:OBS-A');
  assert(e && e.pubkey === FAR.public_key, 'ef:OBS-A (rewritten last) survives; got ' + (e ? e.name : 'evicted'));
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

test('destroy() during cacheResolvedPaths stops it too', async () => {
  const T = loadPackets([FAR, NEAR, C1A, C1B], [OBS_A, OBS_B]);
  await T.resolveHops(['00'], 'OBS-A'); // HopResolver ready
  T._destroy();
  const job = T.cacheResolvedPaths(serverRows(5000, ['OBS-A', 'OBS-B']));
  setTimeout(() => T._destroy(), 0); // left during the first yield
  await job;
  assert(T._hopCacheSize() === 0, 'the old job wrote ' + T._hopCacheSize() + ' entries into the new cache');
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
