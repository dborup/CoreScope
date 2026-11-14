/**
 * Unit tests (#721): move A or B of an open Path profile without starting
 * the measurement over — public/terrain-profile.js.
 *
 * The real module is loaded with vm.createContext in a browser-shaped
 * sandbox, as test-issue-681-profile-tool.js does; nothing here copies
 * production logic.
 *
 * Covered:
 *   1. moving one end keeps the other end, both heights and the model, and
 *      measures exactly once (debounced);
 *   2. a move onto a node marker gives a pubkey endpoint, a free drop a
 *      lat/lon endpoint (snapEndpoint, markerRef, pointRef);
 *   3. the address bar follows with replaceState and round-trips;
 *   4. cancelling a move (what Escape does) restores the old endpoint and
 *      issues no request;
 *   5. swap exchanges the ends with their heights, in one request;
 *   6. the Move A / Move B / Swap buttons only exist when the host can pick,
 *      and the moved end keeps its height-source badge.
 *
 * Usage: node test-issue-721-move-endpoints.js
 */
'use strict';
const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

let passed = 0, failed = 0;
function eq(actual, expected, msg) {
  assert.deepStrictEqual(JSON.parse(JSON.stringify(actual === undefined ? null : actual)), expected, msg);
}
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}
const asyncTests = [];
function atest(name, fn) { asyncTests.push([name, fn]); }
const sleep = (ms) => new Promise(r => setTimeout(r, ms));
// The request after a move is debounced like the height steppers (350 ms).
const SETTLE_MS = 450;

const PK_A = 'a'.repeat(64);
const PK_B = 'b'.repeat(64);
const PK_C = 'c'.repeat(64);

// ── Sandbox ────────────────────────────────────────────────────────────────

function makeStorage() {
  const store = {};
  return {
    getItem: k => (Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null),
    setItem: (k, v) => { store[k] = String(v); },
    removeItem: k => { delete store[k]; },
  };
}

/* A DOM just real enough for create(): elements that remember children,
 * attributes and listeners. */
function fakeDom() {
  const made = [];
  function mkEl(tag) {
    const on = {};
    const classes = new Set();
    const e = {
      tagName: String(tag).toUpperCase(), children: [], attrs: {}, style: {},
      className: '', textContent: '', innerHTML: '', value: '', hidden: false, disabled: false,
      parentNode: null,
      classList: {
        add: (c) => classes.add(c),
        remove: (c) => classes.delete(c),
        contains: (c) => classes.has(c),
        toggle: (c, want) => { const v = want === undefined ? !classes.has(c) : !!want; if (v) classes.add(c); else classes.delete(c); },
      },
      setAttribute: (k, v) => { e.attrs[k] = String(v); },
      getAttribute: (k) => (Object.prototype.hasOwnProperty.call(e.attrs, k) ? e.attrs[k] : null),
      appendChild: (c) => { e.children.push(c); c.parentNode = e; return c; },
      removeChild: (c) => { e.children = e.children.filter(x => x !== c); c.parentNode = null; return c; },
      addEventListener: (t, fn) => { (on[t] || (on[t] = [])).push(fn); },
      removeEventListener: (t, fn) => { on[t] = (on[t] || []).filter(f => f !== fn); },
      fire: (t) => { (on[t] || []).slice().forEach(fn => fn({ target: e, preventDefault() {} })); },
    };
    made.push(e);
    return e;
  }
  const body = mkEl('body');
  return {
    doc: { createElement: mkEl, body: body, addEventListener() {}, querySelector: () => null, querySelectorAll: () => [] },
    body: body,
    find: (id) => made.filter(e => e.attrs['data-testid'] === id).pop() || null,
  };
}

function load() {
  const dom = fakeDom();
  const loc = { origin: 'http://x', pathname: '/', search: '', hash: '#/map?lat=56&lon=10&zoom=9' };
  const replaced = [];
  const win = {
    addEventListener() {}, removeEventListener() {}, dispatchEvent() {},
    localStorage: makeStorage(), document: dom.doc, location: loc,
    history: {
      state: null,
      replaceState: (s, t, url) => {
        replaced.push(url);
        const hi = url.indexOf('#');
        loc.hash = hi >= 0 ? url.slice(hi) : '';
      },
    },
    setTimeout: (fn, ms) => setTimeout(fn, ms),
    clearTimeout: (t) => clearTimeout(t),
  };
  const ctx = {
    window: win, console, Math, Date, JSON, Array, Object, String, Number,
    RegExp, Error, TypeError, Boolean, isNaN, isFinite, parseInt, parseFloat,
    URLSearchParams, Map, Set, Promise, encodeURIComponent, decodeURIComponent,
    setTimeout, clearTimeout, document: dom.doc,
  };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync('public/antenna-height.js', 'utf8'), ctx);
  vm.runInContext(fs.readFileSync('public/terrain-profile.js', 'utf8'), ctx);
  return { TP: ctx.window.TerrainProfile, dom, win, loc, replaced };
}

