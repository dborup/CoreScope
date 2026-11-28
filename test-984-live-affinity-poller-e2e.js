/**
 * E2E: the Live neighbor-graph (hop affinity) poller (#984).
 *
 * A stalled connection used to collect one /api/analytics/neighbor-graph
 * request per 60 s tick and release them all at once. On the real page, with
 * Playwright's clock, the socket replaced by page.routeWebSocket, and every
 * neighbor-graph request intercepted (held, or answered with the server's
 * real body), counted and tracked while open:
 *
 *   1. A 375x560 touch phone from a cold start: one request; a connection
 *      that stalls for 5 ticks makes one request per tick, each aborted
 *      before the next; while one is held, extra ticks and a re-armed
 *      poller add none; the timeout aborts it, the next tick asks once more
 *      and its answer reaches HopResolver.setAffinity. At most one request
 *      is ever open.
 *   2. A hidden tab polls nothing for 5 min; becoming visible again with
 *      data older than the interval refreshes exactly once.
 *   3. #/live?kiosk=1 at 1920x1080: the affinity data still refreshes on
 *      the 60 s interval.
 *   4. Leaving Live aborts the open request and nothing polls afterwards;
 *      an in-place remount polls on one interval, not two.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-984-live-affinity-poller-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
if (!['localhost', '127.0.0.1', '[::1]'].includes(new URL(BASE).hostname)) throw new Error('local server only');

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + (e && e.stack || e)); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

const GRAPH = /\/api\/analytics\/neighbor-graph(\?|$)/;

async function open(browser, o) {
  o = o || {};
  const ctx = await browser.newContext(Object.assign({
    viewport: { width: 1280, height: 720 }, reducedMotion: 'reduce',
  }, o.context || {}));
  await ctx.routeWebSocket(/.*/, () => {});
  const h = { ctx, errors: [], requests: 0, open: 0, maxOpen: 0, held: [], aborted: 0, hold: !!o.hold };
  // Held requests stay open until the page aborts them or the test answers.
  await ctx.route(GRAPH, async (route) => {
    h.requests++;
    if (h.hold) { h.held.push(route); return; }
    try { await route.fulfill({ response: await route.fetch() }); } catch (_) { /* page gone */ }
  });
  const page = await ctx.newPage();
  h.page = page;
  page.on('request', (r) => { if (GRAPH.test(r.url())) { h.open++; h.maxOpen = Math.max(h.maxOpen, h.open); } });
  page.on('requestfinished', (r) => { if (GRAPH.test(r.url())) h.open--; });
  page.on('requestfailed', (r) => { if (GRAPH.test(r.url())) { h.open--; h.aborted++; } });
  page.on('pageerror', (e) => h.errors.push(String(e.message)));
  // A controllable document.hidden; the real visibilitychange listener runs.
  await ctx.addInitScript(() => {
    window.__hidden984 = false;
    Object.defineProperty(document, 'hidden', { configurable: true, get: () => window.__hidden984 });
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => (window.__hidden984 ? 'hidden' : 'visible') });
  });
  await page.clock.install();
  h.ready = () => page.waitForFunction(() => window._liveGetMap && window._liveGetMap() &&
    window.HopResolver && !!document.querySelector('.live-page'), null, { timeout: 20000 });
  // Counts the graphs Live hands to the hop resolver (wrapped once per page).
  h.spy = () => page.evaluate(() => {
    if (window.__affinity984) return;
    window.__affinity984 = 0;
    const orig = window.HopResolver.setAffinity;
    window.HopResolver.setAffinity = function (g) { window.__affinity984++; return orig.apply(this, arguments); };
  });
  h.affinity = () => page.evaluate(() => window.__affinity984);
  h.setHidden = (hidden) => page.evaluate((x) => { window.__hidden984 = x; document.dispatchEvent(new Event('visibilitychange')); }, hidden);
  h.run = (ms) => page.clock.runFor(ms);
  // Network events arrive after the page's own work; let them land.
  h.settle = async () => { for (let i = 0; i < 10; i++) await page.waitForTimeout(50); };
  h.waitRequests = async (n) => {
    for (let i = 0; i < 100 && h.requests < n; i++) await page.waitForTimeout(50);
    assert(h.requests >= n, 'expected ' + n + ' neighbor-graph request(s), saw ' + h.requests);
  };
  h.answerHeld = async () => {
    const routes = h.held.splice(0);
    for (const r of routes) { try { await r.fulfill({ response: await r.fetch() }); } catch (_) { /* aborted */ } }
  };
  await page.goto(BASE + '/' + (o.hash || '#/live'));
  await h.ready();
  await h.spy();
  return h;
}

