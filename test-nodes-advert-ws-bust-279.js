/**
 * #279: the nodes advert WebSocket handler must not join an older in-flight
 * /nodes request.
 *
 * public/nodes.js auto-refreshes the node list when ADVERT packets arrive
 * (#131). Both of the handler's refresh paths did
 * `invalidateApiCache('/nodes')` followed by a plain `loadNodes(true)`.
 * `invalidateApiCache()` clears only the TTL cache, not api()'s `_inflight`
 * map, so if a /nodes request was already in flight the refresh coalesced
 * onto it. That request may have been answered by the server before the
 * advert arrived, so the list rendered without the node that just
 * advertised. This is the same shape as the #243 /channels bug, and the
 * #243 fix lives at the call site (`api(..., { bust: true })`), not inside
 * `invalidateApiCache()` — so it did not cover this path (N3 of the #263
 * round-2 review).
 *
 * The tests below load the real public/nodes.js *and* the real public/app.js,
 * so api()'s in-flight dedup, TTL cache and `bust` are the production ones.
 * Every /api/nodes fetch is parked on its own deferred, so each test decides
 * when — and in which order — the server's answers land.
 *
 * Usage: node test-nodes-advert-ws-bust-279.js
 */
'use strict';

const vm = require('vm');
const fs = require('fs');
const path = require('path');
const assert = require('assert');

const REPO = __dirname;

function deferred() {
  let resolve, reject;
  const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

// nodes.js's load path has several awaits per page (fetch, .json(), the
// pagination loop, getFleetSkew, MeshConfigReady), so drain generously.
function flush(n) {
  let p = Promise.resolve();
  for (let i = 0; i < (n || 40); i++) p = p.then(() => new Promise((r) => setImmediate(r)));
  return p;
}

function advertFor(pubKey) {
  return {
    type: 'packet',
    data: {
      packet: { payload_type: 4, timestamp: new Date().toISOString() },
      decoded: { header: { payloadTypeName: 'ADVERT' }, payload: { pubKey } },
    },
  };
}

function node(pubKey, name) {
  return {
    public_key: pubKey, name, role: 'repeater', advert_count: 1,
    last_seen: new Date().toISOString(),
  };
}

const KEY_A = 'a'.repeat(64);
const KEY_B = 'b'.repeat(64);

function el(id) {
  return {
    id, innerHTML: '', textContent: '', value: '', scrollTop: 0,
    style: {}, dataset: {},
    classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } },
    addEventListener() {}, removeEventListener() {},
    querySelectorAll() { return []; }, querySelector() { return null; },
    getAttribute() { return null; }, setAttribute() {}, appendChild() {},
    closest() { return null; }, contains() { return false; },
  };
}

