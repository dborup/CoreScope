/* test-issue-120-distance-building.js — the lazy distance index's
 * 202 Accepted is a transient "building" state, not data (#120).
 *
 * /api/analytics/distance answers 202 {status:"building",
 * retry_after_seconds:5} with a Retry-After header until its lazy index is
 * built (cmd/server/routes.go, handleAnalyticsDistance).
 *
 * Part A loads the REAL public/app.js into a vm with a fake fetch and checks
 * api(): a 202 body is never cached, a 200 still is, and the Retry-After
 * header of a 202 reaches the caller without changing the JSON body.
 *
 * Part B loads the REAL public/analytics.js and drives renderDistanceTab with
 * a stubbed api() and a fake clock: the building state, the retry delay
 * (valid, malformed and absent Retry-After), 202 -> 200, destroy and tab
 * switch, re-entry, out-of-order responses, and at most one retry timer.
 * Unhandled rejections fail the run.
 */
'use strict';

const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

let unhandled = [];
process.on('unhandledRejection', (e) => { unhandled.push(e); });

let passed = 0, failed = 0;
async function test(name, fn) {
  unhandled = [];
  try {
    await fn();
    await new Promise((r) => setImmediate(r));
    if (unhandled.length) throw new Error('unhandled rejection: ' + (unhandled[0] && unhandled[0].message || unhandled[0]));
    passed++; console.log('  ✅ ' + name);
  } catch (e) {
    failed++; console.log('  ❌ ' + name + ': ' + (e && e.message || e));
  }
}
const flush = async () => { for (let i = 0; i < 10; i++) await new Promise((r) => setImmediate(r)); };

// A fake clock: setTimeout only queues; advance(ms) runs what is due.
function fakeClock() {
  let now = 0, nextId = 1;
  const timers = new Map();
  return {
    setTimeout: (fn, ms) => { const id = nextId++; timers.set(id, { fn, at: now + (Number(ms) || 0), ms: Number(ms) || 0 }); return id; },
    clearTimeout: (id) => { timers.delete(id); },
    pending: () => [...timers.values()],
    async advance(ms) {
      now += ms;
      for (const [id, t] of [...timers]) {
        if (t.at <= now) { timers.delete(id); t.fn(); }
      }
      await flush();
    },
  };
}

// ---------------------------------------------------------------- part A

function appCtx(fetchImpl) {
  const ctx = {
    console, Date, Math, Promise, Error, isFinite, parseInt, JSON, Object,
    performance: { now: () => 0 },
    window: {},
    document: { readyState: 'complete', body: { appendChild: () => {} }, createElement: () => ({ style: {} }) },
    fetch: fetchImpl,
    setTimeout: (fn) => { fn(); return 0; },
    setInterval: () => 0, clearInterval: () => {},
    Map, Set,
  };
  vm.createContext(ctx);
  try { vm.runInContext(fs.readFileSync('public/app.js', 'utf8'), ctx); } catch (e) { /* DOM-only tail */ }
  return ctx;
}
function res(status, body, retryAfter) {
  return {
    ok: status >= 200 && status < 300, status,
    headers: { get: (k) => (String(k).toLowerCase() === 'retry-after' && retryAfter !== undefined ? retryAfter : null) },
    json: async () => JSON.parse(JSON.stringify(body)),
  };
}
const BUILDING = { status: 'building', retry_after_seconds: 5, detail: 'distance index is being computed' };

// ---------------------------------------------------------------- part B