(async () => {
  const browser = await chromium.launch({ args: ['--no-sandbox'] });
  console.log('\n=== #984 Live neighbor-graph poller E2E ===');
  try {
    await step('1 phone 375x560 cold start: a stalled request blocks further ticks, the timeout frees the slot, the next tick refreshes', async () => {
      const h = await open(browser, { hold: true, context: { viewport: { width: 375, height: 560 }, hasTouch: true, isMobile: true } });
      await h.waitRequests(1);
      await h.settle();
      assert(h.requests === 1, 'cold start: one request, saw ' + h.requests);
      // The incident: the connection stalls for 5 ticks. One request per
      // tick, each aborted before the next, never two open at once.
      await h.run(5 * 60000);
      await h.waitRequests(6);
      await h.settle();
      assert(h.maxOpen === 1, 'a stalled connection holds at most one open request, saw ' + h.maxOpen);
      assert(h.requests === 6 && h.aborted === 5, 'one request per tick, each aborted before the next: requests ' +
        h.requests + ', aborted ' + h.aborted);
      // Ticks and a re-armed poller (loadNodes() again) while one is held.
      await h.page.evaluate(() => { const p = window._liveAffinityPoller(); p.tick(); p.tick(); window._liveStartAffinityRefresh(); });
      await h.run(10000);
      await h.settle();
      assert(h.requests === 6, 'no further request while one is open, saw ' + h.requests);
      const timeoutMs = await h.page.evaluate(() => window._liveAffinityPoller().timeoutMs);
      assert(timeoutMs > 0 && timeoutMs <= 30000, 'timeout ' + timeoutMs);
      await h.run(timeoutMs);
      await h.settle();
      assert(h.aborted === 6, 'the timeout aborts the held request, aborted ' + h.aborted);
      h.held.splice(0);
      h.hold = false;
      await h.run(60000);
      await h.waitRequests(7);
      await h.settle();
      assert(h.requests === 7, 'the next tick asks once, saw ' + h.requests);
      assert(await h.affinity() === 1, 'its answer reaches HopResolver.setAffinity, got ' + await h.affinity());
      assert(h.maxOpen === 1, 'at most one open request, saw ' + h.maxOpen);
      assert(!h.errors.length, h.errors.join('; '));
      await h.ctx.close();
    });

    await step('2 a hidden tab polls nothing; visible again with stale data refreshes exactly once', async () => {
      const h = await open(browser);
      await h.waitRequests(1);
      await h.settle();
      const base = h.requests;
      const seen = await h.affinity();
      await h.setHidden(true);
      await h.run(5 * 60000);
      await h.settle();
      assert(h.requests === base, 'no request while hidden, saw ' + (h.requests - base));
      await h.setHidden(false);
      await h.setHidden(false);
      await h.waitRequests(base + 1);
      await h.settle();
      assert(h.requests === base + 1, 'one refresh on return, saw ' + (h.requests - base));
      assert(await h.affinity() === seen + 1, 'the refresh reaches HopResolver.setAffinity');
      // Fresh now: hiding and showing again does not ask.
      await h.setHidden(true);
      await h.setHidden(false);
      await h.settle();
      assert(h.requests === base + 1, 'fresh data: no refresh, saw ' + (h.requests - base));
      assert(h.maxOpen === 1, 'at most one open request, saw ' + h.maxOpen);
      assert(!h.errors.length, h.errors.join('; '));
      await h.ctx.close();
    });

    await step('3 kiosk 1920x1080: the affinity data still refreshes every 60 s', async () => {
      const h = await open(browser, { hash: '#/live?kiosk=1', context: { viewport: { width: 1920, height: 1080 } } });
      await h.page.waitForFunction(() => document.body.classList.contains('kiosk'), null, { timeout: 20000 });
      await h.waitRequests(1);
      await h.settle();
      const base = h.requests;
      const seen = await h.affinity();
      await h.run(60000);
      await h.waitRequests(base + 1);
      await h.run(60000);
      await h.waitRequests(base + 2);
      await h.settle();
      assert(h.requests === base + 2, 'two ticks, two requests, saw ' + (h.requests - base));
      assert(await h.affinity() === seen + 2, 'both answers reach HopResolver.setAffinity');
      assert(h.maxOpen === 1, 'at most one open request, saw ' + h.maxOpen);
      assert(!h.errors.length, h.errors.join('; '));
      await h.ctx.close();
    });

    await step('4 leaving Live aborts the open request and stops polling; an in-place remount polls on one interval', async () => {
      const h = await open(browser, { hold: true });
      await h.waitRequests(1);
      await h.page.evaluate(() => { location.hash = '#/packets'; });
      await h.page.waitForFunction(() => !document.querySelector('.live-page'), null, { timeout: 20000 });
      await h.settle();
      assert(h.aborted === 1, 'leaving Live aborts the open request, aborted ' + h.aborted);
      await h.run(5 * 60000);
      await h.settle();
      assert(h.requests === 1, 'nothing polls after leaving, saw ' + h.requests);
      assert(await h.page.evaluate(() => window._liveAffinityPoller()) === null, 'the poller is dropped');
      h.held.splice(0);
      h.hold = false;
      await h.page.evaluate(() => { location.hash = '#/live'; });
      await h.ready();
      await h.waitRequests(2);
      await h.page.evaluate(() => { window.__oldMap984 = window._liveGetMap(); location.hash = '#/live?remount=1'; });
      await h.page.waitForFunction(() => window._liveGetMap && window._liveGetMap() && window._liveGetMap() !== window.__oldMap984 &&
        !!document.querySelector('.live-page'), null, { timeout: 20000 });
      await h.page.evaluate(() => { window.__oldMap984 = null; });
      await h.waitRequests(3);
      await h.settle();
      const base = h.requests;
      await h.run(60000);
      await h.waitRequests(base + 1);
      await h.settle();
      assert(h.requests === base + 1, 'one interval after the remount: one request per tick, saw ' + (h.requests - base));
      assert(h.maxOpen === 1, 'at most one open request, saw ' + h.maxOpen);
      assert(!h.errors.length, h.errors.join('; '));
      await h.ctx.close();
    });
  } finally {
    await browser.close();
  }
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