// A nodes page wired to the real app.js api(). Only fetch is stubbed:
// /api/nodes?... requests are parked, everything else answers {} at once so
// app.js's own config fetches do not hang.
function makeHarness() {
  const parked = [];
  const els = {};
  const ctx = {
    window: { addEventListener() {}, removeEventListener() {}, dispatchEvent() {} },
    console, Date, Infinity, Math, Array, Object, String, Number, JSON, RegExp, Error, TypeError,
    parseInt, parseFloat, isNaN, isFinite, encodeURIComponent, decodeURIComponent,
    Promise, Map, Set, URLSearchParams,
    setTimeout: (fn) => setTimeout(fn, 0), clearTimeout: () => {},
    setInterval: () => 0, clearInterval: () => {},
    requestAnimationFrame: (fn) => { fn(); return 1; }, cancelAnimationFrame: () => {},
    performance: { now: () => Date.now() },
    location: { hash: '' },
    CustomEvent: class CustomEvent {},
    localStorage: (() => {
      const store = {};
      return {
        getItem: (k) => (k in store ? store[k] : null),
        setItem: (k, v) => { store[k] = String(v); },
        removeItem: (k) => { delete store[k]; },
      };
    })(),
  };
  ctx.document = {
    readyState: 'complete',
    head: { appendChild() {} },
    body: el('body'),
    addEventListener() {}, removeEventListener() {},
    querySelectorAll: () => [], querySelector: () => null,
    createElement: () => el('created'),
    getElementById: (id) => { if (!els[id]) els[id] = el(id); return els[id]; },
  };
  ctx.addEventListener = () => {};
  ctx.dispatchEvent = () => {};
  ctx.getHashParams = () => new URLSearchParams((ctx.location.hash.split('?')[1] || ''));
  ctx.fetch = function (url) {
    if (url.indexOf('/api/nodes') !== 0) {
      return Promise.resolve({ ok: true, status: 200, json: async () => ({}), headers: { get: () => null } });
    }
    const d = deferred();
    parked.push({
      url,
      answer: (body) => d.resolve({ ok: true, status: 200, json: async () => body, headers: { get: () => null } }),
    });
    return d.promise;
  };
  vm.createContext(ctx);

  function load(file) {
    vm.runInContext(fs.readFileSync(path.join(REPO, file), 'utf8'), ctx, { filename: file });
    for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k];
  }

  load('public/roles.js');
  // The real api(): in-flight dedup, TTL cache, and the #243 `bust`.
  load('public/app.js');

  // Everything nodes.js reaches for that is not under test.
  const h = { ctx, parked, els, regionParam: '', wsHandler: null, pageMod: null };
  ctx.RegionFilter = { init() {}, onChange() { return () => {}; }, offChange() {}, getRegionParam: () => h.regionParam };
  ctx.AreaFilter = { init() {}, onChange() { return () => {}; }, offChange() {}, getAreaParam: () => '' };
  ctx.onWS = () => {};
  ctx.offWS = () => {};
  // Capture the advert handler instead of debouncing it for 5s.
  ctx.debouncedOnWS = (fn) => { h.wsHandler = fn; return fn; };
  ctx.getFleetSkew = () => Promise.resolve({});
  ctx.HopResolver = { init() {}, resolve: () => ({}), ready: () => false };
  ctx.connectWS = () => {};
  ctx.initTabBar = () => {};
  ctx.makeColumnsResizable = () => {};
  ctx.debounce = (fn) => fn;
  ctx.getFavorites = () => [];
  ctx.isFavorite = () => false;
  ctx.favStar = () => '';
  ctx.bindFavStars = () => {};
  ctx.registerPage = (name, mod) => { if (name === 'nodes') h.pageMod = mod; };

  load('public/nodes.js');

  h.fetchesFor = (p) => parked.filter((f) => f.url.indexOf('/api' + p) === 0);
  // app.js's `api` / `CLIENT_TTL` are top-level lexical bindings of the
  // sandbox script, not properties of its global object, so reach them the
  // way nodes.js does -- from inside the context.
  h.apiCall = (p) => vm.runInContext(
    'api(' + JSON.stringify(p) + ', { ttl: CLIENT_TTL.nodeList })', ctx);
  h.allNodes = () => ctx.window._nodesGetAllNodes();
  h.names = () => (h.allNodes() || []).map((n) => n.name).sort();
  h.setAllNodes = (n) => ctx.window._nodesSetAllNodes(n);
  h.init = () => h.pageMod.init(ctx.document.getElementById('page'));
  return h;
}

let passed = 0, failed = 0;
async function test(name, fn) {
  try { await fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + (e && e.message)); }
}

