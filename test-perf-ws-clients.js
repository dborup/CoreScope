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

function makeSandbox(hash = '#/perf') {
  let capturedHtml = '';
  let historyHtml = '';
  let now = Date.now();
  class ClockDate extends Date { static now() { return now; } }
  const handlers = {};
  const rangeSelect = { value: '24h', addEventListener: (ev, fn) => { handlers.range = fn; } };
  const ctx = {
    window: { addEventListener: () => {}, apiPerf: null, location: { hash }, history: {
      replaceState: (state, title, value) => { ctx.window.location.hash = value; },
    } },
    document: {
      getElementById: (id) => {
        if (id === 'perfContent') return { set innerHTML(v) { capturedHtml = v; } };
        if (id === 'wsHistoryContent') return { set innerHTML(v) { historyHtml = v; } };
        if (id === 'wsHistoryRange') return rangeSelect;
        if (id === 'perfRefresh' || id === 'perfReset') return { addEventListener: (ev, fn) => { handlers[id] = fn; } };
        return null;
      },
      addEventListener: () => {},
      removeEventListener: () => {},
    },
    console, Date: ClockDate, Math, Array, Object, String, Number, JSON, RegExp, Error, TypeError,
    parseInt, parseFloat, isNaN, isFinite,
    setTimeout: () => {}, clearTimeout: () => {}, setInterval: () => 0, clearInterval: () => {},
    performance: { now: () => Date.now() },
    Map, Set, Promise, URLSearchParams,
    registerPage: (name, handler) => { ctx.pages[name] = handler; },
    pages: {},
    _apiCache: null,
    fetch: () => Promise.resolve({ ok: true, json: () => Promise.resolve({}) }),
  };
  ctx.window.document = ctx.document;
  ctx.globalThis = ctx;
  vm.runInNewContext(fs.readFileSync('public/perf.js', 'utf8'), ctx);
  return { ctx, handlers, rangeSelect, advanceNow: ms => { now += ms; }, getHtml: () => capturedHtml, getHistoryHtml: () => historyHtml };
}

const flush = async () => { for (let i = 0; i < 10; i++) await new Promise(r => setImmediate(r)); };

const T0 = Date.parse('2026-10-13T12:00:00Z');
const mk = (n, f) => Array.from({ length: n }, (_, i) => {
  const c = f(i);
  return { t: new Date(T0 + i * 60000).toISOString(), clients: c, peak: c + 1 };
});

