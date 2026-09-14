/**
 * app.js's api() helper: the discarded `.finally()` cleanup promise must
 * not produce a second, unobserved rejection alongside the one the real
 * caller already handles.
 *
 * api() does:
 *   _inflight.set(path, promise);
 *   promise.finally(() => _inflight.delete(path));
 *   return promise;
 *
 * `.finally()` returns its own derived promise mirroring `promise`'s
 * outcome. That derived promise was discarded uncaught, so when a
 * request rejects, BOTH the returned `promise` (correctly awaited/caught
 * by every real caller) AND the orphaned `.finally()` promise reject --
 * the second one with nothing ever attached to observe it. In a browser
 * this only produces a benign `Uncaught (in promise)` console warning
 * (nothing reads that derived promise's value), but it is still a real,
 * avoidable defect: Node's stricter default (an unhandled rejection
 * crashes the process) has no browser equivalent and is what surfaced
 * this while testing #1375's scope-stats fetch behavior.
 *
 * Fix: `promise.finally(() => _inflight.delete(path)).catch(() => {})`.
 * The added `.catch()` only consumes the *derived* promise's mirrored
 * rejection -- it is a different promise object from the `promise`
 * returned to callers, so callers' error handling is completely
 * unaffected (proven by scenario B below matching the pre-fix error
 * text exactly).
 *
 * Scenarios A-H below load the REAL, unmodified public/app.js via vm
 * (only `fetch` is stubbed) so the real api()/_apiCache/_inflight logic
 * runs unmodified from this test's perspective.
 */
'use strict';

const vm = require('vm');
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { spawnSync } = require('child_process');

let passed = 0, failed = 0;
function check(cond, msg) {
  if (cond) { passed++; console.log('  ✓ ' + msg); }
  else { failed++; console.error('  ✗ ' + msg); }
}
async function checkAsync(name, fn) {
  try {
    await fn();
    passed++;
    console.log('  ✓ ' + name);
  } catch (e) {
    failed++;
    console.error('  ✗ ' + name + ': ' + e.message);
  }
}

const APP_JS_PATH = path.join(__dirname, 'public', 'app.js');

function makeSandbox() {
  const fetchLog = [];
  let fetchBehavior = () => ({ ok: true, status: 200, json: async () => ({}), headers: { get: () => null } });

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
    fetchLog.push(url);
    const r = fetchBehavior(url);
    return r instanceof Promise ? r : Promise.resolve(r);
  };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(APP_JS_PATH, 'utf8'), ctx);
  for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k];

  return {
    api: ctx.api,
    invalidateApiCache: ctx.invalidateApiCache,
    // app.js's own top-level `fetch('/api/config/cache')` fires on load;
    // scoping every count to a scenario-private path keeps that separate.
    countFor: (p) => fetchLog.filter((u) => u === '/api' + p).length,
    setFetchBehavior: (fn) => { fetchBehavior = fn; },
  };
}

// Deterministic drain of already-queued microtasks/macrotasks -- no
// wall-clock sleeps.
function flush(times) {
  let p = Promise.resolve();
  for (let i = 0; i < (times || 10); i++) p = p.then(() => new Promise((r) => setImmediate(r)));
  return p;
}

