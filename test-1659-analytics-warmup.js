/* test-1659-analytics-warmup.js
 *
 * Issue #1659: client-side warmup-retry for analytics endpoints.
 *
 * Asserts that public/app.js's api() helper:
 *   (a) retries on a 503 response and honors the Retry-After header
 *       value (in seconds) for the wait between attempts
 *   (b) eventually returns the JSON body once the server flips to 200
 *   (c) caps retries so a permanently broken endpoint does not loop
 *       forever
 *
 * The test loads app.js into a vm context with a fake fetch and a fake
 * setTimeout that resolves immediately (to keep wall-time minimal).
 */
'use strict';
const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

// Some tests intentionally cause api() to throw; the inflight tracker
// inside app.js may surface that as an unhandled rejection a tick later
// even after the test has caught it. Swallow them — they're expected
// for the "cap retries" test path. Any other unhandled rejection (e.g. a
// TypeError from an analytics tab rendered without data, #172) is kept
// so the test that caused it can fail on it.
const unexpectedRejections = [];
process.on('unhandledRejection', (e) => {
  if (e && /API \d+/.test(e.message || '')) return;
  unexpectedRejections.push(e);
});

let passed = 0, failed = 0;
function test(name, fn) {
  return Promise.resolve()
    .then(fn)
    .then(() => { passed++; console.log('  ✅ ' + name); })
    .catch(e => { failed++; console.log('  ❌ ' + name + ': ' + (e && e.message || e)); });
}

function makeCtx(fetchImpl, clock) {
  const ctx = {
    console, Date, Math, Promise, Error, isFinite, parseInt, JSON,
    performance: { now: () => 0 },
    window: {},
    document: { readyState: 'complete', body: { appendChild: () => {} }, createElement: () => ({ style: {} }) },
    fetch: fetchImpl,
    setTimeout: (fn) => { fn(); return 0; }, // resolve immediately — backoff sleep is irrelevant in unit tests
    setInterval: () => 0,
    clearInterval: () => {},
    Map, Set,
  };
  // #172: with a fake clock, backoff sleeps only run when it advances.
  if (clock) { ctx.setTimeout = clock.setTimeout; ctx.clearTimeout = clock.clearTimeout; }
  vm.createContext(ctx);
  // Provide the no-op fetch('/api/config/cache') hit that app.js makes
  // on module load by returning a thenable that ignores.
  const src = fs.readFileSync('public/app.js', 'utf8');
  // Strip everything after the api()-related helpers we need; we only
  // want the top of the file (cache + api + _warmupNotify) and the
  // fetchAllNodes helper isn't required for these tests. Run the full
  // file under the sandbox; references to globals are guarded.
  try { vm.runInContext(src, ctx, { lineOffset: 0 }); } catch (e) {
    // Some downstream code expects window/document/localStorage; we
    // only need api() to be defined for these tests. Ignore.
  }
  return ctx;
}

// ---------------------------------------------------------------- #172

// A fake clock: setTimeout only queues; advance(ms) runs what is due, in
// time order, then lets the promise chains settle.
function fakeClock() {
  let now = 0, nextId = 1;
  const timers = new Map();
  const flush = async () => { for (let i = 0; i < 30; i++) await new Promise((r) => setImmediate(r)); };
  return {
    now: () => now,
    setTimeout: (fn, ms) => { const id = nextId++; timers.set(id, { fn, at: now + (Number(ms) || 0) }); return id; },
    clearTimeout: (id) => { timers.delete(id); },
    pending: () => [...timers.values()],
    async advance(ms) {
      const end = now + ms;
      for (;;) {
        let next = null;
        for (const [id, t] of timers) if (t.at <= end && (!next || t.at < next[1].at)) next = [id, t];
        if (!next) break;
        timers.delete(next[0]);
        now = Math.max(now, next[1].at);
        next[1].fn();
        await flush();
      }
      now = end;
      await flush();
    },
  };
}

const WARM_RF = {
  totalTransmissions: 4321, totalPackets: 1234, packetsPerHour: [{ hour: '2026-10-02T12:00:00Z', count: 5 }],
  snr: { avg: 5, min: -10, max: 12 }, rssi: { avg: -90, min: -120, max: -40 },
  avgPacketSize: 40, minPacketSize: 10, maxPacketSize: 200, timeSpanHours: 24, payloadTypes: [],
};
const WARM_TOPO = { uniqueNodes: 42, avgHops: 1.5, maxHops: 5, hopDistribution: [] };
const WARM_CHAN = { activeChannels: 3, decryptable: 1 };