// perf.js's "Go Runtime" card dereferences server.goRuntime.* and
// health.websocket.clients when health.engine === 'go'; tests that supply a
// `health` object need this shape too or refresh() throws before it ever
// gets to the WS-history card.
const PERF_FIXTURE = () => ({
  totalRequests: 1, avgMs: 1, uptime: 60, slowQueries: [], endpoints: {}, cache: null, packetStore: null, sqlite: null,
  goRuntime: { goroutines: 5, numGC: 1, pauseTotalMs: 1, lastPauseMs: 1, heapAllocMB: 1, heapSysMB: 1, heapInuseMB: 1, heapIdleMB: 1, numCPU: 4 },
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
    assert.ok(c.peakSegments[0][1].y < c.clientSegments[0][1].y, 'aggregate peak must not become the last count');
  });

  await test('buildWSClientChart: restart/missing intervals split both lines rather than inventing continuity', () => {
    const { ctx } = makeSandbox();
    const samples = mk(6, i => i + 2);
    samples[2].gapBefore = true; // Explicit restart within an aggregate bucket.
    samples[4].t = new Date(T0 + 20 * 60000).toISOString();
    samples[5].t = new Date(T0 + 21 * 60000).toISOString();
    const c = ctx.window.buildWSClientChart(samples, 400, 100, 60);
    assert.deepStrictEqual(Array.from(c.clientSegments, s => s.length), [2, 2, 2]);
    assert.deepStrictEqual(Array.from(c.peakSegments, s => s.length), [2, 2, 2]);
    assert.strictEqual(c.clientPoints.length, 6, 'no zero-filled samples');
  });

  await test('renderWSClientHistory: 7d/30d show their period, dates and actual aggregate interval', () => {
    const { ctx } = makeSandbox();
    for (const [range, intervalSec, label] of [['7d', 300, '7 days'], ['30d', 900, '30 days']]) {
      const samples = [
        { t: '2026-10-01T12:00:00Z', clients: 2, peak: 5 },
        { t: '2026-10-02T12:00:00Z', clients: 3, peak: 6, gapBefore: true },
      ];
      const html = ctx.window.renderWSClientHistory({ range, intervalSec, samples, persistence: { enabled: true, healthy: true } }, 8);
      assert.ok(html.includes(label), html);
      assert.ok(html.includes('2026'), 'multi-day labels must include the date, not just the clock');
      assert.ok(html.includes(String(intervalSec / 60) + '-minute'), 'describe aggregated samples');
      assert.ok(/gaps/i.test(html), 'explain missing intervals');
      assert.ok(/survives restarts/i.test(html), 'confirm enabled healthy persistence');
      assert.ok(/<b>8<\/b> open now/.test(html), 'open now is not the aggregate value');
      assert.strictEqual((html.match(/<polyline/g) || []).length, 4, 'separate segments across restart');
    }
  });

  await test('renderWSClientHistory: persistence disabled, unhealthy and legacy metadata are not advertised as durable', () => {
    const { ctx } = makeSandbox();
    const base = { range: '30d', intervalSec: 900, samples: mk(2, () => 2) };
    assert.ok(/disabled/i.test(ctx.window.renderWSClientHistory({ ...base, persistence: { enabled: false, healthy: false } })));
    assert.ok(/unavailable/i.test(ctx.window.renderWSClientHistory({ ...base, persistence: { enabled: true, healthy: false } })));
    assert.ok(!/survives restarts/i.test(ctx.window.renderWSClientHistory(base)), 'legacy API cannot prove persistence');
  });

  await test('refresh: deep-linked ranges, range-specific cache and URL state use the real page handlers', async () => {
    const sb = makeSandbox('#/perf?other=keep&wsRange=7d');
    const { ctx, handlers, rangeSelect } = sb;
    const calls = [];
    ctx.fetch = url => {
      calls.push(url);
      const range = url.includes('range=30d') ? '30d' : url.includes('range=7d') ? '7d' : '24h';
      const body = url.startsWith('/api/perf/ws-clients') ? { range, intervalSec: range === '7d' ? 300 : 900, samples: mk(10, () => range === '30d' ? 30 : 7) }
        : url === '/api/perf' ? PERF_FIXTURE() : null;
      return Promise.resolve({ ok: true, json: () => Promise.resolve(body) });
    };
    ctx.pages.perf.init({ set innerHTML(v) {} });
    await flush();
    assert.strictEqual(rangeSelect.value, '7d');
    assert.ok(sb.getHistoryHtml().includes('7 days'));
    rangeSelect.value = '30d';
    await handlers.range({ target: rangeSelect });
    await flush();
    assert.strictEqual(ctx.window.location.hash, '#/perf?other=keep&wsRange=30d');
    assert.ok(sb.getHistoryHtml().includes('30 days'));
    rangeSelect.value = '7d';
    await handlers.range({ target: rangeSelect });
    await flush();
    assert.strictEqual(calls.filter(u => u === '/api/perf/ws-clients?range=7d').length, 1, 'switching back reuses only the matching range');
    sb.advanceNow(60001);
    await handlers.perfRefresh();
    await flush();
    assert.strictEqual(calls.filter(u => u === '/api/perf/ws-clients?range=7d').length, 2, 'refresh after 60s fetches fresh data');
  });

  await test('refresh: an unsupported URL range falls back safely to 24h', async () => {
    const { ctx, rangeSelect } = makeSandbox('#/perf?wsRange=%3Cimg%20src%3Dx%3E');
    const calls = [];
    ctx.fetch = url => {
      calls.push(url);
      return Promise.resolve({ ok: true, json: () => Promise.resolve(url === '/api/perf' ? PERF_FIXTURE() : null) });
    };
    ctx.pages.perf.init({ set innerHTML(v) {} });
    await flush();
    assert.strictEqual(rangeSelect.value, '24h');
    assert.ok(calls.includes('/api/perf/ws-clients'));
    assert.ok(!calls.some(u => u.includes('img')));
  });

  await test('refresh: a late range response cannot replace the newly selected range', async () => {
    const sb = makeSandbox();
    const { ctx, handlers, rangeSelect } = sb;
    let releaseSeven;
    const seven = new Promise(resolve => { releaseSeven = resolve; });
    ctx.fetch = url => {
      const range = url.includes('range=30d') ? '30d' : url.includes('range=7d') ? '7d' : '24h';
      const response = { ok: true, json: () => Promise.resolve({ range, intervalSec: 900, samples: mk(3, () => 2) }) };
      if (range === '7d') return seven.then(() => response);
      return Promise.resolve(url.startsWith('/api/perf/ws-clients') ? response : { ok: true, json: () => Promise.resolve(url === '/api/perf' ? PERF_FIXTURE() : null) });
    };
    ctx.pages.perf.init({ set innerHTML(v) {} });
    await flush();
    rangeSelect.value = '7d';
    const oldRefresh = handlers.range({ target: rangeSelect });
    await flush();
    rangeSelect.value = '30d';
    await handlers.range({ target: rangeSelect });
    await flush();
    assert.ok(sb.getHistoryHtml().includes('30 days'));
    releaseSeven();
    await oldRefresh;
    await flush();
    assert.ok(sb.getHistoryHtml().includes('30 days'), 'old request must not overwrite the current chart');
    assert.strictEqual(rangeSelect.value, '30d');
  });

  await test('refresh: failed history retries immediately, then caches the successful response', async () => {
    const sb = makeSandbox('#/perf?wsRange=30d');
    const { ctx, handlers } = sb;
    let calls = 0;
    ctx.fetch = url => {
      if (url.startsWith('/api/perf/ws-clients')) {
        calls++;
        return Promise.resolve({ ok: calls > 1, json: () => Promise.resolve({ range: '30d', intervalSec: 900, samples: mk(3, () => 3) }) });
      }
      return Promise.resolve({ ok: true, json: () => Promise.resolve(url === '/api/perf' ? PERF_FIXTURE() : null) });
    };
    ctx.pages.perf.init({ set innerHTML(v) {} });
    await flush();
    assert.ok(/unavailable/i.test(sb.getHistoryHtml()));
    await handlers.perfRefresh();
    await flush();
    assert.strictEqual(calls, 2, 'failed result must not spend a minute in the cache');
    assert.ok(sb.getHistoryHtml().includes('30 days'));
    await handlers.perfRefresh();
    await flush();
    assert.strictEqual(calls, 2, 'successful retry is cached');
  });

  await test('refresh: overlapping history requests coalesce but latest open-now health still wins', async () => {
    const sb = makeSandbox();
    const { ctx } = sb;
    let releaseHistory;
    const pending = new Promise(resolve => { releaseHistory = resolve; });
    let calls = 0, healthCalls = 0, tick;
    ctx.setInterval = fn => { tick = fn; return 1; };
    ctx.fetch = url => {
      if (url === '/api/perf/ws-clients') {
        calls++;
        return pending.then(() => ({ ok: true, json: () => Promise.resolve({ intervalSec: 60, samples: mk(3, () => 1) }) }));
      }
      const body = url === '/api/perf' ? PERF_FIXTURE()
        : url === '/api/health' ? { engine: 'go', websocket: { clients: ++healthCalls } } : null;
      return Promise.resolve({ ok: true, json: () => Promise.resolve(body) });
    };
    ctx.pages.perf.init({ set innerHTML(v) {} });
    await flush();
    tick();
    await flush();
    assert.strictEqual(calls, 1, 'a slow history endpoint must not cause overlapping full history downloads');
    assert.strictEqual(healthCalls, 2, 'health still refreshes independently');
    releaseHistory();
    await flush();
    assert.ok(/<b>2<\/b> open now/.test(sb.getHistoryHtml()), 'latest request wins rather than older health');
  });

  await test('refresh: a response from a destroyed page does not overwrite a new visit', async () => {
    const sb = makeSandbox();
    const { ctx } = sb;
    let releaseHistory;
    const pending = new Promise(resolve => { releaseHistory = resolve; });
    let calls = 0;
    ctx.fetch = url => {
      if (url === '/api/perf/ws-clients') {
        calls++;
        const clients = calls === 1 ? 91 : 3;
        const response = { ok: true, json: () => Promise.resolve({ intervalSec: 60, samples: mk(3, () => clients) }) };
        return calls === 1 ? pending.then(() => response) : Promise.resolve(response);
      }
      return Promise.resolve({ ok: true, json: () => Promise.resolve(url === '/api/perf' ? PERF_FIXTURE() : null) });
    };
    ctx.pages.perf.init({ set innerHTML(v) {} });
    await flush();
    ctx.pages.perf.destroy();
    ctx.pages.perf.init({ set innerHTML(v) {} });
    await flush();
    assert.ok(/<b>3<\/b> open now/.test(sb.getHistoryHtml()));
    releaseHistory();
    await flush();
    assert.ok(/<b>3<\/b> open now/.test(sb.getHistoryHtml()), 'old page must not render into the new visit');
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

  await test('renderWSClientHistory: solid (clients) series uses an AA-contrast token, not --accent (#433 F5)', () => {
    const { ctx } = makeSandbox();
    const html = ctx.window.renderWSClientHistory({ intervalSec: 60, samples: mk(5, i => i) });
    assert.ok(html.includes('stroke="var(--link-color)"'), '--accent is 2.75:1 on white, below WCAG 1.4.11\'s 3:1');
    assert.ok(!/stroke="var\(--accent\)"/.test(html), 'must not regress back to --accent');
  });

  await test('renderWSClientHistory: "open now" uses currentClients, not the last minute sample (#433 F1)', () => {
    const { ctx } = makeSandbox();
    // Last sample says 0 clients; a client connected in the 59s since then.
    const hist = { intervalSec: 60, samples: mk(5, () => 0) };
    const html = ctx.window.renderWSClientHistory(hist, 7);
    assert.ok(/<b>7<\/b> open now/.test(html), html.slice(0, 300));
  });

  await test('renderWSClientHistory: falls back to the last sample when currentClients is missing/invalid (#433 F1)', () => {
    const { ctx } = makeSandbox();
    const hist = { intervalSec: 60, samples: mk(5, i => i) }; // latest = 4
    for (const bad of [undefined, null, NaN, -1, 'nope', Infinity]) {
      const html = ctx.window.renderWSClientHistory(hist, bad);
      assert.ok(/<b>4<\/b> open now/.test(html), `currentClients=${bad}: ` + html.slice(0, 300));
    }
  });

  await test('buildWSClientChart: a null entry and out-of-range values do not throw (#433 F4)', () => {
    const { ctx } = makeSandbox();
    const weird = [
      { t: new Date(T0).toISOString(), clients: 3, peak: 4 },
      null,
      { t: new Date(T0 + 60000).toISOString(), clients: 1e999, peak: -5 }, // Infinity, negative
      { t: new Date(T0 + 120000).toISOString(), clients: 2, peak: 2 },
    ];
    let c;
    assert.doesNotThrow(() => { c = ctx.window.buildWSClientChart(weird, 400, 100); });
    assert.ok(isFinite(c.maxY) && c.maxY > 0, `maxY = ${c.maxY}`);
    for (const p of c.clientPoints.concat(c.peakPoints)) {
      assert.ok(isFinite(p.x) && isFinite(p.y), `point ${JSON.stringify(p)} not finite`);
    }
  });

  await test('renderWSClientHistory: a null entry in samples does not throw and does not crash the page (#433 F4)', () => {
    const { ctx } = makeSandbox();
    const samples = mk(5, i => i);
    samples[2] = null;
    assert.doesNotThrow(() => ctx.window.renderWSClientHistory({ intervalSec: 60, samples }));
  });

  await test('refresh: fetches /api/perf/ws-clients once and shows the card', async () => {
    const { ctx, getHistoryHtml } = makeSandbox();
    const calls = [];
    const perf = PERF_FIXTURE();
    ctx.fetch = (url) => {
      calls.push(url);
      if (url === '/api/health') return Promise.reject(new Error('no health'));
      const body = url === '/api/perf' ? perf
        : url === '/api/perf/ws-clients' ? { intervalSec: 60, samples: mk(10, i => i) }
        : {};
      return Promise.resolve({ ok: true, json: () => Promise.resolve(body) });
    };
    const app = { set innerHTML(v) {} };
    ctx.pages.perf.init(app);
    await flush();
    assert.strictEqual(calls.filter(u => u === '/api/perf/ws-clients').length, 1);
    assert.ok(getHistoryHtml().includes('<svg class="ws-history'), getHistoryHtml().slice(0, 200));
  });

  await test('refresh: a failing ws-clients endpoint does not break the page', async () => {
    const { ctx } = makeSandbox();
    const perf = PERF_FIXTURE();
    ctx.fetch = (url) => (url === '/api/perf/ws-clients' || url === '/api/health')
      ? Promise.reject(new Error('boom'))
      : Promise.resolve({ ok: true, json: () => Promise.resolve(url === '/api/perf' ? perf : {}) });
    ctx.document.getElementById = (id) => id === 'perfContent' ? { set innerHTML(v) { ctx.__html = v; } } : null;
    ctx.pages.perf.init({ set innerHTML(v) {} });
    await flush();
    assert.ok(String(ctx.__html).includes('Total Requests'), 'rest of the page still renders');
  });

  await test('refresh: a 404/500 from ws-clients hides the card instead of hanging on "Collecting data…" (#433 F2)', async () => {
    const { ctx, getHistoryHtml } = makeSandbox();
    const perf = PERF_FIXTURE();
    ctx.fetch = (url) => {
      if (url === '/api/health') return Promise.reject(new Error('no health'));
      if (url === '/api/perf/ws-clients') {
        // #/api/* 404s still return a JSON body — resolving r.json() without
        // checking r.ok is exactly the bug this pins.
        return Promise.resolve({ ok: false, status: 404, json: () => Promise.resolve({ error: 'not found' }) });
      }
      return Promise.resolve({ ok: true, json: () => Promise.resolve(url === '/api/perf' ? perf : {}) });
    };
    ctx.document.getElementById = (id) => id === 'perfContent' ? { set innerHTML(v) { ctx.__html = v; } } : null;
    ctx.pages.perf.init({ set innerHTML(v) {} });
    await flush();
    const html = String(ctx.__html);
    assert.ok(!getHistoryHtml().includes('<svg class="ws-history'), 'no chart expected');
    assert.ok(!/collecting/i.test(getHistoryHtml()), '404 must not look like "still collecting"');
    assert.ok(html.includes('Total Requests'), 'rest of the page still renders');
  });

  await test('refresh: the ws-clients ring is not re-fetched on every 5s tick (#433 F6)', async () => {
    const { ctx, getHistoryHtml } = makeSandbox();
    const perf = PERF_FIXTURE();
    let wsCalls = 0;
    const handlers = {};
    const getElementById = ctx.document.getElementById;
    ctx.document.getElementById = (id) => {
      if (id === 'perfContent') return { set innerHTML(v) { ctx.__html = v; } };
      if (id === 'perfRefresh' || id === 'perfReset') {
        return { addEventListener: (ev, fn) => { handlers[id] = fn; } };
      }
      return getElementById(id);
    };
    ctx.fetch = (url) => {
      if (url === '/api/health') return Promise.reject(new Error('no health'));
      if (url === '/api/perf/ws-clients') { wsCalls++; return Promise.resolve({ ok: true, json: () => Promise.resolve({ intervalSec: 60, samples: mk(10, i => i) }) }); }
      return Promise.resolve({ ok: true, json: () => Promise.resolve(url === '/api/perf' ? perf : {}) });
    };
    ctx.pages.perf.init({ set innerHTML(v) {} });
    await flush();
    assert.strictEqual(wsCalls, 1, 'first refresh fetches the ring');
    for (let i = 0; i < 10; i++) { await handlers.perfRefresh(); await flush(); }
    assert.strictEqual(wsCalls, 1, 'ten more refreshes within the cache window must reuse it, not re-fetch 72.9 KB each time');
    assert.ok(getHistoryHtml().includes('<svg class="ws-history'), 'chart still renders from the cached ring');
  });

  await test('refresh: a round slower than the 5s tick must still render once it completes, not get stuck on "Loading…" forever (PR#461 F1)', async () => {
    const sb = makeSandbox();
    const { ctx } = sb;
    let tick;
    ctx.setInterval = fn => { tick = fn; return 1; };
    const perf = PERF_FIXTURE();
    const releasers = [];
    ctx.fetch = (url) => {
      if (url === '/api/perf') {
        return new Promise(resolve => { releasers.push(() => resolve({ ok: true, json: () => Promise.resolve(perf) })); });
      }
      if (url === '/api/health') return Promise.reject(new Error('no health'));
      if (url === '/api/perf/ws-clients') return Promise.resolve({ ok: true, json: () => Promise.resolve({ intervalSec: 60, samples: mk(3, () => 1) }) });
      return Promise.resolve({ ok: true, json: () => Promise.resolve({}) });
    };
    ctx.pages.perf.init({ set innerHTML(v) {} });
    await flush();
    // Two more 5s ticks elapse while the very first /api/perf round (a loaded
    // server) is still in flight — exactly the overload scenario from F1.
    tick();
    await flush();
    tick();
    await flush();
    assert.strictEqual(releasers.length, 3, 'three refreshes now overlapping, none has resolved yet');
    // The oldest round finally completes. It was overtaken by two newer
    // ticks, but nothing has rendered yet, so it must still paint the page
    // instead of being silently dropped.
    releasers[0]();
    await flush();
    assert.ok(sb.getHtml().includes('Total Requests'), 'an overtaken-but-first-to-finish refresh must still render, not stay on Loading…');
  });

  await test('refresh: re-entering the page (init) forces a fresh ws-clients fetch, not a stale cache', async () => {
    const { ctx } = makeSandbox();
    const perf = PERF_FIXTURE();
    let wsCalls = 0;
    ctx.fetch = (url) => {
      if (url === '/api/health') return Promise.reject(new Error('no health'));
      if (url === '/api/perf/ws-clients') { wsCalls++; return Promise.resolve({ ok: true, json: () => Promise.resolve({ intervalSec: 60, samples: mk(10, i => i) }) }); }
      return Promise.resolve({ ok: true, json: () => Promise.resolve(url === '/api/perf' ? perf : {}) });
    };
    ctx.document.getElementById = (id) => id === 'perfContent' ? { set innerHTML(v) { ctx.__html = v; } } : null;
    ctx.pages.perf.init({ set innerHTML(v) {} });
    await flush();
    ctx.pages.perf.destroy();
    ctx.pages.perf.init({ set innerHTML(v) {} });
    await flush();
    assert.strictEqual(wsCalls, 2, 'leaving and re-entering the page should not reuse a cache from the previous visit');
  });

  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
