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

console.log('\n=== #165 defect 1: missing observer coordinates are not (0, 0) ===');

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

(async () => {
  for (const t of pending) {
    try { await t.fn(); passed++; console.log('  ✅ ' + t.name); }
    catch (e) { failed++; console.log('  ❌ ' + t.name + ': ' + e.message); }
  }
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