function recordingFetch() {
  const calls = [];
  const fn = (url) => {
    calls.push(url);
    const q = new URLSearchParams(url.split('?')[1]);
    const end = (h) => ({
      kind: 'node', groundM: 0, antennaM: q.get(h) !== null ? Number(q.get(h)) : 10,
      antennaSource: q.get(h) !== null ? 'request' : 'role-default', assumed: q.get(h) === null,
    });
    return Promise.resolve({
      a: end('ha'), b: end('hb'), distanceM: 1000, model: q.get('model'),
      coverage: { state: 'full', pct: 100 },
      samples: [{ d: 0, dtm: 0 }, { d: 500, dtm: 0 }, { d: 1000, dtm: 0 }],
      physics: { freqMHz: 869.618, freqSource: 'firmware', lambdaM: 0.34474, k: 1, f1MaxM: 9 },
      verdict: { state: 'clear', criterion: 0.6 },
    });
  };
  fn.calls = calls;
  fn.params = (i) => new URLSearchParams(calls[i].split('?')[1]);
  return fn;
}

function openTool(env, extra) {
  const fetchJSON = recordingFetch();
  const changes = [];
  const starts = [];
  const cancels = [];
  const tool = env.TP.create(Object.assign({
    mount: env.dom.body, fetchJSON,
    resolveNode: (pk) => ({ name: pk.slice(0, 4), lat: 56, lon: 10, role: 'repeater' }),
    onMoveStart: (side) => starts.push(side),
    onMoveCancel: () => cancels.push(true),
    onEndpointsChange: () => changes.push(tool.getState()),
  }, extra || {}));
  return { tool, fetchJSON, changes, starts, cancels };
}

// ── 1. Moving one end keeps everything else ───────────────────────────────

console.log('\n=== #721: moving one end keeps the other end, the heights and the model ===');

atest('moving A keeps B, both typed heights, the model, k and freq — one request', async () => {
  const env = load();
  const { tool, fetchJSON } = openTool(env);
  tool.setState(env.TP.parseState('profile=' + PK_A + ',' + PK_B + '&model=dtm&ha=12&hb=4&k=1.2&freq=868.5'));
  await sleep(0);
  assert.strictEqual(fetchJSON.calls.length, 1, 'the link did not measure once');
  const moved = tool.moveEndpoint('a', { kind: 'point', lat: 56.1, lon: 10.2 });
  assert.strictEqual(moved, true, 'moveEndpoint did not accept a new position');
  const st = tool.getState();
  eq(st.a, { kind: 'point', lat: 56.1, lon: 10.2 });
  eq(st.b, { kind: 'node', pubkey: PK_B });
  assert.strictEqual(st.ha, 12, 'A lost its height');
  assert.strictEqual(st.hb, 4, 'B lost its height');
  assert.strictEqual(st.model, 'dtm', 'the model was reset');
  assert.strictEqual(st.k, 1.2);
  assert.strictEqual(st.freq, 868.5);
  assert.strictEqual(tool.isOpen(), true, 'the panel closed on a move');
  assert.strictEqual(fetchJSON.calls.length, 1, 'a request went out before the debounce');
  await sleep(SETTLE_MS);
  assert.strictEqual(fetchJSON.calls.length, 2, 'the move issued ' + (fetchJSON.calls.length - 1) + ' requests');
  const q = fetchJSON.params(1);
  assert.strictEqual(q.get('a'), '56.10000,10.20000');
  assert.strictEqual(q.get('b'), PK_B);
  assert.strictEqual(q.get('ha'), '12');
  assert.strictEqual(q.get('hb'), '4');
  assert.strictEqual(q.get('model'), 'dtm');
});

