/* Tests for the WebSocket client history card on the Perf page */
'use strict';
const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

let passed = 0, failed = 0;
async function test(name, fn) {
  try { await fn(); passed++; console.log(`  ✅ ${name}`); }
  catch (e) { failed++; console.log(`  ❌ ${name}: ${e.message}`); }
}

function makeSandbox() {
  let capturedHtml = '';
  const ctx = {
    window: { addEventListener: () => {}, apiPerf: null },
    document: {
      getElementById: (id) => id === 'perfContent' ? { set innerHTML(v) { capturedHtml = v; } } : null,
      addEventListener: () => {},
    },
    console, Date, Math, Array, Object, String, Number, JSON, RegExp, Error, TypeError,
    parseInt, parseFloat, isNaN, isFinite,
    setTimeout: () => {}, clearTimeout: () => {}, setInterval: () => 0, clearInterval: () => {},
    performance: { now: () => Date.now() },
    Map, Set, Promise,
    registerPage: (name, handler) => { ctx.pages[name] = handler; },
    pages: {},
    _apiCache: null,
    fetch: () => Promise.resolve({ json: () => Promise.resolve({}) }),
  };
  ctx.window.document = ctx.document;
  ctx.globalThis = ctx;
  vm.runInNewContext(fs.readFileSync('public/perf.js', 'utf8'), ctx);
  return { ctx, getHtml: () => capturedHtml };
}

const flush = async () => { for (let i = 0; i < 10; i++) await new Promise(r => setImmediate(r)); };

const T0 = Date.parse('2026-10-13T12:00:00Z');
const mk = (n, f) => Array.from({ length: n }, (_, i) => {
  const c = f(i);
  return { t: new Date(T0 + i * 60000).toISOString(), clients: c, peak: c + 1 };
});