function analyticsCtx(apiStub, clock) {
  const pages = {};
  const ctx = {
    window: { addEventListener: () => {}, removeEventListener: () => {}, dispatchEvent: () => {} },
    document: {
      readyState: 'complete', createElement: () => ({ id: '', textContent: '', innerHTML: '' }),
      head: { appendChild: () => {} }, getElementById: () => null, addEventListener: () => {},
      querySelectorAll: () => [], querySelector: () => null,
    },
    console, Date, Infinity, Math, Array, Object, String, Number, JSON, RegExp, Error, TypeError,
    parseInt, parseFloat, isNaN, isFinite, encodeURIComponent, decodeURIComponent,
    setTimeout: clock.setTimeout, clearTimeout: clock.clearTimeout,
    setInterval: () => 0, clearInterval: () => {},
    requestAnimationFrame: () => 0, cancelAnimationFrame: () => {},
    fetch: () => Promise.resolve({ json: () => Promise.resolve({}) }),   // app.js's /api/config/cache
    performance: { now: () => 0 },
    localStorage: { getItem: () => null, setItem: () => {}, removeItem: () => {} },
    location: { hash: '#/analytics?tab=distance' },
    getHashParams: () => new URLSearchParams(''),
    CustomEvent: class CustomEvent {}, Map, Set, Promise, URLSearchParams,
    getComputedStyle: () => ({ getPropertyValue: () => '' }),
    registerPage: (name, obj) => { pages[name] = obj; },
    timeAgo: () => 'x ago',
    RegionFilter: { init: () => {}, onChange: () => {}, regionQueryString: () => '' },
    onWS: () => {}, offWS: () => {}, connectWS: () => {}, invalidateApiCache: () => {},
    makeColumnsResizable: () => {}, initTabBar: () => {}, IATA_COORDS_GEO: {},
  };
  vm.createContext(ctx);
  const load = (f) => { vm.runInContext(fs.readFileSync(f, 'utf8'), ctx); for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k]; };
  load('public/payload-labels.js');
  load('public/roles.js');
  try { load('public/app.js'); } catch (e) { /* DOM-only tail */ }
  ctx.fetchAllNodes = async () => ({ nodes: [] });
  ctx.api = apiStub;
  ctx.registerPage = (name, obj) => { pages[name] = obj; };   // app.js defines its own
  try { load('public/analytics.js'); } catch (e) { for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k]; }
  ctx.__pages = pages;
  return ctx;
}
function fakeEl() { return { innerHTML: '', querySelector: () => null, querySelectorAll: () => [] }; }
const READY = {
  summary: { totalHops: 1234, totalPaths: 56, avgDist: 3.2, maxDist: 40 },
  catStats: { 'R↔R': { count: 3, avg: 2, median: 2, min: 1, max: 4 } },
  distHistogram: { bins: [{ x: 1, count: 2 }] }, distOverTime: [], topHops: [], topPaths: [],
};
// A 202 body as api() hands it to the page: the Retry-After header, when
// valid, travels as a non-enumerable property (it is not part of the JSON).
function accepted(body, retryAfterSeconds) {
  const d = JSON.parse(JSON.stringify(body));
  if (retryAfterSeconds !== undefined) Object.defineProperty(d, 'retryAfterSeconds', { value: retryAfterSeconds, enumerable: false });
  return d;
}
// api() stub fed from a queue of deferreds, so tests control ordering.
function queuedApi() {
  const q = [];
  const calls = [];
  const fn = (path) => {
    calls.push(path);
    if (!q.length) return Promise.reject(new Error('unexpected api call ' + path));
    return q.shift().promise;
  };
  fn.calls = calls;
  fn.push = () => { let resolve, reject; const promise = new Promise((a, b) => { resolve = a; reject = b; }); const d = { promise, resolve, reject }; q.push(d); return d; };
  fn.reply = (value) => { const d = fn.push(); d.resolve(value); return d; };
  return fn;
}

