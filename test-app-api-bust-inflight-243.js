/**
 * #243: an explicit refresh, api(path, { bust: true }), must not join an
 * older in-flight request for the same path.
 *
 * api() deduplicates in-flight requests per path. `bust` used to skip only
 * the TTL cache, so a refresh asked for while a request was in flight got
 * that older request's answer, which the server may have produced before
 * the change the caller wants to see (a channel approved a moment ago).
 *
 * Loads the real public/app.js in a vm sandbox; only fetch is stubbed, and
 * every fetch is parked on its own deferred so the test decides the order
 * in which responses land.
 *
 * Usage: node test-app-api-bust-inflight-243.js
 */
'use strict';

const vm = require('vm');
const fs = require('fs');
const path = require('path');
const assert = require('assert');

const APP_JS_PATH = path.join(__dirname, 'public', 'app.js');

function deferred() {
  let resolve, reject;
  const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

function flush(n) {
  let p = Promise.resolve();
  for (let i = 0; i < (n || 10); i++) p = p.then(() => new Promise((r) => setImmediate(r)));
  return p;
}

function makeSandbox() {
  const parked = [];
  const ctx = {
    window: { addEventListener: () => {}, dispatchEvent: () => {} },
    document: {
      readyState: 'complete',
      getElementById: () => null,
      addEventListener: () => {},
      querySelectorAll: () => [],
      querySelector: () => null,
      createElement: () => ({ style: {} }),
      head: { appendChild: () => {} },
    },
    console, Date, Promise, Map, Set, JSON, Math, Error, TypeError, Array, Object, String, Number, RegExp,
    parseInt, parseFloat, isNaN, isFinite, encodeURIComponent, decodeURIComponent,
    setTimeout: (fn) => setTimeout(fn, 0), clearTimeout: () => {},
    setInterval: () => 1, clearInterval: () => {},
    performance: { now: () => Date.now() },
    location: { hash: '' },
    addEventListener: () => {},
    dispatchEvent: () => {},
  };
  ctx.fetch = function (url) {
    // app.js's own top-level config fetch is answered at once.
    if (url.indexOf('/api/config/') === 0) return Promise.resolve({ ok: true, status: 200, json: async () => ({}), headers: { get: () => null } });
    const d = deferred();
    parked.push({ url, d });
    return d.promise;
  };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(APP_JS_PATH, 'utf8'), ctx, { filename: APP_JS_PATH });
  for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k];
  return {
    api: ctx.api,
    invalidateApiCache: ctx.invalidateApiCache,
    parked,
    fetchesFor: (p) => parked.filter((f) => f.url === '/api' + p),
    answer: (entry, body) => entry.d.resolve({ ok: true, status: 200, json: async () => body, headers: { get: () => null } }),
  };
}

let passed = 0, failed = 0;
async function test(name, fn) {
  try { await fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + (e && e.message)); }
}