(async () => {
  console.log('perf.js — WS client history');

  await test('buildWSClientChart: scales to the peak and keeps points inside the box', () => {
    const { ctx } = makeSandbox();
    const c = ctx.window.buildWSClientChart(mk(5, i => i * 10), 400, 100);
    assert.strictEqual(c.maxY, 41); // highest peak
    assert.strictEqual(c.clientPoints.length, 5);
    for (const p of c.clientPoints.concat(c.peakPoints)) {
      assert.ok(p.x >= 0 && p.x <= 400, `x ${p.x} outside`);
      assert.ok(p.y >= 0 && p.y <= 100, `y ${p.y} outside`);
    }
    // x grows with time; larger counts sit higher (smaller y)
    assert.ok(c.clientPoints[4].x > c.clientPoints[0].x);
    assert.ok(c.clientPoints[4].y < c.clientPoints[0].y);
  });

  await test('buildWSClientChart: lines at 0 and at max are padded away from the edge', () => {
    const { ctx } = makeSandbox();
    const c = ctx.window.buildWSClientChart(mk(3, i => [0, 5, 0][i]), 400, 100);
    const ys = c.clientPoints.map(p => p.y).concat(c.peakPoints.map(p => p.y));
    assert.ok(Math.min.apply(null, ys) > 0, 'top edge clipped');
    assert.ok(Math.max.apply(null, ys) < 100, 'bottom edge clipped');
  });

  await test('buildWSClientChart: all-zero data does not divide by zero', () => {
    const { ctx } = makeSandbox();
    const zero = [{ t: new Date(T0).toISOString(), clients: 0, peak: 0 }, { t: new Date(T0 + 60000).toISOString(), clients: 0, peak: 0 }];
    const c = ctx.window.buildWSClientChart(zero, 400, 100);
    assert.ok(c.maxY >= 1);
    for (const p of c.clientPoints) assert.ok(isFinite(p.x) && isFinite(p.y));
  });

  await test('buildWSClientChart: coerces string values from the API', () => {
    const { ctx } = makeSandbox();
    const s = [{ t: new Date(T0).toISOString(), clients: '3', peak: '4' }, { t: new Date(T0 + 60000).toISOString(), clients: '5', peak: '6' }];
    const c = ctx.window.buildWSClientChart(s, 400, 100);
    assert.strictEqual(c.maxY, 6);
    assert.strictEqual(c.latest, 5);
    assert.strictEqual(c.peakOverall, 6);
  });

  await test('buildWSClientChart: reports latest and 24h peak', () => {
    const { ctx } = makeSandbox();
    const c = ctx.window.buildWSClientChart(mk(4, i => [2, 9, 4, 3][i]), 400, 100);
    assert.strictEqual(c.latest, 3);
    assert.strictEqual(c.peakOverall, 10);
  });

  await test('renderWSClientHistory: fewer than 2 samples shows a collecting message, no svg', () => {
    const { ctx } = makeSandbox();
    for (const s of [[], mk(1, () => 3), null, undefined]) {
      const html = ctx.window.renderWSClientHistory({ intervalSec: 60, samples: s });
      assert.ok(!html.includes('<svg class="ws-history'), 'no chart expected');
      assert.ok(/collecting/i.test(html));
    }
    assert.strictEqual(ctx.window.renderWSClientHistory(null), '');
  });

  await test('renderWSClientHistory: renders svg with both series and the numbers', () => {
    const { ctx } = makeSandbox();
    const html = ctx.window.renderWSClientHistory({ intervalSec: 60, samples: mk(30, i => 20 + (i % 5)) });
    assert.ok(html.includes('<svg class="ws-history'));
    assert.strictEqual((html.match(/<polyline/g) || []).length, 2);
    assert.ok(html.includes('WebSocket'));
    assert.ok(/stroke="var\(--/.test(html), 'colours must be CSS variables');
    assert.ok(!/#[0-9a-fA-F]{3,6}\b/.test(html.replace(/&#\d+;/g, '')), 'no hardcoded hex colours');
  });

  await test('refresh: fetches /api/perf/ws-clients once and shows the card', async () => {
    const { ctx, getHtml } = makeSandbox();
    const calls = [];
    const perf = { totalRequests: 1, avgMs: 1, uptime: 60, slowQueries: [], endpoints: {}, cache: null, packetStore: null, sqlite: null };
    ctx.fetch = (url) => {
      calls.push(url);
      if (url === '/api/health') return Promise.reject(new Error('no health'));
      const body = url === '/api/perf' ? perf
        : url === '/api/perf/ws-clients' ? { intervalSec: 60, samples: mk(10, i => i) }
        : {};
      return Promise.resolve({ json: () => Promise.resolve(body) });
    };
    ctx.document.getElementById = (id) => id === 'perfContent' ? { set innerHTML(v) { ctx.__html = v; } } : null;
    const app = { set innerHTML(v) {} };
    ctx.pages.perf.init(app);
    await flush();
    assert.strictEqual(calls.filter(u => u === '/api/perf/ws-clients').length, 1);
    assert.ok(String(ctx.__html).includes('<svg class="ws-history'), String(ctx.__html).slice(0, 200));
  });

  await test('refresh: a failing ws-clients endpoint does not break the page', async () => {
    const { ctx } = makeSandbox();
    const perf = { totalRequests: 1, avgMs: 1, uptime: 60, slowQueries: [], endpoints: {}, cache: null, packetStore: null, sqlite: null };
    ctx.fetch = (url) => (url === '/api/perf/ws-clients' || url === '/api/health')
      ? Promise.reject(new Error('boom'))
      : Promise.resolve({ json: () => Promise.resolve(url === '/api/perf' ? perf : {}) });
    ctx.document.getElementById = (id) => id === 'perfContent' ? { set innerHTML(v) { ctx.__html = v; } } : null;
    ctx.pages.perf.init({ set innerHTML(v) {} });
    await flush();
    assert.ok(String(ctx.__html).includes('Total Requests'), 'rest of the page still renders');
  });

  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
