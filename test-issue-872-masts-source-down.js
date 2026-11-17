/* test-issue-872-masts-source-down.js — #872: the browser side of the
 * Mastedatabasen circuit breaker (public/masts-layer.js).
 *
 * When the server's breaker is open, an uncached cell answers 503 with
 * X-CoreScope-Masts-Source: down and Retry-After = the remaining cool-down
 * (cmd/server/masts_breaker.go). The layer must then:
 *   1. read that answer as "source down", distinct from a busy 503;
 *   2. not ask the cell again until Retry-After has passed (up to the
 *      server's longest cool-down, not the 60 s busy cap), and not count it
 *      against the busy retries, so it is asked again after the cool-down;
 *   3. ask each cell at most once per cool-down: no retry storm;
 *   4. say "Antenna data source (Mastedatabasen) is not responding — try
 *      again later" instead of "Loading antenna sites…", while the zoom
 *      hint, the too-wide hint and the partial-failure text stay;
 *   5. (owner addition, stale-if-error) read a 200 with
 *      X-CoreScope-Masts-Stale: 1 as a stale cell, show its sites, and say
 *      "Antenna data may be out of date (source not responding)" while a
 *      visible cell is stale.
 *
 * vm sandbox, the real file; nothing here contacts any server.
 * Runs via: node test-issue-872-masts-source-down.js
 */
'use strict';
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { makeSandbox } = require('./tests/map-overlay-sandbox');

let passed = 0, failed = 0;
const pending = [];
function test(name, fn) {
  const done = (e) => {
    if (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); } else { passed++; console.log('  ✅ ' + name); }
  };
  try {
    const r = fn();
    if (r && typeof r.then === 'function') { pending.push(r.then(() => done(), done)); return; }
    done();
  } catch (e) { done(e); }
}
const tick = () => new Promise((r) => setImmediate(r));

const MASTS = {
  enabled: true, url: '/api/masts', minZoom: 13, cellSizeM: 2000,
  attribution: 'Antenna positions: Mastedatabasen, Digitaliseringsstyrelsen (CC0)',
  sourceUrl: 'https://mastedatabasen.dk/', cellExtent: [219, 3017, 462, 3209],
};
const ML = makeSandbox({ masts: MASTS }).window.MastsLayer;
const DOWN_TEXT = 'Antenna data source (Mastedatabasen) is not responding — try again later';
const STALE_TEXT = 'Antenna data may be out of date (source not responding)';
const GO = fs.readFileSync(path.join(__dirname, 'cmd', 'server', 'masts_breaker.go'), 'utf8');
const GO_PROXY = fs.readFileSync(path.join(__dirname, 'cmd', 'server', 'masts_proxy.go'), 'utf8');

function harness(opts) {
  const calls = [];
  const timers = [];
  let t = 0;
  const loader = ML.createCellLoader(Object.assign({
    fetchCell: (key) => new Promise((resolve) => calls.push({ key, resolve })),
    now: () => t,
    setTimeout: (fn, ms) => { const h = { fn, at: t + ms }; timers.push(h); return h; },
    clearTimeout: (h) => { const i = timers.indexOf(h); if (i >= 0) timers.splice(i, 1); },
  }, opts || {}));
  return {
    loader, calls,
    advance(ms) { t += ms; timers.filter((h) => h.at <= t).forEach((h) => { timers.splice(timers.indexOf(h), 1); h.fn(); }); },
  };
}
const down = (retryAfterS) => ({ status: 503, retryAfterS: String(retryAfterS), sourceDown: true });

// ── 1. Reading the response ─────────────────────────────────────────────────

function fakeResponse(status, headers, body) {
  const h = new Map(Object.entries(headers || {}).map(([k, v]) => [k.toLowerCase(), v]));
  return { status, ok: status >= 200 && status < 300, headers: { get: (k) => (h.has(k.toLowerCase()) ? h.get(k.toLowerCase()) : null) }, json: () => Promise.resolve(body) };
}

