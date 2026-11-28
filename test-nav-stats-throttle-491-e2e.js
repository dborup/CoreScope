#!/usr/bin/env node
'use strict';

// Real app + Chromium + network routing, with accelerated browser time and
// browser-driven SIMULATED visibility. Headless Chromium keeps both tabs
// visible even after bringToFront(), so this is not an OS-background test.
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const BASE = process.env.BASE_URL || 'http://127.0.0.1:13991';
assert.ok(['localhost', '127.0.0.1', '[::1]'].includes(new URL(BASE).hostname), 'local server only');
const measureOnly = process.argv.includes('--measure') || process.env.NAV_STATS_MEASURE === '1';

// Waits for the app's in-flight api() requests. The page does the waiting and
// the test reads a flag back with plain evaluates: an evaluate that awaits a
// promise can fail as "Promise was collected" in Chromium when a GC lands
// after that promise settles but before the result is sent (Playwright
// reports it as "Execution context was destroyed ... navigation"), and Live's
// node load allocates enough to hit that window (PR #950). A real reload or
// navigation still fails here: the document token from open() is gone.
async function settle(page) {
  const replaced = 'the page document was replaced (a reload or navigation)';
  const ticket = await page.evaluate(() => {
    if (!window.__navTestDoc) return 0;
    const n = window.__navTestSettles = (window.__navTestSettles || 0) + 1;
    Promise.allSettled([..._inflight.values()]).then(() => { window.__navTestSettled = n; });
    return n;
  });
  assert.ok(ticket, replaced);
  const deadline = Date.now() + 15000;
  for (;;) {
    const settled = await page.evaluate(n => window.__navTestDoc ? window.__navTestSettled >= n : null, ticket);
    assert.notEqual(settled, null, replaced);
    if (settled) return;
    assert.ok(Date.now() < deadline, 'in-flight api() requests did not settle in 15 s');
    await new Promise(resolve => setTimeout(resolve, 5));
  }
}
async function open(browser, { hidden = false, mobile = false } = {}) {
  const context = await browser.newContext({ viewport: mobile ? { width: 390, height: 844 } : { width: 1440, height: 900 } });
  const requests = [], sockets = [], errors = [];
  await context.addInitScript(initialHidden => {
    let hidden = initialHidden;
    Object.defineProperty(document, 'hidden', { configurable: true, get: () => hidden });
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => hidden ? 'hidden' : 'visible' });
    window.__navTestVisibility = value => {
      hidden = value;
      document.dispatchEvent(new Event('visibilitychange'));
    };
  }, hidden);
  await context.route('**/api/config/cache', route => route.fulfill({ json: {} }));
  await context.route('**/api/stats', async route => {
    requests.push(route.request().url());
    await route.fulfill({ json: { totalPackets: requests.length * 100, totalNodes: 2, totalObservers: 3 } });
  });
  await context.routeWebSocket(/.*/, socket => { sockets.push(socket); });
  const page = await context.newPage();
  page.setDefaultTimeout(15000);
  page.on('pageerror', error => errors.push(error.message));
  const loads = [];
  page.on('load', () => loads.push(page.url()));
  const date = new Date('2026-10-16T12:00:00Z');
  await page.clock.install({ time: date });
  await page.clock.pauseAt(new Date(date.getTime() + 1000));
  await page.goto(BASE + '/#/tools', { waitUntil: 'networkidle' });
  await page.evaluate(() => { window.__navTestDoc = true; });
  await settle(page);
  assert.ok(sockets.length, 'app shared WebSocket must connect');
  return {
    context, page, requests, errors, loads,
    async ws() {
      await page.evaluate(() => {
        window.__navTestFrame = new Promise(resolve => {
          const once = () => { offWS(once); resolve(); };
          onWS(once);
        });
      });
      sockets[sockets.length - 1].send(JSON.stringify({ type: 'new_data' }));
      await page.evaluate(() => window.__navTestFrame);
    },
    async advance(ms) { await page.clock.runFor(ms); await settle(page); },
    async visibility(value) { await page.evaluate(value => window.__navTestVisibility(value), value); await settle(page); },
  };
}
async function traffic(h, seconds) {
  for (let i = 0; i < seconds; i++) { await h.ws(); await h.advance(1000); }
}
let passed = 0;
async function check(name, fn) { await fn(); passed++; console.log((measureOnly ? '  measured: ' : '  ✓ ') + name); }
(async () => {
  // --expose-gc: the settle() regression check below forces the GC that the
  // Live mount only sometimes triggers.
  const browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined, args: ['--js-flags=--expose-gc'] });
  try {
    const visible = await open(browser);
    try {
      await check('visible sustained WS traffic: count actual fetches over inclusive simulated 0..60 seconds', async () => {
        await traffic(visible, 60);
        console.log('  measurement visible/60s inclusive: ' + visible.requests.length + ' actual /api/stats fetches');
        if (!measureOnly) assert.equal(visible.requests.length, 5);
      });
      await check('browser-driven hidden/foreground transition: actual network counts and refreshed counters', async () => {
        const start = visible.requests.length;
        await visible.visibility(true); await traffic(visible, 60);
        const hidden = visible.requests.length - start;
        await visible.visibility(false);
        const resumed = visible.requests.length - start - hidden;
        console.log('  measurement hidden/60s: ' + hidden + ', resume: ' + resumed + ' actual fetches');
        if (!measureOnly) {
          assert.equal(hidden, 0); assert.equal(resumed, 1);
          await visible.page.locator('#navStats').filter({ hasText: String(visible.requests.length * 100) + ' pkts' }).waitFor();
          for (let i = 0; i < 5; i++) await visible.visibility(false);
          assert.equal(visible.requests.length, start + 1);
        }
      });
      if (!measureOnly) {
        await check('SPA Live entry/exit leaves one nav poller and other shared WS listeners active', async () => {
          const before = await visible.page.evaluate(() => wsListeners.length);
          await visible.page.evaluate(() => { location.hash = '#/live'; });
          await visible.page.locator('#liveControls').waitFor();
          await settle(visible.page);
          // #514: Live renders #liveControls first and only registers its shared-WS
          // listener after `await loadNodes()` (live.js init), so the count is
          // sampled when that registration is observable, not when the DOM is.
          // If Live never subscribes this wait times out and the assertion fails.
          const subscribed = await visible.page
            .waitForFunction(n => wsListeners.length > n, before)
            .then(() => true, () => false);
          assert.ok(subscribed && await visible.page.evaluate(() => wsListeners.length) > before, 'Live must retain its independent subscription');
          await visible.page.evaluate(() => { location.hash = '#/tools'; });
          await visible.page.locator('.tools-landing').waitFor();
          assert.equal(await visible.page.evaluate(() => wsListeners.length), before);
          const start = visible.requests.length;
          await traffic(visible, 15); assert.equal(visible.requests.length, start + 1);
        });
        await check('settle() survives a GC right after the requests it waits for settle (PR #950)', async () => {
          // A request that settles and is followed by GCs in the next
          // microtasks, as Live's node load does. An evaluate that awaited
          // it failed here in every local run ("Execution context was destroyed").
          await visible.page.evaluate(() => {
            const request = new Promise(resolve => { window.__navTestRelease = resolve; });
            _inflight.set('/__nav-test-gc', request);
            request.finally(() => _inflight.delete('/__nav-test-gc'));
            let next = request;
            for (let i = 0; i < 12; i++) next = next.then(() => gc());
          });
          const settling = settle(visible.page);
          await visible.page.evaluate(() => window.__navTestRelease());
          await settling;
          assert.equal(await visible.page.evaluate(() => _inflight.has('/__nav-test-gc')), false);
        });
        await check('no browser exceptions', async () => { assert.deepEqual(visible.errors, []); });
        await check('no reload or navigation: one document load for the whole run', async () => {
          assert.equal(visible.loads.length, 1, 'document loads: ' + JSON.stringify(visible.loads));
        });
      }
    } finally { await visible.context.close(); }
    const initiallyHidden = await open(browser, { hidden: true, mobile: true });
    try {
      await check('mobile initially-hidden simulation and the first foreground transition', async () => {
        await traffic(initiallyHidden, 30);
        console.log('  measurement initially hidden/30s: ' + initiallyHidden.requests.length + ' actual fetches');
        if (!measureOnly) {
          assert.equal(initiallyHidden.requests.length, 0);
          await initiallyHidden.visibility(false);
          assert.equal(initiallyHidden.requests.length, 1);
          // The mobile design hides this desktop nav element; its DOM
          // counters must still refresh, not become a visible mobile control.
          assert.match(await initiallyHidden.page.locator('#navStats').textContent(), /100 pkts/);
          assert.deepEqual(initiallyHidden.errors, []);
          assert.equal(initiallyHidden.loads.length, 1, 'no reload or navigation');
        }
      });
    } finally { await initiallyHidden.context.close(); }
    console.log(measureOnly ? `\n${passed} browser scenarios measured (no regression assertions)` : `\n${passed} browser checks passed (regression assertions enabled)`);
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