atest('moving B keeps A and its badge; a remembered height for the new end wins', async () => {
  const env = load();
  env.win.localStorage.setItem('mc-terrain-h:' + PK_C, '25');
  const { tool, fetchJSON } = openTool(env);
  tool.setEndpoint('a', { kind: 'node', pubkey: PK_A });
  tool.setEndpoint('b', { kind: 'node', pubkey: PK_B });
  env.dom.find('tp-h-a-plus').fire('click');            // A becomes "yours" = 11
  await sleep(SETTLE_MS);
  const before = fetchJSON.calls.length;
  tool.moveEndpoint('b', { kind: 'node', pubkey: PK_C });
  await sleep(SETTLE_MS);
  assert.strictEqual(fetchJSON.calls.length, before + 1, 'not exactly one request after the move');
  const st = tool.getState();
  eq(st.a, { kind: 'node', pubkey: PK_A });
  assert.strictEqual(st.ha, 1, 'A height moved with B: ' + st.ha);
  assert.strictEqual(st.hb, 25, 'the remembered height of the new B was not used');
  assert.strictEqual(env.dom.find('tp-hsrc-a').attrs['data-source'], 'yours', 'A lost its "yours" badge');
  assert.strictEqual(env.dom.find('tp-hsrc-b').attrs['data-source'], 'yours');
});

atest('an end on a default height takes the new end’s own default', async () => {
  const env = load();
  const { tool, fetchJSON } = openTool(env);
  tool.setEndpoint('a', { kind: 'node', pubkey: PK_A });
  tool.setEndpoint('b', { kind: 'node', pubkey: PK_B });
  await sleep(0);
  tool.moveEndpoint('b', { kind: 'point', lat: 56.2, lon: 10.3 });
  await sleep(SETTLE_MS);
  const q = fetchJSON.params(fetchJSON.calls.length - 1);
  assert.strictEqual(q.get('hb'), null, 'a default height was sent as a confirmed one');
  assert.strictEqual(env.dom.find('tp-hsrc-b').attrs['data-source'], 'default');
});

atest('moving onto the other end, or onto itself, is refused and costs nothing', async () => {
  const env = load();
  const { tool, fetchJSON } = openTool(env);
  tool.setEndpoint('a', { kind: 'node', pubkey: PK_A });
  tool.setEndpoint('b', { kind: 'node', pubkey: PK_B });
  await sleep(0);
  assert.strictEqual(tool.moveEndpoint('a', { kind: 'node', pubkey: PK_B }), false, 'A was moved onto B');
  assert.strictEqual(tool.moveEndpoint('b', { kind: 'node', pubkey: PK_B }), false, 'B was "moved" onto itself');
  assert.strictEqual(tool.moveEndpoint('c', { kind: 'node', pubkey: PK_C }), false, 'an unknown side was accepted');
  await sleep(SETTLE_MS);
  assert.strictEqual(fetchJSON.calls.length, 1, 'a refused move still measured');
  eq(tool.getState().a, { kind: 'node', pubkey: PK_A });
});

atest('a burst of moves coalesces into one request for the last position', async () => {
  const env = load();
  const { tool, fetchJSON } = openTool(env);
  tool.setEndpoint('a', { kind: 'node', pubkey: PK_A });
  tool.setEndpoint('b', { kind: 'node', pubkey: PK_B });
  await sleep(0);
  for (let i = 1; i <= 5; i++) tool.moveEndpoint('b', { kind: 'point', lat: 56 + i / 100, lon: 10 });
  await sleep(SETTLE_MS);
  assert.strictEqual(fetchJSON.calls.length, 2, 'five quick moves issued ' + (fetchJSON.calls.length - 1) + ' requests');
  assert.strictEqual(fetchJSON.params(1).get('b'), '56.05000,10.00000');
});

// ── 2. Snapping ────────────────────────────────────────────────────────────

console.log('\n=== #721: a drop on a node snaps to it, anywhere else is a point ===');

const envS = load();
const isNode = (k) => k === PK_A || k === PK_C;
const CANDS = [
  { x: 100, y: 100, key: PK_A, lat: 56.5, lon: 10.5 },
  { x: 140, y: 100, key: PK_C, lat: 56.6, lon: 10.6 },
  { x: 300, y: 300, key: 'e'.repeat(64), lat: 57.123456, lon: 9.87654 }, // observer only
];