(async () => {
  console.log('\n=== api() orphaned .finally() cleanup-rejection fix ===');

  await checkAsync('A. Success: api() resolves with the real fetched data, unchanged', async () => {
    const h = makeSandbox();
    h.setFetchBehavior(() => ({ ok: true, status: 200, json: async () => ({ hello: 'world' }), headers: { get: () => null } }));
    const data = await h.api('/test/a-success');
    assert.deepStrictEqual(data, { hello: 'world' });
  });

  await checkAsync('B. Failure: the caller still gets the exact same rejection semantics as before the fix', async () => {
    const h = makeSandbox();
    h.setFetchBehavior(() => ({ ok: false, status: 500, json: async () => ({}), headers: { get: () => null } }));
    await assert.rejects(() => h.api('/test/b-fail'), /^Error: API 500: \/test\/b-fail$/);
  });

  await checkAsync('C. A handled request failure produces NO additional unhandled rejection (child process, --unhandled-rejections=strict)', async () => {
    const script = `
      const vm = require('vm');
      const fs = require('fs');
      const ctx = {
        window: { addEventListener: () => {}, dispatchEvent: () => {} },
        document: { readyState: 'complete', getElementById: () => null, addEventListener: () => {}, querySelectorAll: () => [] },
        console, Date, Promise, Map, Set, JSON, Math, Error, TypeError,
        parseInt, isFinite, encodeURIComponent, decodeURIComponent,
        setTimeout: (fn) => setTimeout(fn, 0), clearTimeout: () => {},
        performance: { now: () => Date.now() },
        location: { hash: '' }, addEventListener: () => {}, dispatchEvent: () => {},
      };
      ctx.fetch = () => Promise.resolve({ ok: false, status: 500, headers: { get: () => null }, json: async () => ({}) });
      vm.createContext(ctx);
      vm.runInContext(fs.readFileSync(${JSON.stringify(APP_JS_PATH)}, 'utf8'), ctx);
      for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k];
      (async () => {
        try { await ctx.api('/c-handled-fail'); process.exitCode = 1; }
        catch (e) { /* caller correctly handles it -- this is the ONLY place the rejection should be observed */ }
        let p = Promise.resolve();
        for (let i = 0; i < 10; i++) p = p.then(() => new Promise((r) => setImmediate(r)));
        await p;
      })();
    `;
    const r = spawnSync(process.execPath, ['--unhandled-rejections=strict', '-e', script], { timeout: 10000, encoding: 'utf8' });
    assert.strictEqual(r.status, 0,
      'expected the child process to exit 0 (no unhandled rejection) under --unhandled-rejections=strict; ' +
      'got status=' + r.status + ' stderr=' + r.stderr);
  });

  await checkAsync('D. In-flight entry is cleared after a SUCCESSFUL request (no stale dedup blocking a later independent call)', async () => {
    const h = makeSandbox();
    h.setFetchBehavior(() => ({ ok: true, status: 200, json: async () => ({ n: 1 }), headers: { get: () => null } }));
    await h.api('/test/d-success'); // ttl:0 (default) -> no cache write either
    await h.api('/test/d-success');
    assert.strictEqual(h.countFor('/test/d-success'), 2,
      'expected 2 independent fetches (in-flight entry must not still be present from the first, already-settled call)');
  });

  await checkAsync('E. In-flight entry is cleared after a FAILED request, and the next attempt can legitimately succeed', async () => {
    const h = makeSandbox();
    let attempts = 0;
    h.setFetchBehavior(() => {
      attempts++;
      return attempts === 1
        ? { ok: false, status: 500, json: async () => ({}), headers: { get: () => null } }
        : { ok: true, status: 200, json: async () => ({ recovered: true }), headers: { get: () => null } };
    });
    await assert.rejects(() => h.api('/test/e-retry'));
    const data = await h.api('/test/e-retry'); // must be a fresh fetch, not the stale rejected in-flight promise
    assert.deepStrictEqual(data, { recovered: true });
    assert.strictEqual(h.countFor('/test/e-retry'), 2);
  });

  await checkAsync('F. Concurrent requests for the same path are still deduplicated via _inflight', async () => {
    const h = makeSandbox();
    let resolveFetch;
    h.setFetchBehavior(() => new Promise((res) => {
      resolveFetch = () => res({ ok: true, status: 200, json: async () => ({ shared: true }), headers: { get: () => null } });
    }));
    const p1 = h.api('/test/f-dedup');
    const p2 = h.api('/test/f-dedup'); // fired before p1 settles -> must reuse the same in-flight promise
    await flush(3);
    resolveFetch();
    const [d1, d2] = await Promise.all([p1, p2]);
    assert.deepStrictEqual(d1, { shared: true });
    assert.deepStrictEqual(d2, { shared: true });
    assert.strictEqual(h.countFor('/test/f-dedup'), 1, 'expected exactly 1 real fetch for 2 concurrent identical calls');
  });

  await checkAsync('G. TTL cache still serves a hit within TTL, and still refetches after invalidation', async () => {
    const h = makeSandbox();
    h.setFetchBehavior(() => ({ ok: true, status: 200, json: async () => ({ cached: true }), headers: { get: () => null } }));
    await h.api('/test/g-ttl', { ttl: 30000 });
    await h.api('/test/g-ttl', { ttl: 30000 });
    assert.strictEqual(h.countFor('/test/g-ttl'), 1, 'second call within TTL must be served from cache, not refetched');
    h.invalidateApiCache('/test/g-ttl');
    await h.api('/test/g-ttl', { ttl: 30000 });
    assert.strictEqual(h.countFor('/test/g-ttl'), 2, 'after invalidation, the next call must be a real fetch');
  });

  await checkAsync('H. The harness itself does not hide a real unhandled rejection (no suppressing global handler; the same strict-mode check DOES catch a genuine one)', async () => {
    assert.strictEqual(process.listenerCount('unhandledRejection'), 0,
      'this test file must not install any process-wide unhandledRejection handler');
    // Same technique as C, but with a deliberately uncaught rejection
    // unrelated to api() -- proves the check is discriminating, not
    // vacuously green regardless of what runs inside it.
    const script = `
      Promise.reject(new Error('deliberate-uncaught-control'));
      let p = Promise.resolve();
      for (let i = 0; i < 10; i++) p = p.then(() => new Promise((r) => setImmediate(r)));
      p.then(() => { process.exitCode = 0; });
    `;
    const r = spawnSync(process.execPath, ['--unhandled-rejections=strict', '-e', script], { timeout: 10000, encoding: 'utf8' });
    assert.notStrictEqual(r.status, 0,
      'expected a deliberately uncaught rejection to make the child process exit non-zero under --unhandled-rejections=strict -- ' +
      'got status=' + r.status + ' (if this is 0, the harness technique used in scenario C cannot be trusted)');
  });

  console.log('\n=== Summary ===');
  console.log('  Passed: ' + passed);
  console.log('  Failed: ' + failed);
  console.log('\napi()-inflight-cleanup-rejection ' + (failed === 0 ? 'PASS' : 'FAIL'));
  process.exitCode = failed === 0 ? 0 : 1;
})();