test('the header name is the server\'s', () => {
  const m = /mastsSourceHeader\s*=\s*"([^"]+)"/.exec(GO);
  assert(m, 'mastsSourceHeader not found in masts_breaker.go');
  assert.strictEqual(ML.SOURCE_HEADER, m[1]);
});

test('cellResult: a source-down 503 is marked; a busy 503 is not', async () => {
  const d = await ML.cellResult(fakeResponse(503, { 'Retry-After': '290', 'X-CoreScope-Masts-Source': 'down' }));
  assert.strictEqual(d.status, 503);
  assert.strictEqual(d.sourceDown, true);
  assert.strictEqual(String(d.retryAfterS), '290');
  const b = await ML.cellResult(fakeResponse(503, { 'Retry-After': '5' }));
  assert.strictEqual(b.status, 503);
  assert(!b.sourceDown, 'a busy 503 read as source down');
  const ok = await ML.cellResult(fakeResponse(200, {}, { sites: [] }));
  assert.strictEqual(ok.status, 200);
  assert.deepStrictEqual(JSON.parse(JSON.stringify(ok.data)), { sites: [] });
});

// ── 2. No retry before the cool-down, and it is not a busy retry ────────────

test('a source-down cell waits out Retry-After (beyond the 60 s busy cap), then is asked again', async () => {
  const h = harness();
  h.loader.view(['300,3100']);
  h.calls[0].resolve(down(300));
  await tick();
  let s = h.loader.stats();
  assert.strictEqual(s.down, 1, 'stats.down ' + JSON.stringify(s));
  assert.strictEqual(s.pending, 0, 'a source-down cell counted as pending');
  assert.strictEqual(h.loader.state('300,3100'), 'down');
  h.advance(299999);
  assert.strictEqual(h.calls.length, 1, 'asked again before the cool-down ended');
  h.advance(1);
  assert.strictEqual(h.calls.length, 2, 'not asked again after the cool-down');
  h.calls[1].resolve({ status: 200, data: { sites: [] } });
  await tick();
  s = h.loader.stats();
  assert.strictEqual(s.loaded, 1);
  assert.strictEqual(s.down, 0);
});

test('the wait is bounded by the server\'s longest cool-down', async () => {
  const max = Number(/mastsBreakerCooldownMaxSec\s*=\s*(\d+)/.exec(GO)[1]);
  const h = harness();
  h.loader.view(['300,3100']);
  h.calls[0].resolve(down(10 * max));
  await tick();
  h.advance(max * 1000);
  assert.strictEqual(h.calls.length, 2, 'a huge Retry-After was not capped at ' + max + ' s');
});

test('source-down answers never use up the busy retries', async () => {
  const h = harness();
  h.loader.view(['300,3100']);
  for (let i = 0; i < 6; i++) {
    h.calls[i].resolve(down(30));
    await tick();
    assert.notStrictEqual(h.loader.state('300,3100'), 'failed', 'failed after ' + (i + 1) + ' source-down answers');
    h.advance(30000);
    assert.strictEqual(h.calls.length, i + 2);
  }
});

// ── 3. No retry storm ───────────────────────────────────────────────────────

test('no retry storm: each cell of a view is asked at most once per cool-down', async () => {
  const h = harness();
  const keys = Array.from({ length: 40 }, (_, i) => (300 + i) + ',3100');
  h.loader.view(keys);
  for (let r = 0; r < 20; r++) {
    h.calls.filter((c) => !c.done).forEach((c) => { c.done = true; c.resolve(down(300)); });
    await tick();
  }
  assert.strictEqual(h.calls.length, 40, h.calls.length + ' requests for 40 cells');
  // Panning around inside the cool-down asks nothing again.
  h.loader.view(keys.slice(10));
  h.loader.view(keys);
  h.advance(60000);
  assert.strictEqual(h.calls.length, 40, 'asked again within the cool-down');
  assert.strictEqual(h.loader.stats().down, 40);
});

// ── 4. The status line ──────────────────────────────────────────────────────