(async () => {
  console.log('\n=== #279 the nodes advert WS refresh does not join an in-flight /nodes request ===');

  // Sanity: the harness really is driving the production advert handler.
  await test('the page mounts an advert WS handler and the first load parks one /nodes request', async () => {
    const h = makeHarness();
    h.init();
    await flush();
    assert.strictEqual(typeof h.wsHandler, 'function', 'init() must register a debouncedOnWS handler');
    assert.strictEqual(h.fetchesFor('/nodes?').length, 1,
      'the initial load makes exactly one request (got ' + h.fetchesFor('/nodes?').length + ')');
    assert.ok(h.ctx.window._nodesIsAdvertMessage(advertFor(KEY_B)), 'the fixture message is an ADVERT');
  });

  // Path 1 — nodes.js:~684. An advert arrives while the very first load is
  // still in flight, so `_allNodes` is still null. Its answer was produced
  // before the advert, so the refresh must fetch again rather than join it.
  // The older answer lands first, so only the bust's own request can decide
  // what the list ends up holding.
  await test('an advert during the first load refetches instead of joining it (_allNodes null path)', async () => {
    const h = makeHarness();
    h.init();
    await flush();
    const before = h.fetchesFor('/nodes?');
    assert.strictEqual(before.length, 1, 'the initial load is in flight');

    h.wsHandler([advertFor(KEY_B)]);
    await flush();
    const after = h.fetchesFor('/nodes?');
    assert.strictEqual(after.length, 2,
      'the advert refresh must start its own /nodes request instead of joining the in-flight one (got ' +
      after.length + ')');
    assert.strictEqual(after[0].url, after[1].url, 'both requests are for the same page of the same list');

    // Older answer first (pre-advert), then the refresh's own answer.
    after[0].answer({ nodes: [node(KEY_A, 'Alpha')], counts: {}, total: 1 });
    await flush();
    after[1].answer({ nodes: [node(KEY_A, 'Alpha'), node(KEY_B, 'Bravo')], counts: {}, total: 2 });
    await flush();

    assert.deepStrictEqual(h.names(), ['Alpha', 'Bravo'],
      'the node that just advertised must be in the list (got ' + JSON.stringify(h.names()) + ')');
  });

  // Path 2 — nodes.js:~711. `_allNodes` is populated and the advert names a
  // node that is not in it, so the handler drops the cache and reloads. A
  // /nodes request for the same page can still be in flight: two region
  // switches in quick succession leave two loads running, and the one that
  // answers first is the one that populates `_allNodes`.
  await test('an advert for an unknown node refetches instead of joining an in-flight load (needReload path)', async () => {
    const h = makeHarness();
    h.init();
    await flush();
    const inflight = h.fetchesFor('/nodes?');
    assert.strictEqual(inflight.length, 1, 'a load is in flight');
    // A load that already finished left this list behind.
    h.setAllNodes([node(KEY_A, 'Alpha')]);

    h.wsHandler([advertFor(KEY_B)]);
    await flush();
    const after = h.fetchesFor('/nodes?');
    assert.strictEqual(after.length, 2,
      'the reload must start its own /nodes request instead of joining the in-flight one (got ' +
      after.length + ')');

    after[0].answer({ nodes: [node(KEY_A, 'Alpha')], counts: {}, total: 1 });
    await flush();
    after[1].answer({ nodes: [node(KEY_A, 'Alpha'), node(KEY_B, 'Bravo')], counts: {}, total: 2 });
    await flush();

    assert.deepStrictEqual(h.names(), ['Alpha', 'Bravo'],
      'the unknown node must have arrived with the reload (got ' + JSON.stringify(h.names()) + ')');
  });

  // The bust takes the in-flight slot (#243), so a load started after it
  // joins the newer request rather than fetching a third time.
  await test('a load started after the advert refresh joins the refresh, not the superseded request', async () => {
    const h = makeHarness();
    h.init();
    await flush();
    h.wsHandler([advertFor(KEY_B)]);
    await flush();
    const after = h.fetchesFor('/nodes?');
    assert.strictEqual(after.length, 2, 'the refresh fetched (got ' + after.length + ')');
    // A region change with the same (empty) region param re-runs the load.
    const joined = h.apiCall(after[1].url.slice('/api'.length));
    await flush();
    assert.strictEqual(h.fetchesFor('/nodes?').length, 2,
      'a later plain load must join the refresh request (got ' + h.fetchesFor('/nodes?').length + ')');
    after[1].answer({ nodes: [node(KEY_B, 'Bravo')], counts: {}, total: 1 });
    const data = await joined;
    assert.deepStrictEqual(data.nodes.map((n) => n.name), ['Bravo'], 'it got the refresh answer');
  });

  // Guard: an advert that only updates a node already in the list is
  // handled in place and must not make a request at all.
  await test('an advert for a known node updates it in place and makes no request', async () => {
    const h = makeHarness();
    h.init();
    await flush();
    h.fetchesFor('/nodes?')[0].answer({ nodes: [node(KEY_A, 'Alpha')], counts: {}, total: 1 });
    await flush();
    assert.deepStrictEqual(h.names(), ['Alpha'], 'the first load settled');
    const beforeCount = h.fetchesFor('/nodes?').length;

    const m = advertFor(KEY_A);
    m.data.decoded.payload.name = 'Alpha renamed';
    h.wsHandler([m]);
    await flush();
    assert.strictEqual(h.fetchesFor('/nodes?').length, beforeCount,
      'a known-node advert must not make a /nodes request (got ' +
      (h.fetchesFor('/nodes?').length - beforeCount) + ' extra)');
    assert.deepStrictEqual(h.names(), ['Alpha renamed'], 'the advert was applied in place');
  });

  // Guard: with nothing in flight, the refresh still makes exactly one
  // request — `bust` must not turn one refresh into two.
  await test('an advert with nothing in flight makes exactly one /nodes request', async () => {
    const h = makeHarness();
    h.init();
    await flush();
    h.fetchesFor('/nodes?')[0].answer({ nodes: [node(KEY_A, 'Alpha')], counts: {}, total: 1 });
    await flush();
    const before = h.fetchesFor('/nodes?').length;

    h.wsHandler([advertFor(KEY_B)]);
    await flush();
    const after = h.fetchesFor('/nodes?');
    assert.strictEqual(after.length - before, 1,
      'exactly one request for the refresh (got ' + (after.length - before) + ')');
    after[after.length - 1].answer({ nodes: [node(KEY_A, 'Alpha'), node(KEY_B, 'Bravo')], counts: {}, total: 2 });
    await flush();
    assert.deepStrictEqual(h.names(), ['Alpha', 'Bravo'], 'the refresh rendered');
  });

  // Guard: a non-advert batch is ignored entirely.
  await test('a batch with no advert makes no request', async () => {
    const h = makeHarness();
    h.init();
    await flush();
    const before = h.fetchesFor('/nodes?').length;
    h.wsHandler([{ type: 'packet', data: { packet: { payload_type: 1 }, decoded: {} } }]);
    await flush();
    assert.strictEqual(h.fetchesFor('/nodes?').length, before, 'no request for a non-advert batch');
  });

  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})();