test('a drop inside the radius of a node marker gives that node', () => {
  eq(envS.TP.snapEndpoint({ x: 108, y: 104, lat: 1, lon: 2 }, CANDS, 24, isNode), { kind: 'node', pubkey: PK_A });
});

test('the nearest marker wins when two are in range', () => {
  eq(envS.TP.snapEndpoint({ x: 128, y: 100, lat: 1, lon: 2 }, CANDS, 24, isNode), { kind: 'node', pubkey: PK_C });
});

test('a free drop gives a lat/lon point at the drop, to five decimals', () => {
  eq(envS.TP.snapEndpoint({ x: 600, y: 20, lat: 56.1234567, lon: 10.7654321 }, CANDS, 24, isNode),
    { kind: 'point', lat: 56.12346, lon: 10.76543 });
});

test('just outside the radius is a free point, not a snap', () => {
  const r = envS.TP.snapEndpoint({ x: 100, y: 125, lat: 56, lon: 10 }, CANDS, 24, isNode);
  assert.strictEqual(r.kind, 'point', 'snapped from 25px away');
});

test('exactly on the radius still snaps (24 px), one pixel further does not', () => {
  // 24 px straight below A; C is 46 px away, so only the boundary decides.
  eq(envS.TP.snapEndpoint({ x: 100, y: 124, lat: 1, lon: 2 }, CANDS, 24, isNode), { kind: 'node', pubkey: PK_A });
  eq(envS.TP.snapEndpoint({ x: 100, y: 125, lat: 1, lon: 2 }, CANDS, 24, isNode), { kind: 'point', lat: 1, lon: 2 });
});

test('an observer-only marker snaps to its position as a point, never as a 404 node', () => {
  eq(envS.TP.snapEndpoint({ x: 301, y: 301, lat: 0, lon: 0 }, CANDS, 24, isNode),
    { kind: 'point', lat: 57.12346, lon: 9.87654 });
});

test('markerRef is the one marker → endpoint rule (pick and snap share it)', () => {
  eq(envS.TP.markerRef(PK_A.toUpperCase(), 56, 10, isNode), { kind: 'node', pubkey: PK_A });
  eq(envS.TP.markerRef(null, 56.000004, 10, isNode), { kind: 'point', lat: 56, lon: 10 });
  eq(envS.TP.markerRef('not-hex', 56, 10, () => true), { kind: 'point', lat: 56, lon: 10 });
});

test('pointRef rejects junk instead of measuring to NaN', () => {
  assert.strictEqual(envS.TP.pointRef(NaN, 10), null);
  assert.strictEqual(envS.TP.pointRef(91, 10), null);
  assert.strictEqual(envS.TP.pointRef(56, 181), null);
  eq(envS.TP.pointRef('56.1', '10.2'), { kind: 'point', lat: 56.1, lon: 10.2 });
});

// ── 3. Deep link ───────────────────────────────────────────────────────────

console.log('\n=== #721: the address bar follows a move ===');