function fakeEl(id) {
  let html = '', dataWrites = 0;
  const listeners = {};
  return {
    id, value: '', style: {}, dataset: {}, parentElement: null,
    get innerHTML() { return html; },
    set innerHTML(v) { html = String(v); if (/Total Transmissions/.test(html)) dataWrites++; },
    dataWrites: () => dataWrites,
    classList: { add() {}, remove() {}, contains: () => false, toggle() {} },
    addEventListener(type, fn) { (listeners[type] = listeners[type] || []).push(fn); }, removeEventListener() {},
    dispatch(type, ev) { for (const fn of listeners[type] || []) fn(ev); },
    contains: () => true,
    querySelector: () => null, querySelectorAll: () => [],
    appendChild: (c) => c, insertBefore: (c) => c, setAttribute() {}, getAttribute: () => null,
  };
}

// The analytics page on a server whose rf/topology/channels answer 503 +
// Retry-After: 5 until warmMs on the fake clock, each response after
// latencyMs (0: at once). opts.status and opts.retryAfter replace the
// 503 and its Retry-After value.
function pageEnv(warmMs, latencyMs, opts) {
  const errStatus = (opts && opts.status) || 503;
  const errRetryAfter = opts && 'retryAfter' in opts ? opts.retryAfter : '5';
  const clock = fakeClock();
  const els = {};
  const el = (id) => els[id] || (els[id] = fakeEl(id));
  const fetchLog = [];
  const respond = (status, body, retryAfter) => ({
    ok: status >= 200 && status < 300, status,
    headers: { get: (k) => (String(k).toLowerCase() === 'retry-after' ? retryAfter || null : null) },
    json: async () => JSON.parse(JSON.stringify(body)),
  });
  const fetchImpl = async (url) => {
    fetchLog.push(url);
    if (latencyMs) await new Promise((r) => clock.setTimeout(r, latencyMs));
    if (/\/api\/analytics\/(rf|topology|channels)\b/.test(url) && clock.now() < warmMs) {
      return respond(errStatus, { error: 'analytics warming up', retry_after_s: 5 }, errRetryAfter);
    }
    if (/\/api\/analytics\/rf\b/.test(url)) {
      return respond(200, Object.assign({}, WARM_RF, /region=CPH/.test(url) ? { totalTransmissions: 8765 } : {}));
    }
    if (/\/api\/analytics\/topology\b/.test(url)) return respond(200, WARM_TOPO);
    if (/\/api\/analytics\/channels\b/.test(url)) return respond(200, WARM_CHAN);
    if (/relay-airtime-share/.test(url)) return respond(200, { rows: [] });
    return respond(200, {});
  };
  class FakeDate extends Date { static now() { return 1790000000000 + clock.now(); } }
  let regionCb = null, region = '';
  const pages = {};
  const ctx = {
    window: { addEventListener() {}, removeEventListener() {}, dispatchEvent() {} },
    document: {
      readyState: 'complete', body: { appendChild() {} }, head: { appendChild() {} },
      createElement: () => fakeEl(''), getElementById: el, addEventListener() {},
      querySelectorAll: () => [], querySelector: () => null, documentElement: fakeEl('html'),
    },
    console: { log() {}, warn() {}, error: console.error }, Date: FakeDate, Infinity, Math, Array, Object, String, Number,
    JSON, RegExp, Error, TypeError, parseInt, parseFloat, isNaN, isFinite, encodeURIComponent, decodeURIComponent,
    setTimeout: clock.setTimeout, clearTimeout: clock.clearTimeout, setInterval: () => 0, clearInterval() {},
    requestAnimationFrame: () => 0, cancelAnimationFrame() {},
    fetch: fetchImpl, performance: { now: () => clock.now() },
    localStorage: { getItem: () => null, setItem() {}, removeItem() {} },
    location: { hash: '#/analytics' }, history: { replaceState() {} },
    CustomEvent: class CustomEvent {}, Map, Set, Promise, URLSearchParams,
    getComputedStyle: () => ({ getPropertyValue: () => '' }),
    timeAgo: () => 'x ago', initTabBar() {}, makeColumnsResizable() {},
    onWS() {}, offWS() {}, connectWS() {}, invalidateApiCache() {}, IATA_COORDS_GEO: {},
    RegionFilter: { init() {}, onChange: (fn) => { regionCb = fn; }, regionQueryString: () => region },
    AreaFilter: { init() {}, onChange() {}, areaQueryString: () => '' },
  };
  vm.createContext(ctx);
  const load = (f) => { vm.runInContext(fs.readFileSync(f, 'utf8'), ctx); for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k]; };
  load('public/payload-labels.js');
  load('public/roles.js');
  try { load('public/app.js'); } catch (e) { /* DOM-only tail */ }
  ctx.registerPage = (name, obj) => { pages[name] = obj; };   // app.js defines its own
  load('public/analytics.js');
  return {
    clock, el, page: pages.analytics,
    regionChanged: (r) => { region = r ? '&region=' + r : ''; regionCb(); },
    // A click on the analytics tab button for `tab`, as the browser sends it.
    clickTab: (tab) => {
      const btn = { dataset: { tab }, classList: { add() {}, remove() {} }, setAttribute() {} };
      el('analyticsTabs').dispatch('click', { target: { closest: () => btn } });
    },
    analyticsFetches: () => fetchLog.filter((u) => /\/api\/analytics\/(rf|topology|channels)\b/.test(u)).length,
  };
}

