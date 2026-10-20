#!/usr/bin/env node
'use strict';

// Drive the REAL nav block, shared api cache and WebSocket invalidation with
// a deterministic clock. Count fetches, not just scheduler invocations.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(process.env.NAV_STATS_APP_SOURCE || 'public/app.js', 'utf8');

function section(start, end) {
  const a = source.indexOf(start), b = source.indexOf(end, a + start.length);
  assert.ok(a >= 0 && b > a, 'production section not found: ' + start);
  return source.slice(a, b);
}
async function flush() { for (let i = 0; i < 24; i++) await Promise.resolve(); }
function events() {
  const listeners = new Map();
  return {
    addEventListener(name, fn) { if (!listeners.has(name)) listeners.set(name, new Set()); listeners.get(name).add(fn); },
    removeEventListener(name, fn) { if (listeners.has(name)) listeners.get(name).delete(fn); },
    emit(name, event = {}) { for (const fn of [...(listeners.get(name) || [])]) fn(event); },
    count(name) { return (listeners.get(name) || new Set()).size; },
  };
}
function harness({ hidden = false, firstStatus = 200, parked = false } = {}) {
  let now = 100000, wallOffset = 0, nextId = 0, layoutRuns = 0;
  const timers = new Map(), requests = [], replies = [], classList = { add() {}, remove() {} };
  const nav = { innerHTML: '', querySelectorAll: () => [{ classList }, { classList }, { classList }] };
  const window = events(), document = Object.assign(events(), {
    hidden, getElementById: id => id === 'navStats' ? nav : null,
  });
  const timer = (fn, ms, interval) => {
    const id = ++nextId;
    timers.set(id, { fn, due: now + Math.max(0, Number(ms) || 0), interval });
    return id;
  };
  const answer = (status, count) => ({
    ok: status >= 200 && status < 300, status,
    headers: { get: () => status === 503 ? '1' : null },
    json: async () => ({ totalPackets: count, totalNodes: 2, totalObservers: 3 }),
  });
  class ClockDate extends Date { static now() { return now + wallOffset; } }
  const context = vm.createContext({
    window, document, console: { warn() {} }, Date: ClockDate,
    performance: { now: () => now },
    setTimeout: (fn, ms) => timer(fn, ms, 0), clearTimeout: id => timers.delete(id),
    setInterval: (fn, ms) => timer(fn, ms, ms), clearInterval: id => timers.delete(id),
    location: { protocol: 'http:', host: 'localhost' },
    navPriorityFn: () => layoutRuns++,
    fetch: async url => {
      if (url === '/api/config/cache') return { json: async () => ({}) };
      const request = { url, time: now };
      requests.push(request);
      const count = requests.length;
      const status = requests.length === 1 ? firstStatus : 200;
      if (parked) return new Promise(resolve => replies.push(() => resolve(answer(status, count))));
      return answer(status, count);
    },
    WebSocket: class { close() {} },
    Logo: { setConnected() {}, pulse() {} },
    checkWSLiveness() {}, dropWS() {}, WS_STALE_MS: 75000, WS_HEARTBEAT: '{"type":"heartbeat"}',
    ws: null, wsListeners: [], wsReconnectTimer: null, wsWatchdogTimer: null, wsLastFrameAt: 0,
    _warmupNotify_1659() {},
  });
  vm.runInContext(section('const _apiPerf =', '// Issue #1659: minimal'), context);
  vm.runInContext(section('function invalidateApiCache(', '// Expose for console'), context);
  vm.runInContext(section('function connectWS()', '// Timers in a hidden'), context);
  vm.runInContext(section('function onWS(', '// --- Pull-to-reconnect'), context);
  vm.runInContext(section('function debouncedOnWS(', '// --- Router ---'), context);
  vm.runInContext('connectWS();', context);
  vm.runInContext(section('// --- Nav Stats ---', '// --- Theme Customization ---'), context);
  return {
    context, document, window, nav, requests, timers,
    count: () => requests.filter(r => r.url === '/api/stats').length,
    times: () => requests.filter(r => r.url === '/api/stats').map(r => r.time - 100000),
    layoutRuns: () => layoutRuns,
    cacheHits: () => vm.runInContext('_apiPerf.cacheHits', context),
    jumpWall(ms) { wallOffset += ms; },
    ws() { vm.runInContext('ws.onmessage({ data: JSON.stringify({ type: "new_data" }) });', context); },
    async visibility(value) { document.hidden = value; document.emit('visibilitychange'); await flush(); },
    async advance(ms) {
      const target = now + ms;
      let iterations = 0;
      while (true) {
        const next = [...timers.entries()].filter(([, t]) => t.due <= target).sort((a, b) => a[1].due - b[1].due || a[0] - b[0])[0];
        if (!next) break;
        assert.ok(++iterations < 100000, 'timer loop must stay bounded');
        now = next[1].due;
        if (next[1].interval) next[1].due += next[1].interval;
        else timers.delete(next[0]);
        next[1].fn();
        await flush();
      }
      now = target;
      await flush();
    },
    async answer() { assert.ok(replies.length, 'no parked fetch'); replies.shift()(); await flush(); },
    async otherStats() { await vm.runInContext("api('/stats', { ttl: CLIENT_TTL.stats })", context); await flush(); },
  };
}
async function traffic(h, seconds) {
  for (let i = 0; i < seconds; i++) { h.ws(); await h.advance(1000); }
}
async function measurements() {
  const busy = harness(); await flush(); await traffic(busy, 60);
  const idle = harness(); await flush(); await idle.advance(60000);
  const hidden = harness({ hidden: true }); await flush(); await traffic(hidden, 60);
  return { simulatedSeconds: 60, sustained: busy.count(), idle: idle.count(), initiallyHidden: hidden.count(), sustainedTimesMs: busy.times() };
}

