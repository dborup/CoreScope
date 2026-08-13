/**
 * Regression test for the #1889 review's core structural fix: the Nodes
 * table and the JSON export must share exactly one display-order
 * computation (public/nodes.js's computeDisplayOrder()), not two
 * independently-maintained copies of the sort/claimed/favorites-pinning
 * logic that could silently drift apart.
 *
 * Drives computeDisplayOrder() via its window._nodesComputeDisplayOrder
 * test hook (same hook family as _nodesGetFiltered/_nodesSortNodes used by
 * test-nodes-geo-scope-filter.js) rather than re-implementing the
 * sort/pin algorithm here — this exercises the actual production
 * function, so a bug or later regression in the real ordering logic shows
 * up here directly instead of only in a parallel reimplementation.
 *
 * test-nodes-export-wiring.js separately proves (via source inspection)
 * that both renderRows() and the export click handler call this SAME
 * function; this file proves the function itself produces the order the
 * review demands: sort column changes reorder, asc/desc is respected,
 * claimed nodes are pinned first, favorites second, and each group keeps
 * its internal sort order.
 */
'use strict';
const vm = require('vm');
const fs = require('fs');
const path = require('path');
const assert = require('assert');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}

function loadInCtx(ctx, file) {
  vm.runInContext(fs.readFileSync(file, 'utf8'), ctx);
  for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k];
}

function makeSandbox() {
  const ctx = {
    window: { addEventListener: () => {}, dispatchEvent: () => {} },
    document: {
      readyState: 'complete',
      createElement: () => ({ id: '', textContent: '', innerHTML: '', style: {}, classList: { add(){}, remove(){}, toggle(){}, contains(){return false;} }, appendChild(){}, addEventListener(){} }),
      head: { appendChild: () => {} },
      getElementById: () => null,
      addEventListener: () => {},
      removeEventListener: () => {},
      querySelectorAll: () => [],
      querySelector: () => null,
    },
    console, Date, Infinity, Math, Array, Object, String, Number, JSON, RegExp, Error, TypeError,
    parseInt, parseFloat, isNaN, isFinite, encodeURIComponent, decodeURIComponent,
    setTimeout: (fn) => { fn(); return 0; }, clearTimeout: () => {},
    setInterval: () => 0, clearInterval: () => {},
    Promise, Map, Set, URLSearchParams,
    fetch: () => Promise.resolve({ json: () => Promise.resolve({}) }),
    performance: { now: () => Date.now() },
    localStorage: (() => {
      const store = {};
      return { getItem: k => (k in store ? store[k] : null), setItem: (k, v) => { store[k] = String(v); }, removeItem: k => { delete store[k]; } };
    })(),
    location: { hash: '' },
    getHashParams: function () { return new URLSearchParams((ctx.location.hash.split('?')[1] || '')); },
    CustomEvent: class CustomEvent {},
  };
  vm.createContext(ctx);
  return ctx;
}