(async () => {
  console.log('\n=== #243 api(path, { bust: true }) does not join an older in-flight request ===');

  await test('a bust during an in-flight request fetches again and gets the newer data', async () => {
    const h = makeSandbox();
    const older = h.api('/x', { ttl: 15000 });
    await flush();
    const newer = h.api('/x', { ttl: 15000, bust: true });
    await flush();
    const fetches = h.fetchesFor('/x');
    assert.strictEqual(fetches.length, 2, 'the bust must start its own request (got ' + fetches.length + ')');
    h.answer(fetches[1], { v: 'new' });
    h.answer(fetches[0], { v: 'old' });
    assert.deepStrictEqual(await newer, { v: 'new' });
    assert.deepStrictEqual(await older, { v: 'old' }, 'the older caller still gets its own answer');
  });

  await test('a later call without bust joins the newer request, not the older one', async () => {
    const h = makeSandbox();
    h.api('/x');
    await flush();
    const newer = h.api('/x', { bust: true });
    const joined = h.api('/x');
    await flush();
    const fetches = h.fetchesFor('/x');
    assert.strictEqual(fetches.length, 2, 'the plain call must join an in-flight request, not fetch (got ' + fetches.length + ')');
    h.answer(fetches[1], { v: 'new' });
    h.answer(fetches[0], { v: 'old' });
    assert.deepStrictEqual(await joined, { v: 'new' }, 'it joins the bust request');
    assert.deepStrictEqual(await newer, { v: 'new' });
  });

  await test('the older request settling first does not remove the newer in-flight entry', async () => {
    const h = makeSandbox();
    const older = h.api('/x');
    await flush();
    h.api('/x', { bust: true });
    await flush();
    const fetches = h.fetchesFor('/x');
    assert.strictEqual(fetches.length, 2, 'the bust must start its own request (got ' + fetches.length + ')');
    h.answer(fetches[0], { v: 'old' });
    await older;
    await flush();
    const joined = h.api('/x');
    await flush();
    assert.strictEqual(h.fetchesFor('/x').length, 2, 'a plain call while the bust is in flight must join it (got ' + h.fetchesFor('/x').length + ' fetches)');
    h.answer(fetches[1], { v: 'new' });
    assert.deepStrictEqual(await joined, { v: 'new' });
  });

  await test('an older response landing last does not overwrite the newer data in the TTL cache', async () => {
    const h = makeSandbox();
    const older = h.api('/x', { ttl: 15000 });
    await flush();
    const newer = h.api('/x', { ttl: 15000, bust: true });
    await flush();
    const fetches = h.fetchesFor('/x');
    assert.strictEqual(fetches.length, 2, 'the bust must start its own request (got ' + fetches.length + ')');
    h.answer(fetches[1], { v: 'new' });
    await newer;
    h.answer(fetches[0], { v: 'old' });
    await older;
    await flush();
    const cached = await h.api('/x', { ttl: 15000 });
    assert.strictEqual(h.fetchesFor('/x').length, 2, 'served from the cache');
    assert.deepStrictEqual(cached, { v: 'new' }, 'the cache must keep the newer data (got ' + JSON.stringify(cached) + ')');
  });

  await test('a bust with nothing in flight makes exactly one request and fills the cache', async () => {
    const h = makeSandbox();
    const p = h.api('/x', { ttl: 15000, bust: true });
    await flush();
    assert.strictEqual(h.fetchesFor('/x').length, 1);
    h.answer(h.fetchesFor('/x')[0], { v: 1 });
    assert.deepStrictEqual(await p, { v: 1 });
    assert.deepStrictEqual(await h.api('/x', { ttl: 15000 }), { v: 1 });
    assert.strictEqual(h.fetchesFor('/x').length, 1, 'the next plain call is a cache hit');
  });

  await test('guard: calls without bust still share one in-flight request and the TTL cache', async () => {
    const h = makeSandbox();
    const a = h.api('/x', { ttl: 15000 });
    const b = h.api('/x', { ttl: 15000 });
    await flush();
    assert.strictEqual(h.fetchesFor('/x').length, 1, 'concurrent plain calls are deduplicated');
    h.answer(h.fetchesFor('/x')[0], { v: 1 });
    assert.deepStrictEqual(await a, { v: 1 });
    assert.deepStrictEqual(await b, { v: 1 });
    await h.api('/x', { ttl: 15000 });
    assert.strictEqual(h.fetchesFor('/x').length, 1, 'TTL hit');
    h.invalidateApiCache('/x');
    h.api('/x', { ttl: 15000 });
    await flush();
    assert.strictEqual(h.fetchesFor('/x').length, 2, 'refetch after invalidation');
  });

  await test('guard: a failed bust leaves no in-flight entry behind', async () => {
    const h = makeSandbox();
    const p = h.api('/x', { bust: true });
    await flush();
    h.fetchesFor('/x')[0].d.resolve({ ok: false, status: 500, json: async () => ({}), headers: { get: () => null } });
    await assert.rejects(p, /API 500/);
    await flush();
    h.api('/x');
    await flush();
    assert.strictEqual(h.fetchesFor('/x').length, 2, 'the next call fetches again');
  });

  console.log(`\ntest-app-api-bust-inflight-243.js: ${passed} passed, ${failed} failed`);
  process.exitCode = failed ? 1 : 0;
})();
