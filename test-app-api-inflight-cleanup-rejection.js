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
 * rejection -- it is a different promise object from the promise chain
 * returned to callers (api() is `async function`, so every caller
 * actually holds a promise that FOLLOWS `promise`, never `promise`
 * itself -- but that followed promise mirrors the same resolution, so
 * callers' error handling is unaffected either way, proven by scenario
 * B below matching the pre-fix error text exactly).
 *
 * Scenarios A-H below load the REAL, unmodified public/app.js via vm
 * (only `fetch` is stubbed) so the real api()/_apiCache/_inflight logic
 * runs unmodified from this test's perspective.
 *
 * --- Completion guarantee (round 2 review fix) ---
 * A prior version of this file set `process.exitCode` only at the very
 * end. If an in-flight dedup regression left one branch of a
 * Promise.all() permanently pending (scenario F), nothing else kept the
 * event loop alive, so Node drained and exited 0 WITHOUT ever reaching
 * F/G/H or the summary -- a silent false pass. Two independent guards
 * now prevent that:
 *   1. `process.exitCode = 1` is set immediately, before anything else
 *      runs, and only flipped to 0 after every named scenario has been
 *      confirmed to have run AND all of them passed.
 *   2. A watchdog `setTimeout` (not unref'd) is armed for the whole
 *      run's duration. A real, non-unref'd timer is a pending macrotask,
 *      so it keeps the event loop alive even if some other promise
 *      chain stalls -- Node cannot silently drain and exit while this
 *      timer is still the sole reason it stays alive. This is strictly
 *      an event-loop-based safety bound for ASYNC stalls: it cannot
 *      interrupt spawnSync (used by scenarios C and H) or any other
 *      synchronous blocking in this main process, because a blocked
 *      main thread never gets to run the timer's callback either --
 *      an outer runner-level timeout remains the only backstop for
 *      that case. If the suite hasn't finished by the deadline, the
 *      watchdog itself fails loudly and exits 1. It is cleared on the
 *      normal completion path, so a healthy run's timing is unaffected.
 */
'use strict';

process.exitCode = 1; // Flipped to 0 only after every scenario is confirmed complete AND passing.

const vm = require('vm');
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { spawnSync } = require('child_process');

const WATCHDOG_MS = 15000;
const watchdog = setTimeout(() => {
  console.error(
    '\n✗ WATCHDOG: the suite did not finish within ' + WATCHDOG_MS + 'ms. ' +
    'This is an event-loop-based safety bound for an ASYNC stall (e.g. a ' +
    'permanently-pending promise) -- it cannot interrupt spawnSync or any ' +
    'other synchronous blocking in this main process, so it firing means ' +
    'something is genuinely stuck in async code (not the historical ' +
    '"silent early exit 0" failure mode, which this timer separately ' +
    'prevents just by existing). Failing loudly instead of hanging CI ' +
    'indefinitely.'
  );
  process.exitCode = 1;
  process.exit(1);
}, WATCHDOG_MS);

const EXPECTED_SCENARIOS = ['A', 'B', 'C', 'D', 'E', 'F', 'G', 'H'];
const ranScenarios = new Set();

let passed = 0, failed = 0;
async function checkAsync(name, fn) {
  const label = (/^([A-H])\./.exec(name) || [])[1];
  try {
    await fn();
    passed++;
    console.log('  ✓ ' + name);
  } catch (e) {
    failed++;
    console.error('  ✗ ' + name + ': ' + e.message);
  } finally {
    if (label) ranScenarios.add(label);
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

// Shared child-process source for scenario C. Deliberately verbose and
// self-checking rather than a bare try/catch: a wrong/missing api(), an
// error thrown before fetch, or a mismatched failure must each produce
// their OWN distinguishable failure line, and only the fully-verified
// exact path may print SUCCESS_MARKER. An empty catch cannot let any of
// this slide, because there is no catch-and-ignore left in this script;
// every branch either explicitly fails or explicitly proceeds.
const C_SUCCESS_MARKER = 'REACHED_EXPECTED_SUCCESS_MARKER_9f3a1c';
function buildScenarioCChildScript(appJsPath) {
  return `
    const vm = require('vm');
    const fs = require('fs');
    const fetchLog = [];
    const ctx = {
      window: { addEventListener: () => {}, dispatchEvent: () => {} },
      document: { readyState: 'complete', getElementById: () => null, addEventListener: () => {}, querySelectorAll: () => [] },
      console, Date, Promise, Map, Set, JSON, Math, Error, TypeError,
      parseInt, isFinite, encodeURIComponent, decodeURIComponent,
      setTimeout: (fn) => setTimeout(fn, 0), clearTimeout: () => {},
      performance: { now: () => Date.now() },
      location: { hash: '' }, addEventListener: () => {}, dispatchEvent: () => {},
    };
    ctx.fetch = function (url) {
      fetchLog.push(url);
      return Promise.resolve({ ok: false, status: 500, headers: { get: () => null }, json: async () => ({}) });
    };
    vm.createContext(ctx);
    vm.runInContext(fs.readFileSync(${JSON.stringify(appJsPath)}, 'utf8'), ctx);
    for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k];

    (async () => {
      if (typeof ctx.api !== 'function') {
        console.error('CHILD_FAIL: ctx.api is not a function (got ' + typeof ctx.api + ') -- app.js did not expose the real api() helper');
        process.exitCode = 1;
        return;
      }

      const PATH = '/c-handled-fail';
      const EXPECTED_MESSAGE = 'API 500: ' + PATH;
      let rejection = null;
      try {
        const result = await ctx.api(PATH);
        console.error('CHILD_FAIL: expected api(' + PATH + ') to reject, but it resolved with ' + JSON.stringify(result));
        process.exitCode = 1;
        return;
      } catch (e) {
        rejection = e;
      }

      if (!rejection || rejection.message !== EXPECTED_MESSAGE) {
        console.error('CHILD_FAIL: expected rejection message ' + JSON.stringify(EXPECTED_MESSAGE) + ', got ' + JSON.stringify(rejection && rejection.message));
        process.exitCode = 1;
        return;
      }

      const matchingFetches = fetchLog.filter((u) => u === '/api' + PATH);
      if (matchingFetches.length !== 1) {
        console.error('CHILD_FAIL: expected exactly 1 fetch to /api' + PATH + ', got ' + matchingFetches.length + ': ' + JSON.stringify(fetchLog));
        process.exitCode = 1;
        return;
      }

      // Drain queued microtasks/macrotasks so the orphaned .finally()
      // promise's rejection (if the production fix is absent) gets a
      // chance to surface as an unhandled rejection BEFORE we declare
      // success -- this is the actual condition under test.
      let p = Promise.resolve();
      for (let i = 0; i < 10; i++) p = p.then(() => new Promise((r) => setImmediate(r)));
      await p;

      console.log('${C_SUCCESS_MARKER}');
    })();
  `;
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
    const script = buildScenarioCChildScript(APP_JS_PATH);
    const r = spawnSync(process.execPath, ['--unhandled-rejections=strict', '-e', script],
      { timeout: 10000, killSignal: 'SIGKILL', encoding: 'utf8' });
    assert.ok(!(r.error && r.error.code === 'ETIMEDOUT'),
      'child process TIMED OUT after 10000ms and was killed with SIGKILL -- it never reached completion ' +
      '(this is a hang in the child, not an unhandled-rejection failure)');
    assert.strictEqual(r.error, undefined,
      'child process failed to spawn: ' + (r.error && r.error.message));
    assert.strictEqual(r.signal, null,
      'child process was killed by a signal (likely the 10s timeout) instead of exiting normally: ' + r.signal);
    assert.strictEqual(r.status, 0,
      'expected the child to exit 0 (no unhandled rejection) under --unhandled-rejections=strict; ' +
      'got status=' + r.status + ' stdout=' + r.stdout + ' stderr=' + r.stderr);
    assert.ok((r.stdout || '').includes(C_SUCCESS_MARKER),
      'child exited 0 but never printed the success marker -- it must have returned early without ' +
      'actually exercising and verifying the real api() call. stdout=' + r.stdout + ' stderr=' + r.stderr);
    assert.strictEqual((r.stderr || '').trim(), '',
      'expected no stderr output on the success path; got: ' + r.stderr);
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
    // If dedup were broken, p2 would have triggered its OWN fetch here,
    // silently reassigning `resolveFetch` to the second call's resolver
    // and leaving p1 permanently pending. Fail loudly on that instead of
    // calling a possibly-stale resolver and hanging inside Promise.all.
    assert.strictEqual(h.countFor('/test/f-dedup'), 1,
      'expected exactly 1 real fetch to have been made BEFORE resolving (dedup must reuse the in-flight promise, ' +
      'not start a second real fetch that would leave the first caller\'s promise permanently pending)');
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

  await checkAsync('H. The strict-mode child-process technique used in C genuinely detects a deliberate unhandled rejection (and this file installs no suppressing handler)', async () => {
    assert.strictEqual(process.listenerCount('unhandledRejection'), 0,
      'this test file must not install any process-wide unhandledRejection handler');
    // Deliberately uncaught rejection, unrelated to api(), with a unique
    // message so the parent can confirm the crash was caused BY THIS
    // rejection specifically -- not by some unrelated child-process
    // failure (a syntax error, a missing module, etc.) that would also
    // produce a nonzero exit but prove nothing about the technique.
    const marker = 'deliberate-uncaught-control-7d2e';
    const script = `
      Promise.reject(new Error('${marker}'));
      let p = Promise.resolve();
      for (let i = 0; i < 10; i++) p = p.then(() => new Promise((r) => setImmediate(r)));
      p.then(() => { console.log('SHOULD_NOT_REACH_HERE_IF_STRICT_MODE_WORKS'); });
    `;
    const r = spawnSync(process.execPath, ['--unhandled-rejections=strict', '-e', script],
      { timeout: 10000, killSignal: 'SIGKILL', encoding: 'utf8' });
    assert.ok(!(r.error && r.error.code === 'ETIMEDOUT'),
      'child process TIMED OUT after 10000ms and was killed with SIGKILL -- it never reached completion ' +
      '(this is a hang in the child, not evidence for or against the strict-mode technique)');
    assert.strictEqual(r.error, undefined,
      'child process failed to spawn: ' + (r.error && r.error.message));
    assert.strictEqual(r.signal, null,
      'child process was killed by a signal (' + r.signal + ') instead of exiting normally -- a signal kill ' +
      '(including our own 10s timeout SIGKILL) must never be mistaken for the deliberate-rejection crash');
    assert.ok(typeof r.status === 'number' && r.status !== 0,
      'expected the deliberate unhandled rejection to make the child exit with a numeric non-zero status under ' +
      '--unhandled-rejections=strict -- got status=' + JSON.stringify(r.status) + ' (null would mean the process ' +
      'was killed rather than exiting on its own, and must not count as a pass)');
    // A substring check here is not enough: when the child crashes for ANY
    // reason (a typo, a syntax error), Node echoes the OFFENDING SOURCE LINE
    // to stderr, and the script line above containing `${marker}` would
    // itself satisfy a plain `.includes(marker)` check even though no
    // deliberate-rejection crash occurred. Require the exact thrown-message
    // line instead -- `Error: <marker>` alone on its own line -- which only
    // appears when Node prints the uncaught exception's message, not when it
    // is merely quoting a source line.
    const escapedMarker = marker.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
    const thrownMessageLine = new RegExp('^Error: ' + escapedMarker + '$', 'm');
    assert.ok(thrownMessageLine.test(r.stderr || ''),
      'expected the crash to be caused SPECIFICALLY by our deliberate rejection -- stderr should contain the exact ' +
      'thrown-message line "Error: ' + marker + '" on its own line, not merely mention the marker (e.g. by quoting ' +
      'the source line for an unrelated crash) -- stderr=' + r.stderr);
    assert.ok(!(r.stdout || '').includes('SHOULD_NOT_REACH_HERE_IF_STRICT_MODE_WORKS'),
      'the .then() scheduled after the rejection must never have run once the process crashed');
  });

  clearTimeout(watchdog);

  const missing = EXPECTED_SCENARIOS.filter((l) => !ranScenarios.has(l));
  if (missing.length > 0) {
    failed++;
    console.error('\n✗ INCOMPLETE SUITE: scenario(s) ' + missing.join(', ') + ' never ran to completion (expected exactly ' +
      EXPECTED_SCENARIOS.join(', ') + ')');
  }

  console.log('\n=== Summary ===');
  console.log('  Ran: ' + Array.from(ranScenarios).sort().join(', ') + ' (' + ranScenarios.size + '/' + EXPECTED_SCENARIOS.length + ')');
  console.log('  Passed: ' + passed);
  console.log('  Failed: ' + failed);
  const complete = missing.length === 0;
  console.log('\napi()-inflight-cleanup-rejection ' + (failed === 0 && complete ? 'PASS' : 'FAIL'));
  process.exitCode = (failed === 0 && complete) ? 0 : 1;
})();