atest('a move rewrites the hash with replaceState, and the hash round-trips', async () => {
  const env = load();
  const { tool } = openTool(env);
  tool.setState(env.TP.parseState('profile=' + PK_A + ',' + PK_B + '&model=dtm&ha=12'));
  await sleep(0);
  const n = env.replaced.length;
  tool.moveEndpoint('b', { kind: 'point', lat: 56.16290, lon: 10.20390 });
  assert.ok(env.replaced.length > n, 'the move did not replace the URL');
  const hash = env.loc.hash;
  assert.ok(/^#\/map\?/.test(hash), 'the route was lost: ' + hash);
  const qs = new URLSearchParams(hash.split('?')[1]);
  assert.strictEqual(qs.get('lat'), '56', 'the viewport keys were dropped');
  assert.strictEqual(qs.get('profile'), PK_A + ',56.16290,10.20390');
  const back = env.TP.parseState(hash.split('?')[1]);
  eq(back, JSON.parse(JSON.stringify(tool.getState())));
});

atest('swap rewrites the hash with both ends and heights exchanged', async () => {
  const env = load();
  const { tool } = openTool(env);
  tool.setState(env.TP.parseState('profile=' + PK_A + ',' + PK_B + '&ha=12&hb=4'));
  await sleep(0);
  tool.swap();
  const qs = new URLSearchParams(env.loc.hash.split('?')[1]);
  assert.strictEqual(qs.get('profile'), PK_B + ',' + PK_A);
  assert.strictEqual(qs.get('ha'), '4');
  assert.strictEqual(qs.get('hb'), '12');
});

// ── 4. Cancel (Escape) ─────────────────────────────────────────────────────

console.log('\n=== #721: Escape cancels a move and keeps the old end ===');

atest('beginMove then cancelMove restores the old end with no request', async () => {
  const env = load();
  const { tool, fetchJSON, starts, cancels } = openTool(env);
  tool.setEndpoint('a', { kind: 'node', pubkey: PK_A });
  tool.setEndpoint('b', { kind: 'node', pubkey: PK_B });
  await sleep(0);
  const hashBefore = env.loc.hash;
  tool.beginMove('a');
  eq(starts, ['a']);
  assert.strictEqual(tool.movingSide(), 'a');
  assert.strictEqual(env.dom.find('tp-move-a').attrs['aria-pressed'], 'true', 'Move A is not shown as pressed');
  tool.cancelMove();
  assert.strictEqual(cancels.length, 1, 'the host was not told to disarm');
  assert.strictEqual(tool.movingSide(), null);
  assert.strictEqual(env.dom.find('tp-move-a').attrs['aria-pressed'], 'false');
  eq(tool.getState().a, { kind: 'node', pubkey: PK_A });
  assert.strictEqual(env.loc.hash, hashBefore, 'a cancelled move touched the hash');
  await sleep(SETTLE_MS);
  assert.strictEqual(fetchJSON.calls.length, 1, 'a cancelled move measured');
  tool.cancelMove();
  assert.strictEqual(cancels.length, 1, 'a second cancel reached the host again');
});

atest('the Move buttons toggle: pressing the armed one again cancels', async () => {
  const env = load();
  const { tool, starts, cancels } = openTool(env);
  tool.setEndpoint('a', { kind: 'node', pubkey: PK_A });
  tool.setEndpoint('b', { kind: 'node', pubkey: PK_B });
  env.dom.find('tp-move-b').fire('click');
  eq(starts, ['b']);
  env.dom.find('tp-move-b').fire('click');
  assert.strictEqual(cancels.length, 1, 'a second press did not cancel');
  assert.strictEqual(tool.movingSide(), null);
});

atest('a committed move clears the pressed Move button', async () => {
  const env = load();
  const { tool, changes } = openTool(env);
  tool.setEndpoint('a', { kind: 'node', pubkey: PK_A });
  tool.setEndpoint('b', { kind: 'node', pubkey: PK_B });
  tool.beginMove('a');
  tool.moveEndpoint('a', { kind: 'point', lat: 56.3, lon: 10.1 });
  assert.strictEqual(tool.movingSide(), null);
  assert.strictEqual(env.dom.find('tp-move-a').attrs['aria-pressed'], 'false');
  assert.strictEqual(changes.length, 1, 'the host was not told to redraw');
});

// ── 5. Swap ────────────────────────────────────────────────────────────────

console.log('\n=== #721: swap A and B ===');

atest('swap exchanges the ends with their heights and measures once', async () => {
  const env = load();
  const { tool, fetchJSON, changes } = openTool(env);
  tool.setState(env.TP.parseState('profile=' + PK_A + ',56.10000,10.20000&model=dtm&ha=12&hb=4'));
  await sleep(0);
  env.dom.find('tp-swap').fire('click');
  const st = tool.getState();
  eq(st.a, { kind: 'point', lat: 56.1, lon: 10.2 });
  eq(st.b, { kind: 'node', pubkey: PK_A });
  assert.strictEqual(st.ha, 4);
  assert.strictEqual(st.hb, 12);
  assert.strictEqual(st.model, 'dtm');
  assert.strictEqual(env.dom.find('tp-h-a').value, '4', 'the A input still shows the old height');
  assert.strictEqual(changes.length, 1, 'the host was not told to redraw');
  await sleep(SETTLE_MS);
  assert.strictEqual(fetchJSON.calls.length, 2, 'swap issued ' + (fetchJSON.calls.length - 1) + ' requests');
  assert.strictEqual(fetchJSON.params(1).get('a'), '56.10000,10.20000');
  assert.strictEqual(fetchJSON.params(1).get('ha'), '4');
  assert.strictEqual(env.dom.find('tp-hsrc-a').attrs['data-source'], 'link', 'the badge did not travel with the height');
});

atest('swap moves the height-source badges with the heights (typed vs link)', async () => {
  const env = load();
  const { tool } = openTool(env);
  // B's height comes from the link; A's is typed here ("yours").
  tool.setState(env.TP.parseState('profile=' + PK_A + ',' + PK_B + '&hb=4'));
  await sleep(0);
  env.dom.find('tp-h-a-plus').fire('click');
  await sleep(SETTLE_MS);
  assert.strictEqual(env.dom.find('tp-hsrc-a').attrs['data-source'], 'yours', 'setup: A is not "yours"');
  assert.strictEqual(env.dom.find('tp-hsrc-b').attrs['data-source'], 'link', 'setup: B is not "link"');
  env.dom.find('tp-swap').fire('click');
  await sleep(SETTLE_MS);
  assert.strictEqual(env.dom.find('tp-hsrc-a').attrs['data-source'], 'link', 'the link badge stayed on the old A');
  assert.strictEqual(env.dom.find('tp-hsrc-b').attrs['data-source'], 'yours', 'the "yours" badge stayed on the old B');
});

// ── 5b. Moving an end that stands on a customizer (M6) default ────────────

console.log('\n=== #725: a moved end drops the old end’s customizer default ===');

const PK_D = 'd'.repeat(64);
// What the stubbed server knows about each node: its role and how it answers.
const NODE_KIND = {
  [PK_A]: { role: 'repeater' },
  [PK_B]: { role: 'repeater' },
  [PK_C]: { role: 'companion' },
  [PK_D]: { role: 'repeater', source: 'operator-node', antennaM: 20 },
};
function themedFetch() {
  const calls = [];
  const fn = (url) => {
    calls.push(url);
    const q = new URLSearchParams(url.split('?')[1]);
    const end = (side) => {
      const ref = q.get(side), h = q.get('h' + side);
      const k = NODE_KIND[ref] || { role: 'repeater' };
      if (h !== null) return { kind: 'node', role: k.role, groundM: 0, antennaM: Number(h), antennaSource: 'request', assumed: false };
      const src = k.source || 'role-default';
      return { kind: 'node', role: k.role, groundM: 0, antennaM: k.antennaM || 10, antennaSource: src, assumed: src === 'role-default' };
    };
    return Promise.resolve({
      a: end('a'), b: end('b'), distanceM: 1000, model: q.get('model'),
      coverage: { state: 'full', pct: 100 },
      samples: [{ d: 0, dtm: 0 }, { d: 500, dtm: 0 }, { d: 1000, dtm: 0 }],
      physics: { freqMHz: 869.618, freqSource: 'firmware', lambdaM: 0.34474, k: 1, f1MaxM: 9 },
      verdict: { state: 'clear', criterion: 0.6 },
    });
  };
  fn.calls = calls;
  fn.params = (i) => new URLSearchParams(calls[i].split('?')[1]);
  return fn;
}
function openThemed() {
  const env = load();
  env.win.SITE_CONFIG = { terrain: { heightRepeater: 7, heightCompanion: 3 } };
  const fetchJSON = themedFetch();
  const tool = env.TP.create({
    mount: env.dom.body, fetchJSON,
    resolveNode: (pk) => ({ name: pk.slice(0, 4), lat: 56, lon: 10, role: (NODE_KIND[pk] || {}).role || 'repeater' }),
    onMoveStart() {}, onMoveCancel() {}, onEndpointsChange() {},
  });
  return { env, tool, fetchJSON };
}
const lastParams = (f) => f.params(f.calls.length - 1);

atest('moving B off a customizer default gives the new end its own default and badge', async () => {
  const { env, tool, fetchJSON } = openThemed();
  tool.setEndpoint('a', { kind: 'node', pubkey: PK_A });
  tool.setEndpoint('b', { kind: 'node', pubkey: PK_B });
  await sleep(SETTLE_MS);
  assert.strictEqual(lastParams(fetchJSON).get('hb'), '7', 'setup: the repeater default was not adopted on B');
  assert.strictEqual(env.dom.find('tp-hsrc-b').attrs['data-source'], 'theme', 'setup: B is not on a customizer default');
  tool.moveEndpoint('b', { kind: 'node', pubkey: PK_C });   // a companion: customizer default 3
  assert.strictEqual(tool.getState().hb, null, 'a customizer default leaked into the shared state');
  await sleep(SETTLE_MS);
  assert.strictEqual(lastParams(fetchJSON).get('b'), PK_C);
  assert.strictEqual(lastParams(fetchJSON).get('hb'), '3', 'B was measured with ' + lastParams(fetchJSON).get('hb') + ', not the new end’s own default');
  assert.strictEqual(lastParams(fetchJSON).get('ha'), '7', 'A lost its own default');
  assert.strictEqual(env.dom.find('tp-hsrc-b').attrs['data-source'], 'theme');
  assert.strictEqual(env.dom.find('tp-h-b').value, '3', 'the B input does not show the new default');
});

atest('moving B off a customizer default onto an operator height does not leave it "assumed"', async () => {
  const { env, tool, fetchJSON } = openThemed();
  tool.setEndpoint('a', { kind: 'node', pubkey: PK_A });
  tool.setEndpoint('b', { kind: 'node', pubkey: PK_B });
  await sleep(SETTLE_MS);
  assert.strictEqual(env.dom.find('tp-hsrc-b').attrs['data-source'], 'theme', 'setup: B is not on a customizer default');
  tool.moveEndpoint('b', { kind: 'node', pubkey: PK_D });   // the server has a real operator height for D
  await sleep(SETTLE_MS);
  assert.strictEqual(lastParams(fetchJSON).get('hb'), null, 'the old default was sent for the new end');
  assert.strictEqual(env.dom.find('tp-hsrc-b').attrs['data-source'], 'operator', 'B badge is ' + env.dom.find('tp-hsrc-b').attrs['data-source']);
  assert.strictEqual(env.dom.find('tp-assumed-b').classList.contains('is-assumed'), false,
    'the new end is still marked as an assumed height (stale "theme" origin)');
});

// ── 6. The buttons exist only when the host can pick ──────────────────────

console.log('\n=== #721: Move A / Move B / Swap in the panel ===');

test('a host without a picker gets no Move buttons', () => {
  const env = load();
  const tool = env.TP.create({ mount: env.dom.body, fetchJSON: recordingFetch(), syncHash: false });
  tool.open();
  assert.strictEqual(env.dom.find('tp-move-a'), null, 'Move A built with nothing to arm');
  assert.strictEqual(env.dom.find('tp-move-b'), null);
  assert.ok(env.dom.find('tp-swap'), 'swap needs no picker and should still be offered');
});

test('the Move and Swap buttons are labelled buttons, disabled until both ends exist', () => {
  const env = load();
  const { tool } = openTool(env);
  tool.setEndpoint('a', { kind: 'node', pubkey: PK_A });
  const ma = env.dom.find('tp-move-a'), sw = env.dom.find('tp-swap');
  assert.strictEqual(ma.tagName, 'BUTTON');
  assert.strictEqual(ma.type, 'button');
  assert.strictEqual(ma.textContent, 'Move A');
  assert.strictEqual(env.dom.find('tp-move-b').textContent, 'Move B');
  assert.ok(/swap/i.test(sw.attrs['aria-label'] || ''), 'the swap button has no accessible name');
  assert.strictEqual(ma.disabled, true, 'Move A is live while B is still being picked');
  assert.strictEqual(sw.disabled, true, 'Swap is live with one end');
  tool.setEndpoint('b', { kind: 'node', pubkey: PK_B });
  assert.strictEqual(ma.disabled, false);
  assert.strictEqual(sw.disabled, false);
});

test('beginMove is refused while only one end exists', () => {
  const env = load();
  const { tool, starts } = openTool(env);
  tool.setEndpoint('a', { kind: 'node', pubkey: PK_A });
  tool.beginMove('b');
  eq(starts, []);
  assert.strictEqual(tool.movingSide(), null);
});

test('closing the panel drops a pending move', () => {
  const env = load();
  const { tool } = openTool(env);
  tool.setEndpoint('a', { kind: 'node', pubkey: PK_A });
  tool.setEndpoint('b', { kind: 'node', pubkey: PK_B });
  tool.beginMove('b');
  tool.close();
  assert.strictEqual(tool.movingSide(), null);
});

// ── Summary ────────────────────────────────────────────────────────────────

(async () => {
  for (const [name, fn] of asyncTests) {
    try { await fn(); passed++; console.log('  ✅ ' + name); }
    catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
  }
  console.log(`\n#721 unit: ${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
