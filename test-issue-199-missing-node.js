/* Issue #199: the node page must not dead-end on a bare "Node not found" for
 * a device the instance still knows about.
 *
 * A 404 from /api/nodes/{pubkey} carries the key's inactive_nodes row
 * (retired after retention.nodeDays without an advert) and/or its observer
 * row. This test loads the real public/app.js and public/nodes.js in a vm
 * sandbox and checks:
 *   1. api() attaches a JSON error body to the thrown error (err.body);
 *   2. the node page's missing-node view explains the inactive state with
 *      the inactive row's name and role, links the observer, escapes
 *      node-controlled text, and falls back (null) for an unknown key.
 */
'use strict';
const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

let passed = 0, failed = 0;
async function test(name, fn) {
  try { await fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

function makeSandbox(fetchImpl) {
  const ctx = {
    window: { addEventListener: () => {}, dispatchEvent: () => {} },
    document: {
      readyState: 'complete',
      createElement: () => ({ id: '', textContent: '', innerHTML: '' }),
      head: { appendChild: () => {} },
      body: { appendChild: () => {} },
      getElementById: () => null,
      addEventListener: () => {},
      querySelectorAll: () => [],
      querySelector: () => null,
    },
    console, Date, Infinity, Math, Array, Object, String, Number, JSON, RegExp,
    Error, TypeError, parseInt, parseFloat, isNaN, isFinite,
    encodeURIComponent, decodeURIComponent,
    setTimeout, clearTimeout,
    setInterval: () => {}, clearInterval: () => {},
    fetch: fetchImpl || (() => Promise.resolve({ ok: true, status: 200, headers: { get: () => null }, json: () => Promise.resolve({}) })),
    performance: { now: () => Date.now() },
    localStorage: (() => {
      const store = {};
      return {
        getItem: k => store[k] || null,
        setItem: (k, v) => { store[k] = String(v); },
        removeItem: k => { delete store[k]; },
      };
    })(),
    location: { hash: '' },
    CustomEvent: class CustomEvent {},
    Map, Set, Promise, URLSearchParams,
    addEventListener: () => {}, dispatchEvent: () => {},
    requestAnimationFrame: (cb) => setTimeout(cb, 0),
  };
  ctx.getHashParams = function () { return new URLSearchParams(''); };
  ctx.registerPage = () => {};
  ctx.RegionFilter = { init: () => {}, getSelected: () => null, onRegionChange: () => {} };
  ctx.onWS = () => {};
  ctx.offWS = () => {};
  ctx.invalidateApiCache = () => {};
  ctx.favStar = () => '';
  ctx.bindFavStars = () => {};
  ctx.getFavorites = () => [];
  ctx.isFavorite = () => false;
  ctx.connectWS = () => {};
  ctx.HopResolver = { init: () => {}, resolve: () => ({}), ready: () => false };
  ctx.CLIENT_TTL = { nodeList: 90000, nodeDetail: 240000, nodeHealth: 240000 };
  ctx.initTabBar = () => {};
  ctx.makeColumnsResizable = () => {};
  ctx.debounce = (fn) => fn;
  vm.createContext(ctx);
  return ctx;
}
function loadInCtx(ctx, file) {
  vm.runInContext(fs.readFileSync(file, 'utf8'), ctx);
  for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k];
}

const PK = 'b199000000000000000000000000000000000000000000000000000000000001';
const OBS_ID = PK.toUpperCase();
const NOT_FOUND = {
  error: 'Not found',
  inactive_node: { public_key: PK, name: 'Quiet Repeater', role: 'repeater', last_seen: '2026-09-24T15:35:00Z', first_seen: '2026-09-11T00:00:00Z' },
  observer: { id: OBS_ID, name: 'Quiet Observer', last_seen: '2026-10-04T04:23:00Z' },
};

(async () => {
  console.log('\n=== issue #199: missing-node explanation ===');

  await test('api() attaches the JSON body of a 404 to the thrown error', async () => {
    const ctx = makeSandbox(() => Promise.resolve({
      ok: false, status: 404, headers: { get: () => null },
      json: () => Promise.resolve(NOT_FOUND),
    }));
    loadInCtx(ctx, __dirname + '/public/app.js');
    let err = null;
    try { await ctx.api('/nodes/' + PK); } catch (e) { err = e; }
    assert.ok(err, 'api() should reject on 404');
    assert.strictEqual(err.status, 404);
    assert.ok(/API 404/.test(err.message), 'message unchanged: ' + err.message);
    assert.ok(err.body && err.body.inactive_node, 'err.body.inactive_node missing');
    assert.strictEqual(err.body.inactive_node.name, 'Quiet Repeater');
  });

  await test('api() still rejects normally when the error body is not JSON', async () => {
    const ctx = makeSandbox(() => Promise.resolve({
      ok: false, status: 500, headers: { get: () => null },
      json: () => Promise.reject(new SyntaxError('Unexpected token')),
    }));
    loadInCtx(ctx, __dirname + '/public/app.js');
    let err = null;
    try { await ctx.api('/broken'); } catch (e) { err = e; }
    assert.ok(err && err.status === 500 && /API 500/.test(err.message), 'got ' + (err && err.message));
    assert.strictEqual(err.body, undefined);
  });

  const ctx = makeSandbox();
  loadInCtx(ctx, __dirname + '/public/roles.js');
  loadInCtx(ctx, __dirname + '/public/app.js');
  loadInCtx(ctx, __dirname + '/public/nodes.js');
  const view = ctx.window._nodesMissingNodeView;

  await test('nodes.js exposes the missing-node view builder', async () => {
    assert.strictEqual(typeof view, 'function');
  });

  await test('inactive node: explains the state with name, role and last advert date', async () => {
    const v = view(PK, NOT_FOUND);
    assert.ok(v, 'expected a view for an inactive node');
    assert.ok(/No advert heard since/.test(v.html), 'missing "No advert heard since"');
    assert.ok(/this device is inactive/.test(v.html), 'missing "this device is inactive"');
    assert.ok(v.html.indexOf(ctx.formatAbsoluteTimestamp('2026-09-24T15:35:00Z')) !== -1, 'last advert date not shown');
    assert.ok(v.html.indexOf('Quiet Repeater') !== -1, 'inactive name missing');
    assert.ok(/repeater/i.test(v.html.replace('Quiet Repeater', '')), 'role missing');
    assert.ok(v.title.indexOf('Quiet Repeater') !== -1, 'title should name the device: ' + v.title);
    assert.ok(!/Node not found/.test(v.html), 'must not dead-end on "Node not found"');
  });

  await test('inactive observer: links to its observer page and back to Nodes', async () => {
    const v = view(PK, NOT_FOUND);
    assert.ok(v.html.indexOf('href="#/observers/' + encodeURIComponent(OBS_ID) + '"') !== -1, 'observer link missing');
    assert.ok(v.html.indexOf('href="#/nodes"') !== -1, 'Back to Nodes link missing');
  });

  await test('observer without a node record: explains it and links the observer', async () => {
    const v = view(PK, { error: 'Not found', observer: NOT_FOUND.observer });
    assert.ok(v, 'expected a view for an observer-only device');
    assert.ok(/no node record/i.test(v.html), 'missing "no node record" explanation');
    assert.ok(/no advert from it has been heard/i.test(v.html), 'explanation must say why: no advert heard');
    assert.ok(!/this device is inactive/.test(v.html), 'observer-only must not claim an inactive row');
    assert.ok(v.html.indexOf('Quiet Observer') !== -1, 'observer name missing');
    assert.ok(v.html.indexOf('href="#/observers/' + encodeURIComponent(OBS_ID) + '"') !== -1, 'observer link missing');
  });

  await test('unknown key: no view, so the generic "Node not found" stays', async () => {
    assert.strictEqual(view(PK, { error: 'Not found' }), null);
    assert.strictEqual(view(PK, undefined), null);
    assert.strictEqual(view(PK, null), null);
  });

  await test('node-controlled names and roles are escaped', async () => {
    const evil = '<img src=x onerror=alert(1)>';
    const v = view(PK, {
      inactive_node: { public_key: PK, name: evil, role: evil, last_seen: '2026-09-24T15:35:00Z' },
      observer: { id: '"><script>x</script>', name: evil, last_seen: '2026-10-04T04:23:00Z' },
    });
    assert.ok(v.html.indexOf('<img') === -1, 'unescaped <img> in html');
    assert.ok(v.html.indexOf('<script') === -1, 'unescaped <script> in html');
  });

  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})();