function makeNodesEnv() {
  const ctx = makeSandbox();
  const domElements = {};
  function getEl(id) {
    if (!domElements[id]) {
      domElements[id] = {
        id, innerHTML: '', textContent: '', value: '', scrollTop: 0,
        style: {}, dataset: {},
        classList: { add(){}, remove(){}, toggle(){}, contains(){return false;} },
        addEventListener() {}, querySelectorAll() { return []; }, querySelector() { return null; },
        getAttribute() { return null; }, setAttribute() {}, appendChild() {},
      };
    }
    return domElements[id];
  }
  ctx.document.getElementById = getEl;

  ctx.api = function () { return Promise.resolve({ nodes: [], total: 0, counts: {} }); };
  ctx.invalidateApiCache = () => {};
  ctx.nodePassesGeoFilter = () => true;
  ctx.window.MC_GEO_FILTER = null;
  ctx.ROLE_COLORS = { repeater: '#0', room: '#0', companion: '#0', sensor: '#0' };
  ctx.ROLE_STYLE = {};
  ctx.TYPE_COLORS = {};
  ctx.getNodeStatus = () => 'active';
  ctx.getHealthThresholds = () => ({ staleMs: 1, degradedMs: 1, silentMs: 1 });
  ctx.timeAgo = () => '';
  ctx.truncate = (s) => s;
  ctx.escapeHtml = (s) => String(s || '');
  ctx.payloadTypeName = () => '';
  ctx.payloadTypeColor = () => '';
  ctx.debounce = (fn) => fn;
  ctx.initTabBar = () => {};
  // Real localStorage-backed getFavorites (the actual public/app.js
  // implementation), not the always-empty stub other nodes.js tests use —
  // this file specifically needs to control favorites per-test.
  ctx.getFavorites = function () {
    try { return JSON.parse(ctx.localStorage.getItem('meshcore-favorites') || '[]'); } catch (e) { return []; }
  };
  ctx.favStar = () => '';
  ctx.bindFavStars = () => {};
  ctx.makeColumnsResizable = () => {};
  ctx.CLIENT_TTL = { nodeList: 0, nodeDetail: 0, nodeHealth: 0 };
  ctx.RegionFilter = { init(){}, onChange(){ return () => {}; }, offChange(){}, getRegionParam(){ return ''; } };
  ctx.AreaFilter = { init(){}, onChange(){ return () => {}; }, offChange(){}, getAreaParam(){ return ''; }, getSelected(){ return null; } };
  ctx.getFleetSkew = () => Promise.resolve({});
  ctx.onWS = () => {};
  ctx.offWS = () => {};
  ctx.debouncedOnWS = () => () => {};
  let pageMod = null;
  ctx.registerPage = (name, handlers) => { pageMod = handlers; };

  loadInCtx(ctx, path.join(__dirname, 'public/nodes.js'));

  return { ctx, pageMod: () => pageMod };
}

function setClaimed(ctx, pubkeys) {
  ctx.localStorage.setItem('meshcore-my-nodes', JSON.stringify(pubkeys.map(pk => ({ pubkey: pk }))));
}
function setFavorites(ctx, pubkeys) {
  ctx.localStorage.setItem('meshcore-favorites', JSON.stringify(pubkeys));
}
function setSort(ctx, column, direction) {
  ctx._nodesSetSortState({ column: column, direction: direction });
}

function node(id, patch) {
  return Object.assign({
    public_key: id.padEnd(64, '0'),
    name: id,
    role: 'repeater',
    last_seen: '2026-01-01T00:00:00Z',
    advert_count: 1,
  }, patch || {});
}

console.log('=== computeDisplayOrder(): shared order for table + export (#1889 review fix 1) ===');

test('sort column change reorders the result (name asc vs. advert_count desc)', () => {
  const { ctx } = makeNodesEnv();
  const fixture = [
    node('bravo', { name: 'Bravo', advert_count: 5 }),
    node('alpha', { name: 'Alpha', advert_count: 1 }),
    node('charlie', { name: 'Charlie', advert_count: 9 }),
  ];

  setSort(ctx, 'name', 'asc');
  const byName = ctx._nodesComputeDisplayOrder(fixture.slice());
  assert.strictEqual(byName.map(n => n.name).join(','), 'Alpha,Bravo,Charlie', 'sorted by name asc');

  setSort(ctx, 'advert_count', 'desc');
  const byAdverts = ctx._nodesComputeDisplayOrder(fixture.slice());
  assert.strictEqual(byAdverts.map(n => n.name).join(','), 'Charlie,Bravo,Alpha', 'sorted by advert_count desc');

  assert.notStrictEqual(byName.map(n => n.name).join(','), byAdverts.map(n => n.name).join(','),
    'changing the sort column actually changes the computed order');
});

test('asc/desc direction is respected for the same column', () => {
  const { ctx } = makeNodesEnv();
  const fixture = [
    node('a', { name: 'Alpha', advert_count: 1 }),
    node('b', { name: 'Bravo', advert_count: 5 }),
    node('c', { name: 'Charlie', advert_count: 9 }),
  ];

  setSort(ctx, 'advert_count', 'asc');
  const asc = ctx._nodesComputeDisplayOrder(fixture.slice()).map(n => n.name).join(',');
  setSort(ctx, 'advert_count', 'desc');
  const desc = ctx._nodesComputeDisplayOrder(fixture.slice()).map(n => n.name).join(',');

  assert.strictEqual(asc, 'Alpha,Bravo,Charlie');
  assert.strictEqual(desc, 'Charlie,Bravo,Alpha');
});