(async () => {
  console.log('\n=== #120 part A: api() and 202 ===');

  await test('a 202 body is not cached: the next call refetches and gets the 200', async () => {
    let n = 0;
    const ctx = appCtx(async (url) => {
      if (!/distance/.test(url)) return res(200, {});
      n++;
      return n === 1 ? res(202, BUILDING, '5') : res(200, READY);
    });
    const first = await ctx.api('/analytics/distance', { ttl: 300000 });
    assert.strictEqual(first.status, 'building');
    const second = await ctx.api('/analytics/distance', { ttl: 300000 });
    assert.strictEqual(n, 2, 'second call must reach the server');
    assert.ok(second.summary, 'second call returns the 200 body');
  });

  await test('a 200 is still cached for its TTL', async () => {
    let n = 0;
    const ctx = appCtx(async (url) => { if (/distance/.test(url)) n++; return res(200, READY); });
    await ctx.api('/analytics/distance', { ttl: 300000 });
    await ctx.api('/analytics/distance', { ttl: 300000 });
    assert.strictEqual(n, 1);
  });

  await test('a valid Retry-After on a 202 reaches the caller; the JSON body is unchanged', async () => {
    const ctx = appCtx(async (url) => (/distance/.test(url) ? res(202, BUILDING, '7') : res(200, {})));
    const d = await ctx.api('/analytics/distance', { ttl: 300000 });
    assert.strictEqual(d.retryAfterSeconds, 7);
    assert.deepStrictEqual(JSON.parse(JSON.stringify(d)), BUILDING, 'body as served');
  });

  await test('a malformed or absent Retry-After is not passed on', async () => {
    for (const h of ['abc', '0', '-3', '', undefined]) {
      const ctx = appCtx(async (url) => (/distance/.test(url) ? res(202, BUILDING, h) : res(200, {})));
      const d = await ctx.api('/analytics/distance');
      assert.strictEqual(d.retryAfterSeconds, undefined, 'header ' + JSON.stringify(h));
    }
  });

  console.log('\n=== #120 part B: renderDistanceTab ===');

  await test('202 shows a building state instead of an error, and schedules one retry', async () => {
    const clock = fakeClock();
    const api = queuedApi();
    const ctx = analyticsCtx(api, clock);
    const render = ctx.window._analyticsRenderDistanceTab;
    assert.strictEqual(typeof render, 'function', 'renderDistanceTab test hook');
    const el = fakeEl();
    api.reply(accepted(BUILDING, 5));
    await render(el);
    assert.ok(/Building the distance index/.test(el.innerHTML), 'building notice: ' + el.innerHTML.slice(0, 120));
    assert.ok(!/Failed to load/.test(el.innerHTML), 'no error: ' + el.innerHTML.slice(0, 120));
    assert.strictEqual(clock.pending().length, 1, 'exactly one retry timer');
    assert.strictEqual(clock.pending()[0].ms, 5000, 'retry after the 5s Retry-After');
  });

  await test('retry delay: header, then body, then a 5s fallback, clamped to 1..30s', async () => {
    const cases = [
      [accepted(BUILDING, 7), 7000],
      [accepted({ status: 'building', retry_after_seconds: 9 }), 9000],
      [accepted({ status: 'building', retry_after_seconds: 'soon' }), 5000],
      [accepted({ status: 'building' }), 5000],
      [accepted({ status: 'building', retry_after_seconds: 0 }), 5000],
      [accepted(BUILDING, 3600), 30000],
      [accepted({ status: 'building', retry_after_seconds: 0.2 }), 1000],
    ];
    for (const [body, want] of cases) {
      const clock = fakeClock();
      const api = queuedApi();
      const ctx = analyticsCtx(api, clock);
      api.reply(body);
      await ctx.window._analyticsRenderDistanceTab(fakeEl());
      assert.strictEqual(clock.pending().length, 1);
      assert.strictEqual(clock.pending()[0].ms, want, JSON.stringify(body) + ' ' + body.retryAfterSeconds);
    }
  });

  await test('202 then 200: the retry replaces the building state and stops', async () => {
    const clock = fakeClock();
    const api = queuedApi();
    const ctx = analyticsCtx(api, clock);
    const el = fakeEl();
    api.reply(accepted(BUILDING, 5));
    await ctx.window._analyticsRenderDistanceTab(el);
    api.reply(READY);
    await clock.advance(5000);
    assert.ok(/Total Hops Analyzed/.test(el.innerHTML), 'rendered data');
    assert.ok(/1,234/.test(el.innerHTML), 'the 200 summary');
    assert.strictEqual(clock.pending().length, 0, 'no further retries');
    assert.strictEqual(api.calls.length, 2);
  });

  await test('destroy (navigation away) cancels the retry and ignores a late response', async () => {
    const clock = fakeClock();
    const api = queuedApi();
    const ctx = analyticsCtx(api, clock);
    const el = fakeEl();
    api.reply(accepted(BUILDING, 5));
    await ctx.window._analyticsRenderDistanceTab(el);
    ctx.__pages.analytics.destroy();
    assert.strictEqual(clock.pending().length, 0, 'timer cleared on destroy');
    // a render whose response arrives after destroy must not write
    const late = api.push();
    const p = ctx.window._analyticsRenderDistanceTab(el);
    ctx.__pages.analytics.destroy();
    el.innerHTML = 'OTHER PAGE';
    late.resolve(accepted(BUILDING, 5));
    await p;
    assert.strictEqual(el.innerHTML, 'OTHER PAGE', 'late response wrote into a destroyed view');
    assert.strictEqual(clock.pending().length, 0, 'late 202 scheduled a retry');
  });

  await test('switching tab invalidates the pending retry and response', async () => {
    const clock = fakeClock();
    const api = queuedApi();
    const ctx = analyticsCtx(api, clock);
    const leave = ctx.window._analyticsLeaveDistanceTab;
    assert.strictEqual(typeof leave, 'function', 'tab-switch invalidation hook');
    const el = fakeEl();
    api.reply(accepted(BUILDING, 5));
    await ctx.window._analyticsRenderDistanceTab(el);
    leave();
    assert.strictEqual(clock.pending().length, 0);
    const late = api.push();
    const p = ctx.window._analyticsRenderDistanceTab(el);
    leave();
    el.innerHTML = 'RF TAB';
    late.resolve(READY);
    await p;
    assert.strictEqual(el.innerHTML, 'RF TAB');
    // the tab bar calls it on every switch away from Distance
    const src = fs.readFileSync('public/analytics.js', 'utf8');
    assert.ok(/if \(_currentTab !== 'distance'\) _leaveDistanceTab\(\);/.test(src), 'tab click handler calls _leaveDistanceTab()');
  });

  await test('re-entry after leaving starts a fresh chain that still works', async () => {
    const clock = fakeClock();
    const api = queuedApi();
    const ctx = analyticsCtx(api, clock);
    const el = fakeEl();
    api.reply(accepted(BUILDING, 5));
    await ctx.window._analyticsRenderDistanceTab(el);
    ctx.__pages.analytics.destroy();
    api.reply(accepted(BUILDING, 2));
    await ctx.window._analyticsRenderDistanceTab(el);
    assert.strictEqual(clock.pending().length, 1);
    assert.strictEqual(clock.pending()[0].ms, 2000);
    api.reply(READY);
    await clock.advance(2000);
    assert.ok(/Total Hops Analyzed/.test(el.innerHTML));
  });

  await test('out of order: an older 202 arriving after a newer 200 changes nothing', async () => {
    const clock = fakeClock();
    const api = queuedApi();
    const ctx = analyticsCtx(api, clock);
    const el = fakeEl();
    const older = api.push();
    const p1 = ctx.window._analyticsRenderDistanceTab(el);   // e.g. before a region change
    api.reply(READY);
    await ctx.window._analyticsRenderDistanceTab(el);          // newer render wins
    const after = el.innerHTML;
    assert.ok(/Total Hops Analyzed/.test(after));
    older.resolve(accepted(BUILDING, 5));
    await p1;
    assert.strictEqual(el.innerHTML, after, 'older response overwrote the newer render');
    assert.strictEqual(clock.pending().length, 0, 'older response started a retry chain');
  });

  await test('repeated renders while building keep at most one retry timer', async () => {
    const clock = fakeClock();
    const api = queuedApi();
    const ctx = analyticsCtx(api, clock);
    const el = fakeEl();
    for (let i = 0; i < 4; i++) {
      api.reply(accepted(BUILDING, 5));
      await ctx.window._analyticsRenderDistanceTab(el);
      assert.strictEqual(clock.pending().length, 1, 'after render ' + (i + 1));
    }
    api.reply(accepted(BUILDING, 5));
    await clock.advance(5000);
    assert.strictEqual(clock.pending().length, 1, 'the retry re-arms exactly one timer');
  });

  await test('a failed request still shows the error state, with no retry', async () => {
    const clock = fakeClock();
    const api = queuedApi();
    const ctx = analyticsCtx(api, clock);
    const el = fakeEl();
    api.push().reject(new Error('API 500: /analytics/distance'));
    await ctx.window._analyticsRenderDistanceTab(el);
    assert.ok(/Failed to load distance analytics: API 500/.test(el.innerHTML));
    assert.strictEqual(clock.pending().length, 0);
  });

  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed > 0 ? 1 : 0);
})();