(async () => {
  console.log('\n=== #1659: api() retries on 503 with Retry-After ===');

  await test('honors Retry-After then returns body on success', async () => {
    let calls = 0;
    const ctx = makeCtx(async (url) => {
      if (!/analytics/.test(url || '')) {
        return { ok: true, status: 200, headers: { get: () => null }, json: async () => ({}) };
      }
      calls++;
      if (calls < 3) {
        return {
          ok: false, status: 503,
          headers: { get: (k) => k.toLowerCase() === 'retry-after' ? '5' : null },
          json: async () => ({ error: 'analytics warming up', retry_after_s: 5 }),
        };
      }
      return { ok: true, status: 200, headers: { get: () => null }, json: async () => ({ totalPackets: 99 }) };
    });
    const data = await ctx.api('/analytics/rf');
    assert.strictEqual(calls, 3, 'expected 3 analytics fetches (two 503s then 200)');
    assert.deepStrictEqual(data, { totalPackets: 99 });
  });

  await test('caps retry attempts (no infinite loop on permanent 503)', async () => {
    let calls = 0;
    const ctx = makeCtx(async (url) => {
      if (!/analytics/.test(url || '')) {
        return { ok: true, status: 200, headers: { get: () => null }, json: async () => ({}) };
      }
      calls++;
      return {
        ok: false, status: 503,
        headers: { get: () => '1' },
        json: async () => ({}),
      };
    });
    let thrown = null;
    try { await ctx.api('/analytics/rf'); } catch (e) { thrown = e; }
    assert.ok(thrown, 'expected api() to throw after retries exhausted');
    assert.ok(/API 503/.test(thrown.message), 'expected 503 error message, got: ' + thrown.message);
    assert.ok(calls <= 12, 'retry cap should keep call count bounded, got ' + calls);
    assert.ok(calls >= 2, 'should have retried at least once, got ' + calls);
  });

  await test('non-503 errors do not trigger retry loop', async () => {
    let calls = 0;
    const ctx = makeCtx(async (url) => {
      if (!/analytics/.test(url || '')) {
        return { ok: true, status: 200, headers: { get: () => null }, json: async () => ({}) };
      }
      calls++;
      return { ok: false, status: 500, headers: { get: () => null }, json: async () => ({}) };
    });
    let thrown = null;
    try { await ctx.api('/analytics/rf'); } catch (e) { thrown = e; }
    assert.ok(thrown, 'expected throw on 500');
    assert.strictEqual(calls, 1, 'should not retry on non-503 errors');
  });

  // === PR #1688 r1 — banner counter leak fixes (adv #1 + munger #4) ===
  // The banner state must end HIDDEN after BOTH a retry-then-success
  // path and a retry-exhausted-throw path. Previously the in-flight
  // counter leaked, leaving the banner stuck visible.
  //
  // We can't read the internal `let _warmupInflight_1659` from outside
  // the vm (let is not attached to globalThis). Instead we hook
  // window.onWarmup_1659 which fires whenever banner state changes,
  // and assert the LAST state is `false` (hidden) — equivalent to
  // counter == 0.

  function withBannerHook(fetchImpl) {
    const ctx = makeCtx(fetchImpl);
    const events = [];
    ctx.window.onWarmup_1659 = (visible) => { events.push(visible); };
    return { ctx, events };
  }

  await test('banner hidden after retry-then-success (no leak)', async () => {
    let calls = 0;
    const { ctx, events } = withBannerHook(async (url) => {
      if (!/analytics/.test(url || '')) {
        return { ok: true, status: 200, headers: { get: () => null }, json: async () => ({}) };
      }
      calls++;
      if (calls < 4) {
        return { ok: false, status: 503, headers: { get: () => '1' }, json: async () => ({}) };
      }
      return { ok: true, status: 200, headers: { get: () => null }, json: async () => ({ ok: 1 }) };
    });
    await ctx.api('/analytics/rf');
    assert.ok(events.length > 0, 'expected at least one banner state change');
    assert.strictEqual(events[events.length - 1], false,
      'banner must end hidden after success (events=' + JSON.stringify(events) + ')');
  });

  await test('banner hidden after retry-exhausted throw (no leak)', async () => {
    const { ctx, events } = withBannerHook(async (url) => {
      if (!/analytics/.test(url || '')) {
        return { ok: true, status: 200, headers: { get: () => null }, json: async () => ({}) };
      }
      return { ok: false, status: 503, headers: { get: () => '1' }, json: async () => ({}) };
    });
    let thrown = null;
    try { await ctx.api('/analytics/rf'); } catch (e) { thrown = e; }
    assert.ok(thrown, 'expected throw after retries exhausted');
    assert.ok(events.length > 0, 'expected at least one banner state change');
    assert.strictEqual(events[events.length - 1], false,
      'banner must end hidden after retry-exhausted throw (events=' + JSON.stringify(events) + ')');
  });

  await test('banner hidden after three parallel analytics endpoints', async () => {
    let calls = 0;
    const { ctx, events } = withBannerHook(async (url) => {
      if (!/analytics/.test(url || '')) {
        return { ok: true, status: 200, headers: { get: () => null }, json: async () => ({}) };
      }
      calls++;
      // Each endpoint takes one 503 then succeeds.
      if (calls % 2 === 1) {
        return { ok: false, status: 503, headers: { get: () => '1' }, json: async () => ({}) };
      }
      return { ok: true, status: 200, headers: { get: () => null }, json: async () => ({}) };
    });
    await Promise.all([
      ctx.api('/analytics/rf'),
      ctx.api('/analytics/topology'),
      ctx.api('/analytics/channels'),
    ]);
    assert.strictEqual(events[events.length - 1], false,
      'banner must end hidden after three parallel calls (events=' + JSON.stringify(events) + ')');
  });

  await test('banner visible DURING retries (not just at the end)', async () => {
    let calls = 0;
    const { ctx, events } = withBannerHook(async (url) => {
      if (!/analytics/.test(url || '')) {
        return { ok: true, status: 200, headers: { get: () => null }, json: async () => ({}) };
      }
      calls++;
      if (calls < 3) {
        return { ok: false, status: 503, headers: { get: () => '1' }, json: async () => ({}) };
      }
      return { ok: true, status: 200, headers: { get: () => null }, json: async () => ({}) };
    });
    await ctx.api('/analytics/rf');
    assert.ok(events.includes(true),
      'banner must have been visible during retries (events=' + JSON.stringify(events) + ')');
    assert.strictEqual(events[events.length - 1], false, 'banner must end hidden');
  });

  console.log('\n=== #172: the analytics page outlasts the server warm-up ===');

  // The server answers 503 + Retry-After: 5 on rf/topology/channels until
  // its background load is done, for up to its 60 s force-open
  // (cmd/server/analytics_warmup_1659.go). These run the REAL app.js api()
  // and the REAL analytics page in one vm, with a fake fetch and a fake
  // clock that drives setTimeout and Date.now.

  await test('a warm-up longer than 30 s ends with data and never shows an error', async () => {
    const env = pageEnv(45000);
    env.page.init(env.el('app'));
    const content = env.el('analyticsContent');
    let sawLoading = false;
    for (let t = 0; t < 60; t++) {
      await env.clock.advance(1000);
      assert.ok(!/Failed to load/.test(content.innerHTML), 'error shown at ' + env.clock.now() + ' ms: ' + content.innerHTML);
      if (env.clock.now() < 45000 && /still loading/i.test(content.innerHTML)) sawLoading = true;
    }
    assert.ok(sawLoading, 'no "still loading" state while the server warmed up');
    assert.ok(/Total Transmissions/.test(content.innerHTML) && /4,321|4321/.test(content.innerHTML),
      'no data after the warm-up: ' + content.innerHTML.slice(0, 200));
  });

  await test('a permanent 503 keeps retrying past the 60 s force-open, then shows the error', async () => {
    const env = pageEnv(Infinity);
    env.page.init(env.el('app'));
    const content = env.el('analyticsContent');
    let errorAt = null;
    for (let t = 0; t < 200 && errorAt === null; t++) {
      await env.clock.advance(1000);
      if (/Failed to load/.test(content.innerHTML)) errorAt = env.clock.now();
    }
    assert.ok(errorAt !== null, 'no error after 200 s of 503s: the retry loop is unbounded');
    assert.ok(errorAt >= 90000, 'gave up after ' + errorAt + ' ms, before the 90 s floor (server force-open is 60 s)');
    assert.ok(errorAt <= 125000, 'gave up only after ' + errorAt + ' ms, past the 120 s cap');
    const before = env.analyticsFetches();
    await env.clock.advance(60000);
    assert.strictEqual(env.analyticsFetches(), before, 'still fetching after giving up');
  });

  await test('leaving the page during the warm-up stops the retries and writes nothing', async () => {
    const env = pageEnv(45000);
    env.page.init(env.el('app'));
    const content = env.el('analyticsContent');
    await env.clock.advance(12000);
    env.page.destroy();
    const fetches = env.analyticsFetches();
    const html = content.innerHTML;
    for (let t = 0; t < 150; t++) await env.clock.advance(1000);
    assert.strictEqual(env.analyticsFetches(), fetches, 'warm-up fetches after destroy()');
    assert.strictEqual(content.innerHTML, html, 'the left page was written after destroy()');
    assert.strictEqual(env.clock.pending().length, 0, 'retry timers left after destroy(): ' + env.clock.pending().length);
  });

  await test('a new load during the warm-up replaces the old one: one retry timer, one render', async () => {
    const env = pageEnv(45000);
    env.page.init(env.el('app'));
    const content = env.el('analyticsContent');
    await env.clock.advance(7000);
    env.regionChanged();   // e.g. a region filter change starts a new load
    let maxTimers = 0;
    for (let t = 0; t < 60; t++) {
      await env.clock.advance(1000);
      maxTimers = Math.max(maxTimers, env.clock.pending().length);
    }
    assert.ok(maxTimers <= 1, maxTimers + ' retry timers pending at once');
    assert.strictEqual(content.dataWrites(), 1, 'the data was rendered ' + content.dataWrites() + ' times (the superseded load rendered too)');
  });

  await test('a slow response of a superseded load does not render over the newer one', async () => {
    const env = pageEnv(0, 3000);
    env.page.init(env.el('app'));               // all regions: answers at 3 s
    const content = env.el('analyticsContent');
    await env.clock.advance(1000);
    env.regionChanged('CPH');                   // region CPH: answers at 4 s
    await env.clock.advance(2500);
    assert.strictEqual(content.dataWrites(), 0, 'the superseded all-regions load rendered: ' + content.innerHTML.slice(0, 120));
    await env.clock.advance(2000);
    assert.strictEqual(content.dataWrites(), 1, 'the CPH load did not render once: ' + content.dataWrites());
    assert.ok(/8,765|8765/.test(content.innerHTML), 'not the CPH data: ' + content.innerHTML.slice(0, 200));
  });

  console.log('\n=== #172 r2: in-flight sharing respects retry503 ===');

  // One request per path is shared while in flight, but a retry503:false
  // caller (analytics.js) and a default caller want different promises:
  // the first must see the 503 at once, the second must ride out the
  // warm-up. rf answers 503 + Retry-After: 5 until 10 s on the fake clock.
  function inflightEnv() {
    const clock = fakeClock();
    let rfFetches = 0;
    const ctx = makeCtx(async (url) => {
      if (!/analytics\/rf/.test(url || '')) {
        return { ok: true, status: 200, headers: { get: () => null }, json: async () => ({}) };
      }
      rfFetches++;
      if (clock.now() < 10000) {
        return { ok: false, status: 503, headers: { get: (k) => (k.toLowerCase() === 'retry-after' ? '5' : null) }, json: async () => ({}) };
      }
      return { ok: true, status: 200, headers: { get: () => null }, json: async () => ({ totalPackets: 7 }) };
    }, clock);
    const track = (p) => {
      const r = { settled: false, value: undefined, error: undefined };
      p.then((v) => { r.settled = true; r.value = v; }, (e) => { r.settled = true; r.error = e; });
      return r;
    };
    return { clock, ctx, track, rfFetches: () => rfFetches };
  }

  await test('a default caller does not join a retry503:false request and gets the data', async () => {
    const env = inflightEnv();
    const noRetry = env.track(env.ctx.api('/analytics/rf', { retry503: false }));
    const dflt = env.track(env.ctx.api('/analytics/rf'));
    await env.clock.advance(1000);
    assert.ok(noRetry.settled && noRetry.error && noRetry.error.status === 503,
      'the retry503:false caller did not get the 503 at once');
    assert.ok(!dflt.settled, 'the default caller settled after 1 s: ' + (dflt.error ? dflt.error.message : JSON.stringify(dflt.value)));
    await env.clock.advance(30000);
    assert.ok(dflt.settled && !dflt.error, 'the default caller failed: ' + (dflt.error && dflt.error.message));
    assert.deepStrictEqual(dflt.value, { totalPackets: 7 });
  });

  await test('a retry503:false caller does not join a retrying request and gets the 503 at once', async () => {
    const env = inflightEnv();
    const dflt = env.track(env.ctx.api('/analytics/rf'));
    await env.clock.advance(100);                          // the default call is now sleeping on Retry-After
    const noRetry = env.track(env.ctx.api('/analytics/rf', { retry503: false }));
    await env.clock.advance(100);
    assert.ok(noRetry.settled, 'the retry503:false caller is waiting on the retrying request');
    assert.ok(noRetry.error && noRetry.error.status === 503, 'not a 503 error: ' + (noRetry.error && noRetry.error.message));
    await env.clock.advance(30000);
    assert.ok(dflt.settled && !dflt.error, 'the default caller failed: ' + (dflt.error && dflt.error.message));
  });

  await test('callers with the same retry503 still share one request', async () => {
    const env = inflightEnv();
    await env.clock.advance(10000);                        // warm: one 200
    const a = env.ctx.api('/analytics/rf', { retry503: false });
    const b = env.ctx.api('/analytics/rf', { retry503: false });
    const c = env.ctx.api('/analytics/rf');
    const d = env.ctx.api('/analytics/rf');
    await env.clock.advance(100);
    await Promise.all([a, b, c, d]);
    assert.strictEqual(env.rfFetches(), 2, 'expected one fetch per retry503 value, got ' + env.rfFetches());
  });

  console.log('\n=== #172 r2: what the analytics page does with an error ===');

  for (const status of [500, 404]) {
    await test('a ' + status + ' shows the error at once and is not retried', async () => {
      const env = pageEnv(Infinity, 0, { status, retryAfter: null });
      env.page.init(env.el('app'));
      const content = env.el('analyticsContent');
      await env.clock.advance(1000);
      assert.ok(/Failed to load/.test(content.innerHTML), 'no error after 1 s: ' + content.innerHTML.slice(0, 200));
      assert.ok(!/still loading/i.test(content.innerHTML), 'a ' + status + ' is shown as "still loading"');
      const fetches = env.analyticsFetches();
      await env.clock.advance(130000);
      assert.strictEqual(env.analyticsFetches(), fetches, 'a ' + status + ' was retried');
      assert.strictEqual(env.clock.pending().length, 0, 'retry timers pending after a ' + status);
    });
  }

  // The retry follows the server's Retry-After, clamped to 1..30 s, and the
  // status line says so (role="status", not an alert).
  for (const [ra, wantMs] of [['1', 1000], ['12', 12000], ['60', 30000], ['abc', 5000]]) {
    await test('a 503 with Retry-After: ' + ra + ' retries after ' + wantMs / 1000 + ' s', async () => {
      const env = pageEnv(Infinity, 0, { retryAfter: ra });
      env.page.init(env.el('app'));
      const content = env.el('analyticsContent');
      await env.clock.advance(1);
      const timers = env.clock.pending();
      assert.strictEqual(timers.length, 1, timers.length + ' timers pending after the first 503');
      assert.strictEqual(timers[0].at - env.clock.now(), wantMs, 'retry timer');
      assert.ok(/role="status"/.test(content.innerHTML), 'the loading state is not role="status": ' + content.innerHTML.slice(0, 200));
      assert.ok(!/role="alert"/.test(content.innerHTML), 'the loading state is an alert');
      assert.ok(content.innerHTML.includes('Retrying in ' + wantMs / 1000 + 's'), 'status text: ' + content.innerHTML.slice(0, 300));
      const fetches = env.analyticsFetches();
      await env.clock.advance(wantMs - 1);
      assert.strictEqual(env.analyticsFetches(), fetches, 'retried before ' + wantMs + ' ms');
      await env.clock.advance(1);
      assert.ok(env.analyticsFetches() > fetches, 'not retried at ' + wantMs + ' ms');
    });
  }

  console.log('\n=== #172 r2: tab clicks while the analytics are still loading ===');

  // These six tabs render from the shared load (_analyticsData). The other
  // tabs fetch their own data and are covered by the browser check.
  const DATA_TABS = ['overview', 'rf', 'topology', 'channels', 'hashsizes', 'collisions'];
  for (const tab of DATA_TABS) {
    await test('clicking "' + tab + '" during the warm-up shows the loading state, then that tab\'s data', async () => {
      const env = pageEnv(45000);
      env.page.init(env.el('app'));
      const content = env.el('analyticsContent');
      await env.clock.advance(6000);
      const before = unexpectedRejections.length;
      env.clickTab(tab);
      await env.clock.advance(100);
      const errs = unexpectedRejections.splice(before);
      assert.strictEqual(errs.length, 0, 'the click threw: ' + errs.map((e) => e && e.message).join('; '));
      assert.ok(/still loading/i.test(content.innerHTML) && /role="status"/.test(content.innerHTML),
        'no loading state after the click: ' + content.innerHTML.slice(0, 200));
      await env.clock.advance(15000);                      // a retry while the tab is shown
      assert.ok(/still loading/i.test(content.innerHTML), 'loading state lost on a retry: ' + content.innerHTML.slice(0, 200));
      await env.clock.advance(40000);
      const late = unexpectedRejections.splice(before);
      assert.strictEqual(late.length, 0, 'rendering threw: ' + late.map((e) => e && e.message).join('; '));
      assert.ok(!/still loading|Failed to load/i.test(content.innerHTML), 'not rendered after the warm-up: ' + content.innerHTML.slice(0, 200));
      if (tab === 'overview') assert.ok(/Total Transmissions/.test(content.innerHTML), 'not the overview');
      if (tab !== 'overview') assert.ok(!/Total Transmissions/.test(content.innerHTML), 'the overview was rendered instead of ' + tab);
    });
  }

  await test('a tab that fetches its own data is not overwritten by the loading state', async () => {
    const env = pageEnv(45000);
    env.page.init(env.el('app'));
    const content = env.el('analyticsContent');
    await env.clock.advance(6000);
    // Route patterns fetches its own data; a click makes it the current
    // tab. What it renders in this fake DOM does not matter: a marker
    // stands in for it, and the warm-up retries must leave it alone.
    const before = unexpectedRejections.length;
    env.clickTab('subpaths');
    await env.clock.advance(100);
    unexpectedRejections.splice(before);
    content.innerHTML = '<div id="own-tab">own data</div>';
    await env.clock.advance(20000);       // several 503 retries
    assert.ok(/own-tab/.test(content.innerHTML), 'the loading state replaced the selected tab: ' + content.innerHTML.slice(0, 200));
  });

  if (unexpectedRejections.length) {
    failed++;
    console.log('  ❌ unexpected unhandled rejections: ' + unexpectedRejections.map((e) => e && e.message).join('; '));
  }

  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  if (failed > 0) process.exit(1);
})();