test('status line: source down replaces "Loading…"; zoom, too-wide and partial-failure texts stay', () => {
  const s0 = { loaded: 0, pending: 0, failed: 0, truncated: 0, down: 0 };
  const st = (z, s, wide) => ML.statusText(z, MASTS, true, Object.assign({}, s0, s), !!wide);
  assert.strictEqual(st(13, { down: 3 }), DOWN_TEXT);
  assert.strictEqual(st(13, { down: 3, pending: 5 }), DOWN_TEXT, 'Loading… shown while the source is down');
  assert.strictEqual(st(13, { down: 1, failed: 2 }), DOWN_TEXT);
  assert.strictEqual(st(12, { down: 3 }), 'Zoom in to 13+ to see antenna positions');
  assert.strictEqual(st(13, { down: 3 }, true), 'This view is too wide for antenna positions; zoom in one more step');
  assert.strictEqual(st(13, { pending: 2 }), 'Loading antenna sites…');
  assert.strictEqual(st(13, { failed: 1 }), 'Antenna data unavailable for some areas');
  assert.strictEqual(ML.statusText(13, MASTS, false, Object.assign({}, s0, { down: 3 }), false), '');
});

// ── 5. Stale-if-error ───────────────────────────────────────────────────────

test('the stale header name is the server\'s', () => {
  const m = /mastsStaleHeader\s*=\s*"([^"]+)"/.exec(GO_PROXY);
  assert(m, 'mastsStaleHeader not found in masts_proxy.go');
  assert.strictEqual(ML.STALE_HEADER, m[1]);
});

test('cellResult: a 200 marked stale is stale; a plain 200 is not', async () => {
  const s = await ML.cellResult(fakeResponse(200, { 'X-CoreScope-Masts-Stale': '1' }, { sites: [{ id: 1 }] }));
  assert.strictEqual(s.status, 200);
  assert.strictEqual(s.stale, true);
  assert.strictEqual(s.data.sites.length, 1);
  const f = await ML.cellResult(fakeResponse(200, {}, { sites: [] }));
  assert(!f.stale, 'a fresh 200 read as stale');
});

test('a stale cell is loaded (its sites are drawn) and counted as stale in the view only', async () => {
  const loads = [];
  const h = harness({ onLoad: (k) => loads.push(k) });
  h.loader.view(['300,3100', '301,3100']);
  h.calls[0].resolve({ status: 200, data: { sites: [] }, stale: true });
  h.calls[1].resolve({ status: 200, data: { sites: [] } });
  await tick();
  assert.deepStrictEqual(loads, ['300,3100', '301,3100']);
  assert.strictEqual(h.loader.state('300,3100'), 'loaded');
  assert(h.loader.data('300,3100'), 'stale data not handed out');
  const all = h.loader.stats();
  assert.strictEqual(all.loaded, 2);
  assert.strictEqual(all.stale, 1);
  assert.strictEqual(h.loader.stats(['301,3100']).stale, 0, 'a fresh visible cell counted as stale');
});

test('status line: the stale note shows while a visible cell is stale; source-down and zoom texts win', () => {
  const s0 = { loaded: 0, pending: 0, failed: 0, truncated: 0, down: 0, stale: 0 };
  const st = (z, s, wide) => ML.statusText(z, MASTS, true, Object.assign({}, s0, s), !!wide);
  assert.strictEqual(st(13, { loaded: 4, stale: 1 }), STALE_TEXT);
  assert.strictEqual(st(13, { loaded: 4, stale: 1, pending: 2 }), STALE_TEXT);
  assert.strictEqual(st(13, { loaded: 4, stale: 1, failed: 1 }), STALE_TEXT);
  assert.strictEqual(st(13, { loaded: 4, stale: 1, truncated: 1 }), STALE_TEXT);
  assert.strictEqual(st(13, { stale: 1, down: 2 }), DOWN_TEXT);
  assert.strictEqual(st(12, { stale: 1 }), 'Zoom in to 13+ to see antenna positions');
  assert.strictEqual(st(13, { loaded: 4 }), '');
});

Promise.all(pending).then(() => {
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
});