let passed = 0, failed = 0;
async function test(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (error) { failed++; console.error('  ✗ ' + name + ': ' + error.stack); }
}
async function main() {
  if (process.argv.includes('--measure')) { console.log(JSON.stringify(await measurements())); return; }
  await test('initial refresh renders the real counts and retains layout coalescing', async () => {
    const h = harness(); await flush();
    assert.equal(h.count(), 1); assert.match(h.nav.innerHTML, /1<\/span> pkts/);
    assert.equal(h.layoutRuns(), 1);
  });
  await test('sustained WS invalidations cannot cause network starts less than 15 seconds apart', async () => {
    const h = harness(); await flush(); await traffic(h, 60);
    assert.deepEqual(h.times(), [0, 15000, 30000, 45000, 60000]);
  });
  await test('a large burst uses one timer, no immediate extra network request', async () => {
    const h = harness(); await flush();
    for (let i = 0; i < 10000; i++) h.ws();
    assert.ok(h.timers.size <= 4, 'no per-message timer/queue');
    await h.advance(14999); assert.equal(h.count(), 1);
    await h.advance(1); assert.equal(h.count(), 2);
  });
  await test('idle foreground still refreshes every 15 seconds without WS traffic', async () => {
    const h = harness(); await flush(); await h.advance(60000);
    assert.deepEqual(h.times(), [0, 15000, 30000, 45000, 60000]);
  });
  await test('initially hidden load makes zero nav requests until visible', async () => {
    const h = harness({ hidden: true }); await flush(); await traffic(h, 60);
    assert.equal(h.count(), 0); assert.equal(h.nav.innerHTML, '');
    await h.visibility(false); assert.equal(h.count(), 1);
    assert.match(h.nav.innerHTML, /1<\/span> pkts/);
  });
  await test('hidden interval/WS traffic does not burst; resume refreshes immediately once', async () => {
    const h = harness(); await flush(); await h.advance(1000);
    await h.visibility(true); await traffic(h, 60); assert.equal(h.count(), 1);
    await h.visibility(false); assert.equal(h.count(), 2);
    for (let i = 0; i < 10; i++) await h.visibility(false);
    assert.equal(h.count(), 2); await h.advance(14999); assert.equal(h.count(), 2);
    await h.advance(1); assert.equal(h.count(), 3);
  });
  await test('other stats consumers keep the shared TTL and WS invalidation semantics', async () => {
    const h = harness(); await flush();
    await h.otherStats(); assert.equal(h.count(), 1); assert.equal(h.cacheHits(), 1);
    h.ws(); await h.advance(5000); await h.otherStats();
    assert.equal(h.count(), 2, 'other consumers may fetch inside the nav throttle');
    await h.advance(1000); h.ws(); await h.advance(5000);
    await h.advance(4000); assert.equal(h.count(), 3, 'nav refresh after independent cache invalidation');
  });
  await test('an in-flight nav fetch never overlaps and idle polling resumes after completion', async () => {
    const h = harness({ parked: true }); await flush(); await traffic(h, 40);
    assert.equal(h.count(), 1); await h.answer(); assert.equal(h.count(), 1);
    // The just-completed response fills the shared TTL cache: an overdue
    // tick can reuse it, but must not count as a second network request.
    await h.advance(0); assert.equal(h.count(), 1);
    await h.advance(15000); assert.equal(h.count(), 2);
    await h.answer(); await h.advance(15000); assert.equal(h.count(), 3);
  });
  await test('other consumers still coalesce their own in-flight requests', async () => {
    const h = harness({ hidden: true, parked: true }); await flush();
    const a = h.otherStats(), b = h.otherStats(); await flush();
    assert.equal(h.count(), 1); await h.answer(); await Promise.all([a, b]);
    assert.equal(h.count(), 1);
  });
  await test('nav no-retry and default consumers intentionally use separate in-flight contracts', async () => {
    const h = harness({ parked: true }); await flush();
    const other = h.otherStats(); await flush();
    assert.equal(h.count(), 2, 'different retry policies cannot share a retrying promise');
    await h.answer(); await h.answer(); await other;
    await h.otherStats(); assert.equal(h.count(), 2, 'completed TTL data is still shared');
    assert.equal(h.cacheHits(), 1);
  });
  await test('failed requests retry only on the next foreground cadence', async () => {
    const h = harness({ firstStatus: 500 }); await flush(); await traffic(h, 14);
    assert.equal(h.count(), 1); await h.advance(1000); assert.equal(h.count(), 2);
    assert.match(h.nav.innerHTML, /2<\/span> pkts/);
  });
  await test('503 has no automatic retry after hiding; foreground recovery is prompt', async () => {
    const h = harness({ firstStatus: 503 }); await flush(); await h.visibility(true);
    await h.advance(60000); assert.equal(h.count(), 1, 'no shared API retry loop for nav');
    await h.visibility(false); assert.equal(h.count(), 2);
    assert.match(h.nav.innerHTML, /2<\/span> pkts/);
  });
  await test('other consumers retain the default shared API 503-retry behavior', async () => {
    const h = harness({ hidden: true, firstStatus: 503 }); await flush();
    const request = h.otherStats(); await flush(); assert.equal(h.count(), 1);
    await h.advance(1000); await request;
    assert.equal(h.count(), 2, 'only nav opts out of the shared retry loop');
  });
  await test('hidden completion neither renders nor starts another fetch', async () => {
    const h = harness({ parked: true }); await flush(); await h.visibility(true); await h.answer();
    assert.equal(h.nav.innerHTML, ''); await h.advance(60000); assert.equal(h.count(), 1);
    await h.visibility(false); assert.equal(h.count(), 2); await h.answer();
    assert.match(h.nav.innerHTML, /2<\/span> pkts/);
  });
  await test('hide/show then pagehide keeps the original in-flight request without stale rendering', async () => {
    const h = harness({ parked: true }); await flush();
    await h.visibility(true); await h.advance(30000); await h.visibility(false);
    await h.advance(30000); assert.equal(h.count(), 1);
    h.window.emit('pagehide', { persisted: false }); await h.answer();
    await h.advance(30000); assert.equal(h.count(), 1); assert.equal(h.nav.innerHTML, '');
    assert.equal(h.layoutRuns(), 0, 'no layout callback for a disposed response');
  });
  await test('wall-clock jumps cannot change the monotonic foreground cadence', async () => {
    const h = harness(); await flush();
    h.jumpWall(-3600000); await traffic(h, 30);
    h.jumpWall(7200000); await traffic(h, 30);
    assert.deepEqual(h.times(), [0, 15000, 30000, 45000, 60000]);
  });
  await test('pagehide removes owned listeners and timers; late responses cannot render', async () => {
    const h = harness({ parked: true }); await flush();
    h.window.emit('pagehide', { persisted: false }); await h.answer();
    await traffic(h, 60); await h.visibility(true); await h.visibility(false);
    assert.equal(h.count(), 1); assert.equal(h.nav.innerHTML, '');
    assert.equal(h.document.count('visibilitychange'), 0);
    assert.equal(vm.runInContext('wsListeners.length', h.context), 0);
    assert.equal(h.window.count('pagehide'), 0); assert.equal(h.window.count('pageshow'), 0);
  });
  await test('bfcache restores one poller and SPA events do not recreate it', async () => {
    const h = harness(); await flush();
    h.window.emit('pagehide', { persisted: true }); await h.advance(60000);
    assert.equal(h.count(), 1); h.window.emit('pageshow', { persisted: true }); await flush();
    assert.equal(h.count(), 2); h.window.emit('pageshow', { persisted: true }); await flush();
    assert.equal(h.count(), 2); assert.equal(h.document.count('visibilitychange'), 1);
    assert.equal(vm.runInContext('wsListeners.length', h.context), 1);
    h.window.emit('hashchange'); await traffic(h, 15); assert.equal(h.count(), 3);
  });
  console.log(`\n${passed} passed, ${failed} failed`);
  if (failed) process.exitCode = 1;
}
if (require.main === module) main().catch(error => { console.error(error); process.exitCode = 1; });
module.exports = { harness, flush, traffic, measurements };