test('claimed ("My Mesh") nodes are pinned first, regardless of sort column', () => {
  const { ctx } = makeNodesEnv();
  const fixture = [
    node('zulu', { name: 'Zulu' }),
    node('mike', { name: 'MyClaimed' }),
    node('alpha', { name: 'Alpha' }),
  ];
  setClaimed(ctx, [fixture[1].public_key]);
  setSort(ctx, 'name', 'asc'); // would otherwise put Alpha first

  // .join(',') rather than deepStrictEqual/array-slice comparisons: arrays
  // built inside the vm sandbox propagate the sandbox's own Array species
  // through .map()/.slice(), which fails Node's host-realm identity check
  // even when the actual contents match (see test-nodes-export.js).
  const order = ctx._nodesComputeDisplayOrder(fixture.slice()).map(n => n.name).join(',');
  assert.strictEqual(order, 'MyClaimed,Alpha,Zulu',
    'the claimed node leads even though it sorts last alphabetically; the rest keep the active column sort');
});

test('favorites are pinned second, after claimed, before the rest', () => {
  const { ctx } = makeNodesEnv();
  const fixture = [
    node('zulu', { name: 'Zulu' }),
    node('fave', { name: 'MyFavorite' }),
    node('claim', { name: 'MyClaimed' }),
    node('alpha', { name: 'Alpha' }),
  ];
  setClaimed(ctx, [fixture[2].public_key]);
  setFavorites(ctx, [fixture[1].public_key]);
  setSort(ctx, 'name', 'asc');

  const order = ctx._nodesComputeDisplayOrder(fixture.slice()).map(n => n.name).join(',');
  assert.strictEqual(order, 'MyClaimed,MyFavorite,Alpha,Zulu',
    'claimed first, then favorite, then the remaining rows in column-sort order');
});

test('a node that is both claimed and favorited is grouped with claimed (not double-counted)', () => {
  const { ctx } = makeNodesEnv();
  const fixture = [
    node('zulu', { name: 'Zulu' }),
    node('both', { name: 'ClaimedAndFav' }),
  ];
  setClaimed(ctx, [fixture[1].public_key]);
  setFavorites(ctx, [fixture[1].public_key]);
  setSort(ctx, 'name', 'asc');

  const order = ctx._nodesComputeDisplayOrder(fixture.slice()).map(n => n.name).join(',');
  assert.strictEqual(order, 'ClaimedAndFav,Zulu');
});

test('multiple claimed nodes keep the active column sort within the claimed group', () => {
  const { ctx } = makeNodesEnv();
  const fixture = [
    node('c1', { name: 'ClaimedC', advert_count: 1 }),
    node('c2', { name: 'ClaimedA', advert_count: 9 }),
    node('plain', { name: 'Plain', advert_count: 5 }),
  ];
  setClaimed(ctx, [fixture[0].public_key, fixture[1].public_key]);
  setSort(ctx, 'advert_count', 'desc');

  const order = ctx._nodesComputeDisplayOrder(fixture.slice()).map(n => n.name).join(',');
  assert.strictEqual(order, 'ClaimedA,ClaimedC,Plain',
    'both claimed nodes lead (sorted desc by advert_count within the group), plain node trails');
});

test('does not mutate its input array', () => {
  const { ctx } = makeNodesEnv();
  const fixture = [node('b', { name: 'B' }), node('a', { name: 'A' })];
  const original = fixture.slice();
  setSort(ctx, 'name', 'asc');
  ctx._nodesComputeDisplayOrder(fixture);
  assert.deepStrictEqual(fixture.map(n => n.name), original.map(n => n.name),
    'the array passed in is left in its original order');
});

console.log('\n' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
